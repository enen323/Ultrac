package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// agentFlags is the parsed command line for one agent run.
//
// Hand-rolled rather than cobra, and deliberately so at this size. The
// TypeScript CLI has 54 flags and 193 subcommands, which is why it needs a
// framework; this binary has 12, and the parsing rules that matter here —
// a repeated --add-dir, a `--flag=value` form, and a prompt that may contain
// anything at all including things that look like flags after `--` — are
// easier to get right explicitly than to configure.
type agentFlags struct {
	Print        bool
	OutputFormat string
	Model        string
	Permission   string
	AddDirs      []string
	Settings     string
	Version      bool
	Help         bool

	// Extensions beyond the eight T1 flags, each of which the agent is
	// unusable without in some ordinary situation.
	Prompt             string
	MaxTurns           int
	SystemPrompt       string
	AppendSystemPrompt string
	SkipPermissions    bool
	Debug              bool
	SessionID          string
	NoTranscript       bool
}

// outputFormats is the closed set --output-format accepts.
var outputFormats = []string{"text", "json", "stream-json"}

// parseFlags parses argv.
//
// Errors are returned rather than printed so the caller decides the exit code.
// An unknown flag is an error: silently ignoring one is how somebody's
// --permission-mode ends up not applying and the run does something they did
// not intend.
func parseFlags(args []string) (*agentFlags, error) {
	f := &agentFlags{
		OutputFormat: "text",
		Permission:   "",
		MaxTurns:     0,
	}

	i := 0
	rest := false
	for ; i < len(args); i++ {
		a := args[i]

		if rest {
			f.Prompt = joinPrompt(f.Prompt, a)
			continue
		}
		if a == "--" {
			rest = true
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			f.Prompt = joinPrompt(f.Prompt, a)
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
		case "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions":
			f.SkipPermissions = true
		case "--no-transcript":
			f.NoTranscript = true
		case "--output-format":
			v, err := value()
			if err != nil {
				return nil, err
			}
			if !contains(outputFormats, v) {
				return nil, fmt.Errorf("--output-format must be one of %s; got %q",
					strings.Join(outputFormats, ", "), v)
			}
			f.OutputFormat = v
		case "--model":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.Model = v
		case "--permission-mode":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.Permission = v
		case "--settings":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.Settings = v
		case "--add-dir":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.AddDirs = append(f.AddDirs, v)
		case "--max-turns":
			v, err := value()
			if err != nil {
				return nil, err
			}
			n, cerr := strconv.Atoi(v)
			if cerr != nil || n <= 0 {
				return nil, fmt.Errorf("--max-turns must be a positive integer; got %q", v)
			}
			f.MaxTurns = n
		case "--system-prompt":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.SystemPrompt = v
		case "--append-system-prompt":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.AppendSystemPrompt = v
		case "--session-id":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.SessionID = v
		default:
			return nil, fmt.Errorf("unknown flag %q", name)
		}
	}

	if f.SkipPermissions {
		// --dangerously-skip-permissions is a mode, not a separate switch, so
		// it is expressed as one. Two ways to say the same thing would drift.
		if f.Permission != "" && f.Permission != "bypassPermissions" {
			return nil, fmt.Errorf("--dangerously-skip-permissions conflicts with --permission-mode %s", f.Permission)
		}
		f.Permission = "bypassPermissions"
	}
	return f, nil
}

// splitFlag separates `--flag=value` into its parts.
func splitFlag(arg string) (name, value string, hasValue bool) {
	if i := strings.IndexByte(arg, '='); i > 0 {
		return arg[:i], arg[i+1:], true
	}
	return arg, "", false
}

func joinPrompt(existing, part string) string {
	if existing == "" {
		return part
	}
	return existing + " " + part
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
func usageText(w io.Writer, version, providers, tools string) {
	fmt.Fprintf(w, "ultrac %s — Go engine\n\n", version)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ultrac [flags] [prompt]       run the agent on a prompt")
	fmt.Fprintln(w, "  ultrac <command> [args]       run a diagnostic command")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  config              show the resolved settings stack and effective env")
	fmt.Fprintln(w, "  sessions [dir]      list transcripts for a directory (default: cwd)")
	fmt.Fprintln(w, "  ask <prompt>        stream one completion without tools")
	fmt.Fprintln(w, "  doctor              check the foundation end to end")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -p, --print                  run headless and exit (implied by a prompt)")
	fmt.Fprintln(w, "      --output-format FORMAT   text (default) | json | stream-json")
	fmt.Fprintln(w, "      --model MODEL            override the configured model")
	fmt.Fprintln(w, "      --permission-mode MODE   default | acceptEdits | plan | bypassPermissions | dontAsk")
	fmt.Fprintln(w, "      --add-dir DIR            grant access to another directory (repeatable)")
	fmt.Fprintln(w, "      --settings PATH          load an extra settings file at flag precedence")
	fmt.Fprintln(w, "      --max-turns N            stop after N assistant turns")
	fmt.Fprintln(w, "      --system-prompt TEXT     replace the built-in system prompt")
	fmt.Fprintln(w, "      --append-system-prompt TEXT  append to the system prompt")
	fmt.Fprintln(w, "      --session-id ID          use a specific session id")
	fmt.Fprintln(w, "      --no-transcript          do not write a transcript")
	fmt.Fprintln(w, "      --dangerously-skip-permissions")
	fmt.Fprintln(w, "                               approve every tool call that no deny rule forbids")
	fmt.Fprintln(w, "      --debug                  verbose logging on stderr")
	fmt.Fprintln(w, "  -v, --version                print the version")
	fmt.Fprintln(w, "  -h, --help                   print this help")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  provider kinds: %s\n", providers)
	fmt.Fprintf(w, "  built-in tools: %s\n", tools)
}
