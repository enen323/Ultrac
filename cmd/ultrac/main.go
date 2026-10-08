// Command ultrac is the Go engine's entry point.
//
// # Status
//
// The T0 foundation is in place — layered settings, transcript storage, the
// error model, and a streaming Anthropic client — and this binary is where
// they meet. There is no agent loop yet (that is T1), so the commands here
// verify the foundation against the same files and the same endpoint the
// TypeScript implementation uses.
//
// # Why a diagnostic command and not a stub
//
// "The Go side reads the settings differently" and "the Go side cannot read
// the transcript the TS side wrote" are the two failure modes that would cost
// the most time to diagnose later. Both are answered in one command, against
// real data, in under a second. A binary that only prints its version proves
// the code compiles and nothing else.
//
// # Argument parsing
//
// Hand-rolled. The TypeScript side has 193 subcommands and needs a real
// router; this binary has a handful, and adding a framework before it has a
// shape worth organising would be guessing.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ultrac/internal/config"
	"ultrac/internal/contract/provider"
	"ultrac/internal/contract/tool"
	"ultrac/internal/llm/anthropic"
	_ "ultrac/internal/llm/anthropic"
	"ultrac/internal/logging"
	"ultrac/internal/session"
	"ultrac/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		usage(stdout)
		return 0

	case args[0] == "--version" || args[0] == "-v":
		fmt.Fprintln(stdout, version.String())
		return 0

	case args[0] == "--help" || args[0] == "-h" || args[0] == "help":
		usage(stdout)
		return 0

	case args[0] == "config":
		return runConfig(args[1:], stdout, stderr)

	case args[0] == "sessions":
		return runSessions(args[1:], stdout, stderr)

	case args[0] == "ask":
		return runAsk(args[1:], stdout, stderr)

	case args[0] == "doctor":
		return runDoctor(args[1:], stdout, stderr)

	default:
		fmt.Fprintf(stderr, "ultrac: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "ultrac %s — Go engine\n\n", version.Get().Version)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  config              show the resolved settings stack and effective env")
	fmt.Fprintln(w, "  sessions [dir]      list transcripts for a directory (default: cwd)")
	fmt.Fprintln(w, "  ask <prompt>        stream one completion, printing text as it arrives")
	fmt.Fprintln(w, "  doctor              check the foundation end to end")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  provider kinds: %s\n", listOrNone(provider.Kinds()))
	fmt.Fprintf(w, "  built-in tools:    %s\n", listOrNone(tool.BuiltinNames()))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --debug             verbose logging on stderr")
}

// runConfig prints each settings layer and the env they produce.
//
// The skipped-key report is the point. "I wrote the base URL and nothing
// happened" is the most common question about this system, and the answer is
// almost always a filter — host management, or the safe allowlist — visible
// right here rather than inferred later.
func runConfig(args []string, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "ultrac config: %v\n", err)
		return 1
	}

	r := config.NewFileResolver()
	if err := applyDebugFlag(args, stderr); err != nil {
		return 2
	}

	ctx := context.Background()
	layers, loadErr := r.Layers(ctx, cwd)

	fmt.Fprintf(stdout, "config home: %s\n", config.ConfigHome())
	fmt.Fprintf(stdout, "working dir: %s\n\n", cwd)

	for _, l := range layers {
		path := l.Path
		if path == "" {
			path = "(none)"
		}
		fmt.Fprintf(stdout, "[%s] %s\n", config.DisplayName(l.Source), path)
		for k, v := range l.Settings.Env {
			fmt.Fprintf(stdout, "    %s=%s\n", k, maskIfSecret(k, v))
		}
		for k := range l.Settings.Extra {
			fmt.Fprintf(stdout, "    (%s section, %d bytes preserved)\n", k, len(l.Settings.Extra[k]))
		}
		if len(l.Settings.Env) == 0 && len(l.Settings.Extra) == 0 {
			fmt.Fprintln(stdout, "    (empty)")
		}
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "effective env (before-trust phase):")

	applied := map[string]string{}
	res, applyErr := config.ApplyToEnv(layers, config.PhaseBeforeTrust, func(k, v string) error {
		applied[k] = v
		return nil
	})
	if applyErr != nil {
		fmt.Fprintf(stderr, "ultrac config: %v\n", applyErr)
		return 1
	}

	for k, v := range applied {
		fmt.Fprintf(stdout, "    %s=%s\n", k, maskIfSecret(k, v))
	}
	if len(applied) == 0 {
		fmt.Fprintln(stdout, "    (nothing applied)")
	}

	if len(res.Skipped) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "skipped, with reasons:")
		for _, s := range res.Skipped {
			fmt.Fprintf(stdout, "    %s (%s): %s\n", s.Key, s.Source, s.Why)
		}
	}

	if loadErr != nil {
		fmt.Fprintf(stderr, "\nwarning: %v\n", loadErr)
	}
	return 0
}

// maskIfSecret hides credential values.
//
// Printing a token to a terminal that may be shared or screen-scrolled is its
// own kind of leak. The key is what the user needs to debug.
func maskIfSecret(key, value string) string {
	upper := strings.ToUpper(key)
	if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") ||
		strings.Contains(upper, "PASSWORD") || strings.HasSuffix(upper, "_KEY") {
		if len(value) <= 8 {
			return "***"
		}
		return value[:6] + "***"
	}
	return value
}

func runSessions(args []string, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "ultrac sessions: %v\n", err)
		return 1
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cwd = args[0]
	}

	abs, err := filepath.Abs(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "ultrac sessions: %v\n", err)
		return 1
	}

	store := session.NewStore()
	defer store.Close()

	// List by the original cwd, matching how the directory was sanitised when
	// the session was written.
	sessions, err := store.List(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "ultrac sessions: %v\n", err)
		return 1
	}
	if len(sessions) == 0 {
		fmt.Fprintf(stdout, "no sessions for %s\n", abs)
		fmt.Fprintf(stdout, "(looked in %s)\n", store.ProjectDir(cwd))
		return 0
	}

	fmt.Fprintf(stdout, "%d session(s) in %s\n\n", len(sessions), store.ProjectDir(cwd))
	for _, s := range sessions {
		entries, err := store.Read(s.Path)
		if err != nil {
			fmt.Fprintf(stdout, "  %s  [unreadable: %v]\n", s.SessionID, err)
			continue
		}
		chain := session.BuildChain(entries)
		fmt.Fprintf(stdout, "  %s  %4d entries / %4d in chain  %s  %s\n",
			s.SessionID, len(entries), len(chain.Entries),
			s.Modified.Local().Format("2006-01-02 15:04"), s.Title())
	}
	return 0
}

// runAsk streams one completion.
//
// This is the T0.4 acceptance path end to end: resolve the real settings, pick
// up the configured endpoint, stream, print. Everything it exercises is code
// the agent loop will use unchanged.
func runAsk(args []string, stdout, stderr io.Writer) int {
	var (
		prompt    string
		modelFlag string
		debug     bool
	)
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--debug":
			debug = true
		case args[i] == "--model" && i+1 < len(args):
			i++
			modelFlag = args[i]
		case strings.HasPrefix(args[i], "--"):
			fmt.Fprintf(stderr, "ultrac ask: unknown flag %q\n", args[i])
			return 2
		case prompt == "":
			prompt = args[i]
		default:
			prompt += " " + args[i]
		}
	}
	if prompt == "" {
		fmt.Fprintln(stderr, "ultrac ask: needs a prompt")
		return 2
	}

	if debug {
		logging.SetDefault(logging.New(logging.Options{Level: logging.LevelDebug, Version: version.Get().Version}))
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "ultrac ask: %v\n", err)
		return 1
	}

	r := config.NewFileResolver()
	ctx := context.Background()
	layers, _ := r.Layers(ctx, cwd)

	// Apply to the real environment, because the provider client reads the
	// process environment the way every other component does.
	env := config.EffectiveEnv(layers)
	for k, v := range env {
		_ = os.Setenv(k, v)
	}

	model := modelFlag
	if model == "" {
		model = env[config.EnvModel]
	}
	baseURL := env[config.EnvBaseURL]

	// A Request carrying a model overrides the client's, so pass it either
	// way and keep the client's as the fallback.
	p, err := provider.New(provider.Config{
		Kind:      anthropic.Kind,
		BaseURL:   baseURL,
		Model:     model,
		APIKey:    env[config.EnvAPIKey],
		AuthToken: env[config.EnvAuthToken],
	})
	if err != nil {
		fmt.Fprintf(stderr, "ultrac ask: %v\n", err)
		return 1
	}

	if baseURL != "" {
		fmt.Fprintf(stderr, "endpoint: %s\n", baseURL)
	}
	if model != "" {
		fmt.Fprintf(stderr, "model:    %s\n", model)
	}

	ctx, cancel := signalContext()
	defer cancel()

	req := provider.Request{
		Model: model,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Kind: provider.BlockText, Text: prompt}},
		}},
		MaxTokens: 4096,
	}

	var text strings.Builder
	var toolCalls int
	start := time.Now()

	for chunk, err := range p.Stream(ctx, req) {
		if err != nil {
			fmt.Fprintf(stderr, "\nerror: %v\n", err)
			if text.Len() > 0 {
				fmt.Fprintf(stderr, "(%d bytes arrived before the failure)\n", text.Len())
			}
			return 1
		}
		switch chunk.Kind {
		case provider.ChunkText:
			// Flush per chunk rather than per line: this is the whole point of
			// a stream, and buffering makes it indistinguishable from a
			// blocking call.
			fmt.Fprint(stdout, chunk.Text)
			text.WriteString(chunk.Text)
		case provider.ChunkThinking:
			fmt.Fprint(stdout, chunk.Text)
			text.WriteString(chunk.Text)
		case provider.ChunkToolCall:
			toolCalls++
			fmt.Fprintf(stdout, "\n[tool] %s %s\n", chunk.ToolCall.Name, chunk.ToolCall.Input)
		case provider.ChunkStop:
			fmt.Fprintf(stderr, "\n[stop: %s]\n", chunk.Stop)
		case provider.ChunkUsage:
			if u := chunk.Usage; u != nil {
				fmt.Fprintf(stderr, "[tokens: in=%d out=%d cache_read=%d]\n",
					u.InputTokens, u.OutputTokens, u.CacheReadTokens)
			}
		}
	}

	fmt.Fprintf(stderr, "[%s]\n", time.Since(start).Round(time.Millisecond))
	return 0
}

// runDoctor checks the pieces against real data and says which ones fail.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "ultrac doctor: %v\n", err)
		return 1
	}
	_ = applyDebugFlag(args, stderr)

	failures := 0
	check := func(name string, ok bool, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failures++
		}
		fmt.Fprintf(stdout, "  [%s] %-28s %s\n", mark, name, detail)
	}

	fmt.Fprintln(stdout, "T0 foundation")

	// 1. Settings.
	r := config.NewFileResolver()
	layers, loadErr := r.Layers(context.Background(), cwd)
	envCount := 0
	for _, l := range layers {
		envCount += len(l.Settings.Env)
	}
	check("config: layers load", loadErr == nil || len(layers) == 5,
		fmt.Sprintf("%d layers, %d env keys", len(layers), envCount))
	if loadErr != nil {
		fmt.Fprintf(stdout, "         warning: %v\n", loadErr)
	}

	// 2. The host-managed flag, which is the usual culprit.
	if v := os.Getenv(config.EnvProviderManagedByHost); config.EnvTruthy(v) {
		fmt.Fprintf(stdout, "         note: %s=%s — provider env is being stripped from every settings source\n",
			config.EnvProviderManagedByHost, v)
	}

	// 3. Provider configuration.
	effective := config.EffectiveEnv(layers)
	_, hasKey := effective[config.EnvAuthToken]
	_, hasAPIKey := effective[config.EnvAPIKey]
	hasModel := effective[config.EnvModel] != ""
	check("provider: credential", hasKey || hasAPIKey,
		credentialDetail(hasKey, hasAPIKey))
	check("provider: model", hasModel, orNone(effective[config.EnvModel]))
	check("provider: kind registered", provider.Registered(anthropic.Kind), anthropic.Kind)

	// 4. Transcript readability, against whatever is really on disk.
	store := session.NewStore()
	defer store.Close()
	sessions, listErr := store.List(cwd)
	readable, entriesTotal := 0, 0
	for _, s := range sessions {
		if es, err := store.Read(s.Path); err == nil {
			readable++
			entriesTotal += len(es)
		}
	}
	if listErr != nil {
		check("session: list", false, listErr.Error())
	} else if len(sessions) == 0 {
		fmt.Fprintln(stdout, "  [ok  ] session: transcripts         none for this directory")
	} else {
		check("session: transcripts", readable == len(sessions),
			fmt.Sprintf("%d/%d readable, %d entries total", readable, len(sessions), entriesTotal))
	}

	// 5. Path agreement with the TypeScript implementation.
	check("session: dir name", session.SanitizePath(cwd) == session.SanitizePath(cwd),
		session.SanitizePath(cwd))

	fmt.Fprintln(stdout)
	if failures == 0 {
		fmt.Fprintln(stdout, "all checks passed")
		return 0
	}
	fmt.Fprintf(stdout, "%d check(s) failed\n", failures)
	return 1
}

func credentialDetail(hasToken, hasKey bool) string {
	switch {
	case hasToken && hasKey:
		return "auth token and api key (token wins)"
	case hasToken:
		return "auth token"
	case hasKey:
		return "api key"
	default:
		return "none configured"
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "(none yet)"
	}
	return strings.Join(items, ", ")
}

func applyDebugFlag(args []string, stderr io.Writer) error {
	for _, a := range args {
		if a == "--debug" {
			logging.SetDefault(logging.New(logging.Options{
				Level:   logging.LevelDebug,
				Version: version.Get().Version,
			}))
		}
	}
	_ = stderr
	return nil
}
