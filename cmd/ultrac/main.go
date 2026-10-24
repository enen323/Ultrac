// Command ultrac is the Go engine's entry point — the pi harness port's CLI
// (docs/pi-core-harness-plan.md §3.6 / §6.3, phase P5).
//
// # Modes
//
//	print   `ultrac -p "prompt"` / `ultrac "prompt"` — run and exit
//	json    `ultrac --mode json "prompt"` — the event stream, one JSON per line
//	rpc     `ultrac --mode rpc` — the headless JSONL protocol on stdin/stdout (P6)
//
// The remaining diagnostic commands (config / sessions / probe) inspect the
// live stack: settings layers, the pi-shaped session store, and the provider
// registry. The T0-era ask / doctor commands were removed in P7 together
// with the packages they exercised (plan §3.8) — `probe` covers the
// endpoint, `config` covers the settings.
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
	"sort"
	"strings"

	"ultrac/internal/ai"
	_ "ultrac/internal/ai/anthropic" // blank import: init() registration
	_ "ultrac/internal/ai/openai"    // blank import: init() registration
	"ultrac/internal/config"
	"ultrac/internal/logging"
	"ultrac/internal/session"
	"ultrac/internal/tool"
	_ "ultrac/internal/tool" // blank import: the 8 tools' init() registrations
	"ultrac/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one invocation.
//
// Subcommands are recognised before flags are parsed, so `ultrac config
// --debug` keeps meaning "run the config command verbosely" rather than
// "run the agent on the prompt 'config' with debug logging". The check is on
// the first argument only: a subcommand is the first word of the line or it is
// not a subcommand.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "config":
			return runConfig(args[1:], stdout, stderr)
		case "sessions":
			return runSessions(args[1:], stdout, stderr)
		case "probe":
			return runProbe(args[1:], stdout, stderr)
		case "help":
			usage(stdout)
			return 0
		}
	}

	f, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "ultrac: %v\n\n", err)
		usage(stderr)
		return 2
	}
	if f.Help {
		usage(stdout)
		return 0
	}
	if f.Version {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	// RPC mode takes its prompts from the command stream, not argv: a bare
	// `ultrac --mode rpc` is a complete invocation (pi's resolveAppMode
	// treats the mode flag as the mode).
	if f.Mode == "rpc" {
		return runRPC(f, os.Stdin, stdout, stderr)
	}
	prompt := f.prompt()
	if prompt == "" && !f.Continue {
		// No TUI in this port (plan §2.2): a bare `ultrac` has nothing to
		// do, and saying so beats opening an interactive shell that does
		// not exist.
		usage(stdout)
		return 0
	}
	if prompt == "" && f.Continue {
		fmt.Fprintln(stderr, "ultrac: --continue needs a prompt to continue with.")
		fmt.Fprintln(stderr, "        Pass the next instruction, or drop the flag to start a new session.")
		return 2
	}
	return runPrint(f, stdout, stderr)
}

func usage(w io.Writer) {
	usageText(w, version.Get().Version,
		listOrNone(apiNames()),
		listOrNone(tool.Names()))
}

// apiNames lists the registered provider dialects for --help.
func apiNames() []string {
	apis := ai.Apis()
	out := make([]string, len(apis))
	for i, a := range apis {
		out[i] = string(a)
	}
	return out
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
		// The typed compaction section prints its keys, not a byte count:
		// the point of `ultrac config` is answering "what does this layer
		// actually say", and "3 bytes preserved" answers nothing.
		if l.Settings.Compaction != nil {
			fmt.Fprintf(stdout, "    (compaction section: %s)\n", formatCompaction(l.Settings.Compaction))
		}
		if len(l.Settings.Env) == 0 && len(l.Settings.Extra) == 0 && l.Settings.Compaction == nil {
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

	// The provider-config files (P4): where models.json and auth.json
	// live, what the user's providers declare, and where each one's
	// credential comes from — the question "why did my key not work" is
	// answered here or nowhere.
	printProviderConfigSection(stdout, stderr, cwd)

	if loadErr != nil {
		fmt.Fprintf(stderr, "\nwarning: %v\n", loadErr)
	}
	return 0
}

// formatCompaction renders the typed compaction section for `ultrac config`.
// Absent keys stay absent — the display must not imply a default the file
// never stated (the engine injects defaults at resolution time, plan D6).
func formatCompaction(c *config.CompactionSettings) string {
	var parts []string
	if c.Enabled != nil {
		parts = append(parts, fmt.Sprintf("enabled=%t", *c.Enabled))
	}
	if c.ReserveTokens != nil {
		parts = append(parts, fmt.Sprintf("reserveTokens=%d", *c.ReserveTokens))
	}
	if c.KeepRecentTokens != nil {
		parts = append(parts, fmt.Sprintf("keepRecentTokens=%d", *c.KeepRecentTokens))
	}
	if len(parts) == 0 {
		return "(empty object)"
	}
	return strings.Join(parts, " ")
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

// runSessions lists the sessions recorded for a directory under
// ~/.ultrac/sessions/ — the pi-shaped store the agent path writes (plan
// §6.1). The pre-P7 version listed the T0-era ~/.ultrac/projects/ store,
// which nothing writes any more.
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

	dir := session.SessionDirFor(cwd)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stdout, "no sessions for %s\n", abs)
			fmt.Fprintf(stdout, "(looked in %s)\n", dir)
			return 0
		}
		fmt.Fprintf(stderr, "ultrac sessions: %v\n", err)
		return 1
	}

	names := make([]string, 0, len(dirEntries))
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		fmt.Fprintf(stdout, "no sessions for %s\n", abs)
		fmt.Fprintf(stdout, "(looked in %s)\n", dir)
		return 0
	}
	sort.Strings(names)

	fmt.Fprintf(stdout, "%d session(s) in %s\n\n", len(names), dir)
	for _, name := range names {
		path := filepath.Join(dir, name)
		header, entries, err := session.ReadSessionFile(path)
		if err != nil {
			fmt.Fprintf(stdout, "  %-24s [unreadable: %v]\n", name, err)
			continue
		}
		fmt.Fprintf(stdout, "  %s  %4d entries  %s  %s\n",
			header.ID, len(entries), header.Timestamp, sessionTitle(entries))
	}
	return 0
}

// sessionTitle is the first user message's leading text, truncated — the
// same "what was this session about" line the T0 store's Title() gave.
// The rule itself lives in the session package (list.go) so the CLI and
// the RPC list_sessions command cannot drift apart.
func sessionTitle(entries []*session.SessionEntry) string {
	return session.SessionTitle(entries)
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
