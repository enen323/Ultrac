package main

// print.go — the P5 print/json mode: pi's modes/print-mode.ts, wired to the
// Go harness (docs/pi-core-harness-plan.md §3.6).
//
// # The shape
//
// One invocation: resolve settings → resolve the model → open (or continue) a
// session → build the tools and the sectioned system prompt → run the agent →
// print the answer (text mode) or the event stream (json mode) → exit.
//
// # Two deliberate differences from the old T1 path
//
//   - No os.Setenv. The old runAgent applied the settings env block to the
//     process environment because the old provider client read it from there.
//     The new stack takes the credential through agent.AgentOptions.GetAPIKey
//     and the model through resolveModel, so nothing is written to the
//     process environment — a print run cannot leak settings into a
//     subprocess it spawns.
//   - The system prompt is never a plain message. pi constructs its agent
//     with systemPrompt "" and diffs the sectioned prompt into a system patch
//     message per run (sdk.ts:413 + agent-session.ts:1742-1754), so the
//     session file records the prompt as patchable sections. Mirroring that
//     is what makes a resumed session diff cleanly instead of re-declaring
//     the whole prompt.
//
// # Exit codes
//
//	0    the run completed (pi: print-mode.ts returns exitCode)
//	1    the assistant message ended in error/aborted, or setup failed
//	130  interrupted (SIGINT)
//	143  terminated (SIGTERM)
//	129  hangup (SIGHUP)
//
// A signal cancels the context, which the loop turns into an aborted stream
// and the bash tool turns into a killed process tree — the cleanup is the
// cancellation, not a separate teardown path.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/config"
	"ultrac/internal/logging"
	"ultrac/internal/prompt"
	"ultrac/internal/session"
	"ultrac/internal/tool"
	"ultrac/internal/transport"
	"ultrac/internal/version"
)

// printSession is one print-mode run. It exists as a struct rather than a
// function so the tests can inject a stream function and a working directory
// without touching the process environment.
type printSession struct {
	flags  *agentFlags
	stdout io.Writer
	stderr io.Writer
	cwd    string

	// streamFn overrides the provider registry. Nil in production; tests
	// inject a fake so no request leaves the process (the P2 gate: the loop
	// is verified against preset scripts, not the real API).
	streamFn agent.StreamFn
}

// runPrint is the entry point the CLI calls.
func runPrint(f *agentFlags, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "ultrac: %v\n", err)
		return 1
	}
	if f.CWD != "" {
		// --cwd is not decoration: it picks the directory the tools resolve
		// paths against, the session the transcript belongs to, and the
		// project settings layer that is read.
		abs, aerr := filepath.Abs(f.CWD)
		if aerr != nil {
			fmt.Fprintf(stderr, "ultrac: --cwd: %v\n", aerr)
			return 2
		}
		info, serr := os.Stat(abs)
		if serr != nil || !info.IsDir() {
			fmt.Fprintf(stderr, "ultrac: --cwd %s is not a directory\n", f.CWD)
			return 2
		}
		cwd = abs
	}
	return (&printSession{flags: f, stdout: stdout, stderr: stderr, cwd: cwd}).run()
}

func (ps *printSession) run() int {
	f := ps.flags

	if f.Debug {
		logging.SetDefault(logging.New(logging.Options{
			Level:   logging.LevelDebug,
			Version: version.Get().Version,
		}))
	}

	ctx, stopWatch, firedSignal := printSignalContext()
	defer stopWatch()

	// -- 1. Settings ------------------------------------------------------
	//
	// Layers are read but never applied to the process environment (see the
	// file comment). A load error is a warning, not a failure: a broken
	// project settings file must not take down a run that the user settings
	// alone can serve.
	r := config.NewFileResolver()
	layers, loadErr := r.Layers(ctx, ps.cwd)
	if loadErr != nil {
		fmt.Fprintf(ps.stderr, "warning: %v\n", loadErr)
	}
	effective := config.EffectiveEnv(layers)

	// -- 2. Session ---------------------------------------------------------
	//
	// Opened before the model is resolved because a resumed session can
	// supply the model when neither the flag nor the settings name one.
	mgr, seed, err := ps.openSession()
	if err != nil {
		fmt.Fprintf(ps.stderr, "ultrac: %v\n", err)
		return 1
	}

	// -- 3. Model and credential ------------------------------------------
	flagModel := f.Model
	if flagModel == "" && effective[config.EnvModel] == "" && seed.Model != nil {
		// Resume fallback: the session's last recorded model, expressed in
		// the flag's provider/id syntax so resolveModel treats it exactly
		// like a user-typed value.
		flagModel = seed.Model.Provider + "/" + seed.Model.ModelID
	}
	model, err := resolveModel(flagModel, f.API, effective)
	if err != nil {
		fmt.Fprintf(ps.stderr, "ultrac: %v\n", err)
		return 2
	}
	ps.recordSessionMeta(mgr, model)
	apiKey := apiKeyFor(effective)
	if apiKey == "" {
		fmt.Fprintln(ps.stderr, "ultrac: no credential is configured.")
		fmt.Fprintln(ps.stderr, "        Put ANTHROPIC_AUTH_TOKEN (or ANTHROPIC_API_KEY) in the \"env\" block of")
		fmt.Fprintf(ps.stderr, "        %s.\n", filepath.Join(config.ConfigHome(), "settings.json"))
		fmt.Fprintln(ps.stderr, "        Run `ultrac config` to see what the layers currently resolve to.")
		return 1
	}

	// -- 4. Tools and the sectioned prompt ----------------------------------
	tools := tool.All(ps.cwd)
	toolNames := make([]string, len(tools))
	for i, t := range tools {
		toolNames[i] = t.Name()
	}
	_, sections, err := prompt.BuildSystemPromptState(prompt.Options{
		CWD:           ps.cwd,
		SelectedTools: toolNames,
		ContextFiles:  prompt.LoadContextFiles(ps.cwd),
	})
	if err != nil {
		fmt.Fprintf(ps.stderr, "ultrac: system prompt: %v\n", err)
		return 1
	}

	// The prompt diff: replay the transcript's current sections against the
	// freshly built ones and carry the delta as a system patch message
	// (pi: agent-session.ts:1742-1754, _preparePromptAndToolLoadout).
	initial := &agent.AgentInitialState{
		// Empty on purpose — the prompt lives in the sections patch, not in
		// a plain leading message (pi: sdk.ts:413).
		SystemPrompt: "",
		Model:        &model,
		Tools:        tools,
	}
	if ps.flags.Continue {
		initial.Messages = seed.Messages
	}
	patch := prompt.DiffSystemPromptSections(currentSections(initial.Messages), sections)

	// -- 5. Agent -------------------------------------------------------------
	a := agent.NewAgent(agent.AgentOptions{
		InitialState: initial,
		GetAPIKey:    func(string) string { return apiKey },
		SessionID:    mgr.SessionID(),
		StreamFn:     ps.streamFn,
	})

	// -- 6. Output surfaces ----------------------------------------------------
	jsonMode := f.Mode == "json"
	var writer *transport.PiJSONWriter
	if jsonMode {
		writer = transport.NewPiJSONWriter(ps.stdout)
		// The header opens the stream, before any event (pi:
		// print-mode.ts:122-127).
		h := mgr.Header()
		if err := writer.WriteHeader(&h); err != nil {
			fmt.Fprintf(ps.stderr, "ultrac: %v\n", err)
			return 1
		}
	}

	var lastAssistant *ai.Message
	unsubscribe := a.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if e, ok := ev.(agent.EvMessageEnd); ok {
			// Persist every completed message, system patches included —
			// the same rule pi's agent-session applies (agent-session.ts:
			// 1147-1166).
			if !f.NoTranscript && persistable(e.Message.Role) {
				if _, err := mgr.AppendMessage(e.Message); err != nil && f.Debug {
					fmt.Fprintf(ps.stderr, "[debug] session append: %v\n", err)
				}
			}
			if e.Message.Role == ai.RoleAssistant {
				m := e.Message
				lastAssistant = &m
			}
		}
		if writer != nil {
			return writer.WriteEvent(ev)
		}
		return nil
	})
	defer unsubscribe()

	// -- 7. Run -----------------------------------------------------------------
	prompts := []ai.Message{userMessage(f.prompt())}
	if patch != nil {
		prompts = append([]ai.Message{systemPatchMessage(patch)}, prompts...)
	}
	// Prompt blocks until the run settles (runWithLifecycle calls the loop
	// synchronously); run-level failures arrive as events and
	// State().ErrorMessage, not as a return value (pi: agent.ts:523-530).
	_ = a.Prompt(ctx, prompts...)
	_ = a.WaitForIdle(ctx)
	unsubscribe()

	// -- 8. Output and exit code --------------------------------------------------
	if sig := firedSignal(); sig != "" {
		fmt.Fprintf(ps.stderr, "\nultrac: %s\n", sig)
		return signalExitCode(sig)
	}

	state := a.State()
	exitCode := 0
	switch {
	case lastAssistant == nil:
		// No assistant message at all: the run failed before the first
		// token. State carries the reason.
		msg := state.ErrorMessage
		if msg == "" {
			msg = "no response was produced"
		}
		fmt.Fprintf(ps.stderr, "ultrac: %s\n", msg)
		exitCode = 1
	case lastAssistant.StopReason == ai.StopError || lastAssistant.StopReason == ai.StopAborted:
		// pi: print-mode.ts:144-147 — the error goes to stderr, the exit
		// code says so, and no answer is printed.
		msg := lastAssistant.ErrorMessage
		if msg == "" {
			msg = fmt.Sprintf("request %s", lastAssistant.StopReason)
		}
		fmt.Fprintf(ps.stderr, "ultrac: %s\n", msg)
		exitCode = 1
	case jsonMode:
		// The event stream already carried everything; pi prints nothing
		// more in json mode.
	default:
		for _, b := range lastAssistant.Content {
			if b.Type == ai.BlockText {
				fmt.Fprintln(ps.stdout, b.Text)
			}
		}
	}

	if f.Debug {
		fmt.Fprintf(ps.stderr, "[debug] session: %s\n", mgr.SessionFile())
	}
	return exitCode
}

// openSession creates a session or resumes the most recent one for the cwd.
// The seed context is empty for a new session.
func (ps *printSession) openSession() (*session.SessionManager, session.SessionContext, error) {
	if ps.flags.Continue {
		mgr, err := session.ContinueRecentSession(ps.cwd)
		if err != nil {
			return nil, session.SessionContext{}, err
		}
		seed := mgr.BuildSessionContext()
		// Restore missing metadata the way pi does on resume
		// (sdk.ts:449-452): an older file without a thinking entry gets one
		// so a later resume still finds the level.
		if seed.ThinkingLevel == "" && !hasThinkingEntry(mgr) {
			_, _ = mgr.AppendThinkingLevelChange(string(agent.ThinkOff))
		}
		return mgr, seed, nil
	}
	return session.CreateSession(ps.cwd), session.SessionContext{}, nil
}

// recordSessionMeta writes the model and thinking level a new session needs
// so a resume restores them (pi: sdk.ts:453-458). A resumed session already
// carries them, so nothing is written.
//
// The entries land before any message thanks to the lazy flush: nothing is
// written to disk until the first user or assistant message, at which point
// the header and every entry so far go out in one create-write.
func (ps *printSession) recordSessionMeta(mgr *session.SessionManager, model ai.Model) {
	if ps.flags.Continue {
		return
	}
	if _, err := mgr.AppendModelChange(string(model.Provider), model.ID); err != nil && ps.flags.Debug {
		fmt.Fprintf(ps.stderr, "[debug] session append: %v\n", err)
	}
	if _, err := mgr.AppendThinkingLevelChange(string(agent.ThinkOff)); err != nil && ps.flags.Debug {
		fmt.Fprintf(ps.stderr, "[debug] session append: %v\n", err)
	}
}

// persistable reports whether a role is written to the session file. The set
// is pi's (agent-session.ts:1158-1163): system, user, assistant, toolResult.
func persistable(role ai.Role) bool {
	switch role {
	case ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
		return true
	default:
		return false
	}
}

// currentSections returns the sections the transcript's system messages
// currently declare, or nil when there is no system message yet.
func currentSections(messages []ai.Message) map[string]*string {
	for _, m := range messages {
		if m.Role == ai.RoleSystem {
			return m.Sections
		}
	}
	return nil
}

// systemPatchMessage builds the diff message. The empty text block is not
// decoration: pi writes content "" on the wire, and a system message whose
// content key is absent (or null) breaks pi's contentText on read
// (ai/src/utils/text.ts:5-12).
func systemPatchMessage(sections map[string]*string) ai.Message {
	return ai.Message{
		Role:        ai.RoleSystem,
		Content:     []ai.ContentBlock{ai.TextBlock("")},
		Sections:    sections,
		TimestampMS: time.Now().UnixMilli(),
	}
}

// userMessage builds the prompt message with the timestamp pi records
// (agent-session.ts:1912-1916).
func userMessage(text string) ai.Message {
	m := ai.NewTextMessage(ai.RoleUser, text)
	m.TimestampMS = time.Now().UnixMilli()
	return m
}

// hasThinkingEntry reports whether the session file already carries a
// thinking_level_change entry.
func hasThinkingEntry(mgr *session.SessionManager) bool {
	for _, e := range mgr.Entries() {
		if e.Type == session.TypeThinkingLevelChange {
			return true
		}
	}
	return false
}
