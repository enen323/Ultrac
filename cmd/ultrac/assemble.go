// assemble.go — the harness assembly shared by the print and RPC modes:
// settings → session → model → tools → prompt sections → agent
// (docs/pi-core-harness-plan.md §3.6 / §3.7).
//
// # Why this is a file of its own
//
// P5 built this chain inline in print.go because there was one mode. P6
// adds a second (RPC) that needs the same chain plus a long-lived agent,
// and the two must not drift: a model resolved differently in RPC than in
// print is a bug that only shows up on the wire. Both modes now assemble
// through buildHarness and diverge only after it (print runs one prompt and
// exits; RPC serves commands until stdin ends).
//
// # The chain, and where each step's authority lives
//
//   - Settings: read from the layer files, never applied to the process
//     environment (print.go's file comment explains why — a print or RPC
//     run must not leak credentials into a subprocess it spawns).
//   - Session: opened before the model, because a resumed session can
//     supply the model when neither flag nor settings name one.
//   - Model: resolveModel's fallback chain (flag → settings → catalog →
//     base-URL host → provider default).
//   - Prompt: the sections are rebuilt per prompt, exactly as pi
//     recomputes them in _preparePromptAndToolLoadout
//     (agent-session.ts:1742-1754) — a context file edited mid-session
//     shows up in the next prompt's patch.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/compact"
	"ultrac/internal/config"
	"ultrac/internal/logging"
	"ultrac/internal/prompt"
	"ultrac/internal/rpc"
	"ultrac/internal/session"
	"ultrac/internal/tool"
	"ultrac/internal/version"
)

// harness is one assembled agent stack. It is the Host that cmd/ultrac's
// RPC layer drives (see rpc.go) and the source print.go runs its single
// prompt from.
type harness struct {
	flags *agentFlags
	cwd   string

	mgr       *session.SessionManager
	seed      session.SessionContext
	model     ai.Model
	tools     []agent.Tool
	apiKey    string
	effective map[string]string

	// resolver is kept so the RPC layer can persist settings changes
	// (set_auto_compaction) through the same read-modify-write cycle the
	// startup resolution used.
	resolver *config.FileResolver
	// compaction is the resolved compaction configuration (settings
	// section + engine defaults). newAgent hands it to every compactor it
	// builds.
	compaction compact.Settings
	// compactor is the executor of the CURRENT agent/session pair. It is
	// rebuilt by every newAgent call (new_session / resume_session swap
	// the pair), so callers re-read it per command instead of caching.
	compactor *compactor
	// emitEvent puts agent events on the mode's wire (the RPC and
	// print-json PiJSONWriter). Nil in text mode: compaction events have
	// nowhere to go and are dropped, exactly as pi drops them in a mode
	// with no event stream.
	emitEvent func(agent.AgentEvent) error

	// streamFn overrides the provider registry. Nil in production; tests
	// inject a fake so no request leaves the process.
	streamFn agent.StreamFn
}

// buildHarness assembles the stack. A non-zero return is the exit code;
// the reason is on stderr. The codes match P5's print path: 2 for a bad
// invocation (an unwritable --cwd, an unresolvable model), 1 for a setup
// failure.
//
// streamFn overrides the provider registry (tests inject a fake); nil in
// production.
func buildHarness(f *agentFlags, cwd string, stderr io.Writer, streamFn agent.StreamFn) (*harness, int) {
	if f.Debug {
		logging.SetDefault(logging.New(logging.Options{
			Level:   logging.LevelDebug,
			Version: version.Get().Version,
		}))
	}

	// -- 1. Settings ------------------------------------------------------
	//
	// Layers are read but never applied to the process environment. A load
	// error is a warning, not a failure: a broken project settings file
	// must not take down a run that the user settings alone can serve.
	r := config.NewFileResolver()
	layers, loadErr := r.Layers(context.Background(), cwd)
	if loadErr != nil {
		fmt.Fprintf(stderr, "warning: %v\n", loadErr)
	}
	effective := config.EffectiveEnv(layers)

	// The compaction section resolves the same way: merge the layers, then
	// inject the engine defaults for absent keys (plan D6). The defaults
	// never enter a file — Get() reports what the files actually say.
	merged := config.Settings{}
	for _, l := range layers {
		merged = config.MergeSettings(merged, l.Settings)
	}
	compaction := resolveCompactionSettings(merged.Compaction, stderr)

	// -- 2. Session ---------------------------------------------------------
	//
	// Opened before the model is resolved because a resumed session can
	// supply the model when neither the flag nor the settings name one.
	mgr, seed, err := openSessionFor(f, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "ultrac: %v\n", err)
		return nil, 1
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
		fmt.Fprintf(stderr, "ultrac: %v\n", err)
		return nil, 2
	}

	apiKey := apiKeyFor(effective)
	if apiKey == "" {
		fmt.Fprintln(stderr, "ultrac: no credential is configured.")
		fmt.Fprintln(stderr, "        Put ANTHROPIC_AUTH_TOKEN (or ANTHROPIC_API_KEY) in the \"env\" block of")
		fmt.Fprintf(stderr, "        %s.\n", filepath.Join(config.ConfigHome(), "settings.json"))
		fmt.Fprintln(stderr, "        Run `ultrac config` to see what the layers currently resolve to.")
		return nil, 1
	}

	// -- 4. Tools ------------------------------------------------------------
	tools := tool.All(cwd)

	return &harness{
		flags:      f,
		cwd:        cwd,
		mgr:        mgr,
		seed:       seed,
		model:      model,
		tools:      tools,
		apiKey:     apiKey,
		effective:  effective,
		resolver:   r,
		compaction: compaction,
		streamFn:   streamFn,
	}, 0
}

// openSessionFor creates a session or resumes the most recent one for the
// cwd. The seed context is empty for a new session.
func openSessionFor(f *agentFlags, cwd string) (*session.SessionManager, session.SessionContext, error) {
	if f.Continue {
		mgr, err := session.ContinueRecentSession(cwd)
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
	return session.CreateSession(cwd), session.SessionContext{}, nil
}

// recordSessionMeta writes the model and thinking level a new session needs
// so a resume restores them (pi: sdk.ts:453-458). It is unconditional: the
// caller applies the policy — a resumed session already carries the entries
// (skip at startup), while every session created later (RPC new_session)
// needs them written whatever flag the process started with.
//
// The entries land before any message thanks to the lazy flush: nothing is
// written to disk until the first user or assistant message, at which point
// the header and every entry so far go out in one create-write.
func (h *harness) recordSessionMeta(mgr *session.SessionManager) {
	if _, err := mgr.AppendModelChange(string(h.model.Provider), h.model.ID); err != nil && h.flags.Debug {
		fmt.Fprintf(os.Stderr, "[debug] session append: %v\n", err)
	}
	if _, err := mgr.AppendThinkingLevelChange(string(agent.ThinkOff)); err != nil && h.flags.Debug {
		fmt.Fprintf(os.Stderr, "[debug] session append: %v\n", err)
	}
}

// newAgent builds an agent over the harness. resume replays the seed
// transcript (print --continue and the RPC mode's startup); a fresh session
// (RPC new_session) starts empty so its first prompt re-declares the whole
// sectioned prompt. mgr is the session the agent belongs to — after a
// new_session swap it is no longer h.mgr, and the agent's SessionID must
// name the session it actually writes to.
//
// The seed is a parameter rather than h.seed so the caller names which
// transcript is being replayed: the startup one for --continue, the
// freshly loaded one for the RPC resume_session command (rpc.go).
//
// It also builds the compaction executor for this pair and installs its
// PrepareRequest hook (docs/compaction-branch-plan.md P-C2): the hook is
// the auto-compaction trigger, and it must capture the compactor built for
// THIS agent — after a new_session swap the old one belongs to the
// abandoned pair. The compactor is published on h.compactor for the RPC
// layer's manual-compact command.
func (h *harness) newAgent(mgr *session.SessionManager, seed session.SessionContext, resume bool) *agent.Agent {
	initial := &agent.AgentInitialState{
		// Empty on purpose — the prompt lives in the sections patch, not in
		// a plain leading message (pi: sdk.ts:413).
		SystemPrompt: "",
		Model:        &h.model,
		Tools:        h.tools,
	}
	if resume {
		initial.Messages = seed.Messages
		// The transcript's thinking level rides along on resume: dropping
		// back to off would silently change how the continued conversation
		// reasons (pi restores it the same way on resume, sdk.ts:449-458).
		// The clamp is the command path's own rule (set_thinking_level
		// applies ai.ClampThinkingLevel the same way): a file whose
		// (model, level) pair is impossible — hand-edited, or written by a
		// build without the clamp — must not put a level the restored
		// model cannot honour onto the wire.
		if seed.ThinkingLevel != "" {
			level := agent.ThinkingLevel(ai.ClampThinkingLevel(
				ai.SupportedThinkingLevels(h.model),
				ai.ModelThinkingLevel(seed.ThinkingLevel),
			))
			initial.ThinkingLevel = &level
		}
	}
	compactor := newCompactor(mgr, h.compaction, h.apiKey, h.streamFn, h.emitEvent)
	a := agent.NewAgent(agent.AgentOptions{
		InitialState:   initial,
		GetAPIKey:      func(string) string { return h.apiKey },
		SessionID:      mgr.SessionID(),
		StreamFn:       h.streamFn,
		PrepareRequest: compactor.prepareRequest,
	})
	compactor.agent = a
	h.compactor = compactor
	return a
}

// promptOptions is the per-prompt prompt build configuration. The tool set
// is fixed for the process lifetime; the context files are re-read per
// prompt so a mid-session edit shows up in the next patch.
func (h *harness) promptOptions() prompt.Options {
	toolNames := make([]string, len(h.tools))
	for i, t := range h.tools {
		toolNames[i] = t.Name()
	}
	return prompt.Options{
		CWD:           h.cwd,
		SelectedTools: toolNames,
		ContextFiles:  prompt.LoadContextFiles(h.cwd),
	}
}

// buildPrompt assembles one prompt's messages: the system sections patch
// (nil when the transcript already declares the current sections) plus the
// user message with its images.
//
// pi: agent-session.ts:1742-1754 (_preparePromptAndToolLoadout) — the diff
// is computed per prompt, which is what makes a resumed session diff
// cleanly instead of re-declaring the whole prompt.
func (h *harness) buildPrompt(a *agent.Agent, text string, images []rpc.ImageContent) (rpc.PromptInput, error) {
	_, sections, err := prompt.BuildSystemPromptState(h.promptOptions())
	if err != nil {
		return rpc.PromptInput{}, fmt.Errorf("system prompt: %w", err)
	}
	patch := prompt.DiffSystemPromptSections(currentSections(a.State().Messages), sections)
	var patchMsg *ai.Message
	if patch != nil {
		m := systemPatchMessage(patch)
		patchMsg = &m
	}
	return rpc.PromptInput{Patch: patchMsg, User: userMessageWith(text, images)}, nil
}

// availableModels lists the models set_model accepts: the handwritten
// catalog plus the settings-resolved model when it is not already in it.
//
// pi's model runtime snapshot works the same way (rpc-mode.ts:471,
// getAvailableSnapshot) — a model the settings name is selectable even
// when the catalog has not caught up with the provider's lineup.
func (h *harness) availableModels() []ai.Model {
	out := ai.Catalog()
	if m, err := resolveModel("", "", h.effective); err == nil && !containsModel(out, m) {
		out = append(out, m)
	}
	return out
}

// containsModel reports whether the list already carries this
// provider/id pair.
func containsModel(models []ai.Model, m ai.Model) bool {
	for _, e := range models {
		if e.Provider == m.Provider && e.ID == m.ID {
			return true
		}
	}
	return false
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

// userMessageWith builds the prompt message with the timestamp pi records
// (agent-session.ts:1912-1916). Images ride along as content blocks after
// the text (pi's _queueSteer builds the same array, agent-session.ts:2265+).
func userMessageWith(text string, images []rpc.ImageContent) ai.Message {
	blocks := make([]ai.ContentBlock, 0, 1+len(images))
	blocks = append(blocks, ai.TextBlock(text))
	for _, img := range images {
		blocks = append(blocks, ai.ImageBlock(img.Data, img.MimeType))
	}
	return ai.Message{
		Role:        ai.RoleUser,
		Content:     blocks,
		TimestampMS: time.Now().UnixMilli(),
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
