package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ultrac/internal/config"
	"ultrac/internal/contract/event"
	"ultrac/internal/contract/provider"
	"ultrac/internal/contract/tool"
	"ultrac/internal/engine"
	"ultrac/internal/engine/prompt"
	"ultrac/internal/llm/anthropic"
	"ultrac/internal/logging"
	"ultrac/internal/permission"
	"ultrac/internal/session"
	"ultrac/internal/tool/builtin"
	"ultrac/internal/transport"
	"ultrac/internal/transport/sink"
	"ultrac/internal/version"
)

// runAgent is the T1 path: one prompt, a full agent loop, an exit code.
//
// The wiring order is not arbitrary. Settings must be resolved and applied to
// the process environment before the provider is constructed, because the
// provider reads its endpoint and credential from that environment the same
// way every other component does.
func runAgent(f *agentFlags, stdout, stderr io.Writer) int {
	if f.Debug {
		logging.SetDefault(logging.New(logging.Options{
			Level:   logging.LevelDebug,
			Version: version.Get().Version,
		}))
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "ultrac: %v\n", err)
		return 1
	}

	// -- 1. Settings ------------------------------------------------------
	r := config.NewFileResolver()
	if f.Settings != "" {
		// --settings may be a path or an inline JSON document. The TypeScript
		// flag accepts both, and a user pasting a path that starts with "{" is
		// not a case worth supporting wrongly.
		if strings.HasPrefix(strings.TrimSpace(f.Settings), "{") {
			r.FlagInline = json.RawMessage(f.Settings)
		} else {
			abs, aerr := filepath.Abs(f.Settings)
			if aerr != nil {
				fmt.Fprintf(stderr, "ultrac: --settings: %v\n", aerr)
				return 1
			}
			r.FlagPath = abs
		}
	}

	ctx, cancel := signalContext()
	defer cancel()

	layers, loadErr := r.Layers(ctx, cwd)

	// A headless run applies project-scoped env in full rather than through
	// the safe allowlist. That matches the TypeScript behaviour for
	// non-interactive sessions — there is no trust dialog to answer, so the
	// directory the user launched in is taken as trusted — and it is the only
	// way a project's settings.json can configure the provider. It is stated
	// here rather than left implicit because it is a real widening of what a
	// cloned repository can influence.
	envRes, err := config.ApplyToEnv(layers, config.PhaseAfterTrust, os.Setenv)
	if err != nil {
		fmt.Fprintf(stderr, "ultrac: applying settings: %v\n", err)
		return 1
	}
	if f.Debug {
		for _, sk := range envRes.Skipped {
			fmt.Fprintf(stderr, "[debug] env %s skipped from %s: %s\n", sk.Key, sk.Source, sk.Why)
		}
	}
	if loadErr != nil {
		fmt.Fprintf(stderr, "warning: %v\n", loadErr)
	}

	effective := config.EffectiveEnv(layers)

	// -- 2. Permission ----------------------------------------------------
	mode, rules, modeErr := resolvePolicy(f, layers, effective)
	if modeErr != nil {
		fmt.Fprintf(stderr, "ultrac: %v\n", modeErr)
		return 2
	}

	extraDirs := make([]string, 0, len(f.AddDirs))
	for _, d := range f.AddDirs {
		abs, aerr := filepath.Abs(d)
		if aerr != nil {
			fmt.Fprintf(stderr, "ultrac: --add-dir %s: %v\n", d, aerr)
			return 2
		}
		info, serr := os.Stat(abs)
		if serr != nil || !info.IsDir() {
			fmt.Fprintf(stderr, "ultrac: --add-dir %s is not a directory\n", d)
			return 2
		}
		extraDirs = append(extraDirs, abs)
	}

	perm := &permission.Engine{
		Mode:      mode,
		Rules:     rules,
		WorkDir:   cwd,
		ExtraDirs: extraDirs,
		ConfigDir: config.ConfigHome(),
	}

	// -- 3. Provider ------------------------------------------------------
	model := f.Model
	if model == "" {
		model = effective[config.EnvModel]
	}
	baseURL := effective[config.EnvBaseURL]
	apiKey := effective[config.EnvAPIKey]
	authToken := effective[config.EnvAuthToken]

	if baseURL == "" && apiKey == "" && authToken == "" {
		fmt.Fprintln(stderr, "ultrac: no model endpoint or credential is configured.")
		fmt.Fprintln(stderr, "        Put ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN (or ANTHROPIC_API_KEY)")
		fmt.Fprintln(stderr, "        in the \"env\" block of "+filepath.Join(config.ConfigHome(), "settings.json")+".")
		fmt.Fprintln(stderr, "        Run `ultrac config` to see what the layers currently resolve to.")
		return 1
	}
	if model == "" {
		fmt.Fprintln(stderr, "ultrac: no model configured: set ANTHROPIC_MODEL or pass --model")
		return 1
	}

	p, err := provider.New(provider.Config{
		Kind:      anthropic.Kind,
		BaseURL:   baseURL,
		Model:     model,
		APIKey:    apiKey,
		AuthToken: authToken,
	})
	if err != nil {
		fmt.Fprintf(stderr, "ultrac: %v\n", err)
		return 1
	}

	sessionID := f.SessionID
	if sessionID == "" {
		sessionID = session.NewUUID()
	} else if !session.ValidUUID(sessionID) {
		fmt.Fprintf(stderr, "ultrac: --session-id %q is not a UUID\n", sessionID)
		return 2
	}

	// -- 4. Tools ---------------------------------------------------------
	registry := tool.NewRegistry(tool.Builtins()...)

	// -- 5. Prompt --------------------------------------------------------
	toolNames := registry.Names()
	systemPrompt := prompt.Build(prompt.Options{
		WorkDir:              cwd,
		Model:                model,
		Version:              version.Get().Version,
		Tools:                toolNames,
		IsNonInteractive:     true,
		CustomSystemPrompt:   f.SystemPrompt,
		AppendSystemPrompt:   f.AppendSystemPrompt,
		OverrideSystemPrompt: os.Getenv("ULTRAC_SYSTEM_PROMPT"),
		ExtraSections:        todoSection(sessionID),
	})

	// -- 6. Sinks ---------------------------------------------------------
	//
	// Exactly one sink is chosen, because the three formats disagree about
	// what stdout is: prose, a log of JSON lines, or a single JSON document.
	// Fanning out would produce output no reader could parse.
	var (
		ndjson *transport.Writer
		text   *sink.Text
		jsonS  *sink.JSON
		out    event.Sink = sink.Discard{}
	)
	switch f.OutputFormat {
	case "stream-json":
		ndjson = transport.NewWriter(stdout, sessionID)
		ndjson.SetInit(transport.InitInfo{
			CWD:            cwd,
			Model:          model,
			Tools:          toolNames,
			PermissionMode: string(mode),
			Version:        version.Get().Version,
			APIKeySource:   credentialSource(authToken, apiKey),
		})
		if err := ndjson.Init(); err != nil {
			fmt.Fprintf(stderr, "ultrac: %v\n", err)
			return 1
		}
		out = ndjson
	case "json":
		jsonS = &sink.JSON{Out: stdout}
		out = jsonS
	default:
		text = sink.NewText(stdout, stderr)
		text.ShowUsage = f.Debug
		out = text
	}

	// -- 7. Run -----------------------------------------------------------
	eng := engine.New(engine.Options{
		Provider:     p,
		Tools:        registry,
		Model:        model,
		SystemPrompt: systemPrompt,
		WorkDir:      cwd,
		SessionID:    sessionID,
		Permission:   perm,
		// No asker is installed: a headless run has nobody to answer, and an
		// unanswerable prompt must become a denial rather than a hang. The
		// denial message names the flag that would allow the call.
		Ask:       nil,
		MaxTurns:  f.MaxTurns,
		MaxTokens: 0,
	})

	start := time.Now()
	handle, seq := eng.Result(ctx, engine.Params{Prompt: f.Prompt})
	drainErr := event.Drain(ctx, seq, out)
	elapsed := time.Since(start)

	if text != nil {
		_ = text.Finish()
	}

	summary, _ := handle.Wait(context.Background())
	summary.IsError = summary.IsError || drainErr != nil

	if !f.NoTranscript {
		writeTranscript(cwd, sessionID, f.Prompt, summary)
	}

	exitCode := 0
	switch {
	case drainErr != nil && ctx.Err() != nil:
		fmt.Fprintf(stderr, "\nultrac: cancelled\n")
		exitCode = 130
	case summary.Failure != nil:
		exitCode = 1
	case drainErr != nil:
		fmt.Fprintf(stderr, "\nultrac: %v\n", drainErr)
		exitCode = 1
	}

	switch {
	case ndjson != nil:
		_ = ndjson.Result(transport.ResultPayload{
			Subtype:           resultSubtype(summary),
			IsError:           summary.IsError,
			DurationMS:        elapsed.Milliseconds(),
			NumTurns:          summary.Turns,
			Result:            summary.Text,
			StopReason:        string(summary.Stop),
			Usage:             summary.Usage,
			Errors:            failureMessages(summary),
			TotalCostUSD:      0,
			PermissionDenials: []any{},
		})
	case jsonS != nil:
		_ = jsonS.Write(map[string]any{
			"subtype":     resultSubtype(summary),
			"is_error":    summary.IsError,
			"num_turns":   summary.Turns,
			"duration_ms": elapsed.Milliseconds(),
			"stop_reason": string(summary.Stop),
			"session_id":  sessionID,
		})
	}

	if f.Debug {
		fmt.Fprintf(stderr, "\n[%d turn(s), %d tool call(s), %s]\n",
			summary.Turns, summary.ToolCalls, elapsed.Round(time.Millisecond))
	}
	if ret := summary.Failure; ret != nil {
		fmt.Fprintf(stderr, "ultrac: %s: %s\n", ret.Code, ret.Message)
	}
	return exitCode
}

// resolvePolicy turns the flags and the settings stack into a mode and rules.
//
// Precedence is deliberate: a flag beats the file. `--permission-mode` exists
// precisely so a user can override what a repository's settings.json asks for,
// and a project file that outranked the command line would make the flag a
// decoration.
func resolvePolicy(f *agentFlags, layers []config.Layer, effective map[string]string) (permission.Mode, *permission.Set, error) {
	modeName := f.Permission
	rules := permission.NewSet()

	// Settings are read lowest-precedence first, so a later layer's rules are
	// appended after an earlier one's. Deny is consulted first at decision
	// time, so an ordering artefact between an allow and a deny cannot let an
	// allow win.
	for _, layer := range layers {
		m, rs := policyFromSettings(layer.Settings, layer.Source)
		for _, r := range rs {
			rules.Add(r)
		}
		// First layer that names a mode wins, and layers arrive lowest
		// precedence first, so a higher-precedence file is what ends up
		// applying only if the lower one did not set a mode at all. That
		// ordering is the resolver's contract, not this loop's choice.
		if modeName == "" && m != "" {
			modeName = string(m)
		}
	}

	if modeName == "" {
		modeName = effective["ULTRAC_PERMISSION_MODE"]
	}
	if modeName == "" {
		modeName = string(permission.ModeDefault)
	}

	mode, err := permission.ParseMode(modeName)
	if err != nil {
		return "", nil, err
	}
	return mode, rules, nil
}

// policyFromSettings reads the permissions block out of one settings layer.
//
// The shape is the TypeScript one — allow / deny / ask arrays plus
// defaultMode — because these files are shared between the two
// implementations during the migration and a divergent reader would make the
// same file mean two things.
func policyFromSettings(s config.Settings, src config.Source) (permission.Mode, []permission.Rule) {
	raw, ok := s.Extra["permissions"]
	if !ok || len(raw) == 0 {
		return "", nil
	}
	var block struct {
		DefaultMode           string   `json:"defaultMode"`
		Allow                 []string `json:"allow"`
		Deny                  []string `json:"deny"`
		Ask                   []string `json:"ask"`
		AdditionalDirectories []string `json:"additionalDirectories"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return "", nil
	}

	ruleSource := ruleSourceFor(src)
	out := make([]permission.Rule, 0, len(block.Allow)+len(block.Deny)+len(block.Ask))
	for _, s := range block.Allow {
		out = append(out, permission.ParseRule(s, permission.BehaviorAllow, ruleSource))
	}
	for _, s := range block.Deny {
		out = append(out, permission.ParseRule(s, permission.BehaviorDeny, ruleSource))
	}
	for _, s := range block.Ask {
		out = append(out, permission.ParseRule(s, permission.BehaviorAsk, ruleSource))
	}
	return permission.Mode(block.DefaultMode), out
}

func ruleSourceFor(src config.Source) permission.RuleSource {
	switch src {
	case config.SourceUser:
		return permission.SourceUserSettings
	case config.SourceProject:
		return permission.SourceProjectSettings
	case config.SourceLocal:
		return permission.SourceLocalSettings
	case config.SourceFlag:
		return permission.SourceFlagSettings
	case config.SourcePolicy:
		return permission.SourcePolicySettings
	default:
		return permission.SourceUserSettings
	}
}

// todoSection renders the session's checklist into the prompt.
//
// The list has to be in the prompt rather than only in the tool result: the
// tool result scrolls out of the context window, and the point of a checklist
// is that it survives long enough to be worked through.
func todoSection(sessionID string) []string {
	rendered := builtin.RenderTodos(sessionID)
	if rendered == "" {
		return nil
	}
	return []string{rendered}
}

// resultSubtype names the terminal result the way the SDK does.
func resultSubtype(s engine.Result) string {
	if s.Failure == nil {
		return "success"
	}
	switch s.Failure.Code {
	case event.CodeMaxTurns:
		return "error_max_turns"
	case event.CodeMaxTokens:
		return "error_max_output_tokens"
	default:
		return "error_during_execution"
	}
}

func failureMessages(s engine.Result) []string {
	if s.Failure == nil {
		return nil
	}
	return []string{s.Failure.Code + ": " + s.Failure.Message}
}

func credentialSource(authToken, apiKey string) string {
	switch {
	case authToken != "":
		return "ANTHROPIC_AUTH_TOKEN"
	case apiKey != "":
		return "ANTHROPIC_API_KEY"
	default:
		return "none"
	}
}

// writeTranscript records the run.
//
// The record is deliberately minimal — the prompt, the final answer, and a
// last-prompt marker — rather than a full event log. A partial transcript that
// claims to be replayable would break --resume in a way that looks like data
// corruption; T2.6 is where a replayable transcript lands.
func writeTranscript(cwd, sessionID, promptText string, summary engine.Result) {
	store := session.NewStore()
	defer store.Close()

	path := store.TranscriptPath(cwd, sessionID)
	entries := []*session.Entry{
		session.NewUserEntry(promptText, sessionID, cwd),
		session.NewAssistantEntry(map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": summary.Text},
			},
		}, sessionID, cwd),
	}
	for _, e := range entries {
		e.Version = version.Get().Version
		e.UserType = "external"
	}
	// Chain the entries so BuildChain can reconstruct them.
	entries[1].ParentUUID = entries[0].UUID
	entries = append(entries, &session.Entry{
		Type:       session.EntryLastPrompt,
		UUID:       session.NewUUID(),
		ParentUUID: entries[1].UUID,
		Timestamp:  session.NowISO(),
		SessionID:  sessionID,
		CWD:        cwd,
		LastPrompt: promptText,
	})

	if err := store.Append(path, entries...); err != nil {
		// A transcript that cannot be written is a warning, not a failure:
		// the answer the user asked for has already been produced.
		return
	}
	_ = store.Flush(path)
}
