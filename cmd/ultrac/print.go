package main

// print.go — the P5 print/json mode: pi's modes/print-mode.ts, wired to the
// Go harness (docs/pi-core-harness-plan.md §3.6).
//
// # The shape
//
// One invocation: assemble the harness (settings → session → model → tools
// → prompt sections, assemble.go) → build the agent → run the prompt →
// print the answer (text mode) or the event stream (json mode) → exit.
// The RPC mode (rpc.go) assembles through the same chain and diverges only
// after it.
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

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/transport"
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
	cwd, code := resolveCWD(f, cwd, stderr)
	if code != 0 {
		return code
	}
	return (&printSession{flags: f, stdout: stdout, stderr: stderr, cwd: cwd}).run()
}

// resolveCWD applies --cwd to the process working directory. It is shared
// by the print and RPC modes because both resolve paths, sessions and the
// project settings layer against the same directory.
//
// --cwd is not decoration: it picks the directory the tools resolve paths
// against, the session the transcript belongs to, and the project settings
// layer that is read. A bad value is exit code 2 (a bad invocation), the
// same class as an unknown flag.
func resolveCWD(f *agentFlags, cwd string, stderr io.Writer) (string, int) {
	if f.CWD == "" {
		return cwd, 0
	}
	abs, err := filepath.Abs(f.CWD)
	if err != nil {
		fmt.Fprintf(stderr, "ultrac: --cwd: %v\n", err)
		return "", 2
	}
	info, serr := os.Stat(abs)
	if serr != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "ultrac: --cwd %s is not a directory\n", f.CWD)
		return "", 2
	}
	return abs, 0
}

func (ps *printSession) run() int {
	f := ps.flags

	// -- 1-4. The shared assembly ------------------------------------------
	//
	// Settings, session, model, credential and tools — the same chain the
	// RPC mode assembles through (assemble.go). A non-zero code means the
	// reason is already on stderr.
	h, code := buildHarness(f, ps.cwd, ps.stderr, ps.streamFn)
	if code != 0 {
		return code
	}
	mgr := h.mgr
	if !f.Continue {
		// A resumed session already carries the model and thinking level
		// in its file; a fresh one needs them before the first message.
		h.recordSessionMeta(mgr)
	}

	ctx, stopWatch, firedSignal := printSignalContext()
	defer stopWatch()

	// -- 5. Agent -------------------------------------------------------------
	a := h.newAgent(mgr, f.Continue)

	// -- 6. Output surfaces ----------------------------------------------------
	jsonMode := f.Mode == "json"
	var writer *transport.PiJSONWriter
	if jsonMode {
		writer = transport.NewPiJSONWriter(ps.stdout)
		// The header opens the stream, before any event (pi:
		// print-mode.ts:122-127).
		hdr := mgr.Header()
		if err := writer.WriteHeader(&hdr); err != nil {
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
	//
	// The prompt is assembled per run: the sections patch diffed against
	// the transcript's current sections, then the user message (pi:
	// agent-session.ts:1742-1754).
	in, err := h.buildPrompt(a, f.prompt(), nil)
	if err != nil {
		fmt.Fprintf(ps.stderr, "ultrac: %v\n", err)
		return 1
	}
	prompts := []ai.Message{in.User}
	if in.Patch != nil {
		prompts = append([]ai.Message{*in.Patch}, prompts...)
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
