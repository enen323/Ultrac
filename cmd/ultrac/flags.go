package main

import (
	"fmt"
	"io"
	"strings"
)

// agentFlags is the parsed command line for one run — the pi flag surface
// (docs/pi-core-harness-plan.md §6.3), not the old Claude-Code-shaped one.
//
// Hand-rolled rather than cobra, and deliberately so at this size: the
// parsing rules that matter — a `--flag=value` form, a prompt that may
// contain anything at all including things that look like flags after `--` —
// are easier to get right explicitly than to configure.
//
// What is gone and why: --permission-mode, --dangerously-skip-permissions
// and --add-dir belonged to the permission layer, which this port does not
// have (pi ships no permission system; plan §0.2). --output-format is
// replaced by --mode, whose "json" value is the event stream rather than the
// old stream-json dialect.
type agentFlags struct {
	// Print is the explicit headless switch. A prompt argument implies it
	// too (pi: resolveAppMode treats a non-TTY or a prompt as print).
	Print bool
	// Mode is the output format: "text" (default) or "json".
	Mode string
	// Model is the "provider/id" model selection.
	Model string
	// API overrides the provider dialect (anthropic-messages,
	// openai-completions). Empty means the settings convention.
	API string
	// CWD overrides the working directory the agent runs in.
	CWD string
	// Continue resumes the most recent session for the working directory.
	Continue bool
	// NoTranscript skips writing the session file.
	NoTranscript bool
	// Debug enables verbose logging on stderr.
	Debug bool

	Version bool
	Help    bool

	// promptParts collects the positional arguments; prompt() joins them.
	promptParts []string
}

// modes is the closed set --mode accepts. "rpc" is pi's third value and
// lands with P6; rejecting it here rather than silently running text mode is
// what keeps the flag honest.
var modes = []string{"text", "json"}

// parseFlags parses argv.
//
// Errors are returned rather than printed so the caller decides the exit
// code. An unknown flag is an error: silently ignoring one is how somebody's
// --model ends up not applying and the run bills the wrong endpoint.
func parseFlags(args []string) (*agentFlags, error) {
	f := &agentFlags{Mode: "text"}

	i := 0
	rest := false
	for ; i < len(args); i++ {
		a := args[i]

		if rest {
			f.promptParts = append(f.promptParts, a)
			continue
		}
		if a == "--" {
			rest = true
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			f.promptParts = append(f.promptParts, a)
			continue
		}

		name, inline, hasInline := splitFlag(a)
		// value consumes the next argument when the flag was not written as
		// --flag=value.
		value := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", name)
			}
			i++
			return args[i], nil
		}

		switch name {
		case "--print", "-p":
			f.Print = true
		case "--version", "-v":
			f.Version = true
		case "--help", "-h":
			f.Help = true
		case "--debug":
			f.Debug = true
		case "--no-transcript":
			f.NoTranscript = true
		case "--mode":
			v, err := value()
			if err != nil {
				return nil, err
			}
			if !contains(modes, v) {
				return nil, fmt.Errorf("--mode must be one of %s; got %q (rpc arrives with the RPC mode)",
					strings.Join(modes, ", "), v)
			}
			f.Mode = v
		case "--model":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.Model = v
		case "--api":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.API = v
		case "--cwd":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.CWD = v
		case "--continue", "-c":
			f.Continue = true
		default:
			return nil, fmt.Errorf("unknown flag %q", name)
		}
	}
	return f, nil
}

// prompt is the positional argument(s), joined with spaces. It lives outside
// parseFlags' switch so the parser stays a parser; the run layer decides
// whether an empty prompt is an error.
func (f *agentFlags) prompt() string {
	return strings.TrimSpace(strings.Join(f.promptParts, " "))
}

// setPrompt replaces the positional arguments with one prompt. It exists for
// tests and future callers that build flags programmatically; the parser
// appends to promptParts instead.
func (f *agentFlags) setPrompt(text string) {
	f.promptParts = []string{text}
}

// splitFlag separates `--flag=value` into its parts.
func splitFlag(arg string) (name, value string, hasValue bool) {
	if i := strings.IndexByte(arg, '='); i > 0 {
		return arg[:i], arg[i+1:], true
	}
	return arg, "", false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// usageText is the --help output.
func usageText(w io.Writer, version, apis, tools string) {
	fmt.Fprintf(w, "ultrac %s — Go engine (pi harness port)\n\n", version)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ultrac [flags] [prompt]       run the agent on a prompt and exit")
	fmt.Fprintln(w, "  ultrac <command> [args]       run a diagnostic command")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  config              show the resolved settings stack and effective env")
	fmt.Fprintln(w, "  sessions [dir]      list transcripts for a directory (default: cwd)")
	fmt.Fprintln(w, "  ask <prompt>        stream one completion without tools")
	fmt.Fprintln(w, "  doctor              check the foundation end to end")
	fmt.Fprintln(w, "  probe               stream one real request and print the event sequence")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -p, --print                  non-interactive: process the prompt and exit")
	fmt.Fprintln(w, "      --mode MODE              output format: text (default) | json")
	fmt.Fprintln(w, "      --model PROVIDER/ID      model selection (e.g. deepseek/deepseek-chat)")
	fmt.Fprintln(w, "      --api DIALECT            override the provider dialect (anthropic-messages | openai-completions)")
	fmt.Fprintln(w, "      --cwd DIR                run in another directory")
	fmt.Fprintln(w, "  -c, --continue               continue the most recent session for the directory")
	fmt.Fprintln(w, "      --no-transcript          do not write a session file")
	fmt.Fprintln(w, "      --debug                  verbose logging on stderr")
	fmt.Fprintln(w, "  -v, --version                print the version")
	fmt.Fprintln(w, "  -h, --help                   print this help")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  provider dialects: %s\n", apis)
	fmt.Fprintf(w, "  built-in tools: %s\n", tools)
}
