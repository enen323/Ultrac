// rpc.go — the P6 RPC mode: pi's modes/rpc/rpc-mode.ts wired to the Go
// harness (docs/pi-core-harness-plan.md §3.7).
//
// # The shape
//
// runRPC assembles the same harness print.go uses (assemble.go), wraps it
// in an rpcHost — the implementation of rpc.Host, pi's AgentSessionRuntime
// role — and serves JSONL commands on stdin until the stream ends or a
// signal arrives. Responses and events both leave as JSONL on stdout
// through one transport.PiJSONWriter, so the two can never interleave into
// a broken line.
//
// # Lifecycle
//
//	stdin EOF        → exit 0 (pi: onInputEnd → shutdown, rpc-mode.ts:802)
//	SIGINT/SIGTERM   → cancel the run context → in-flight runs abort →
//	                   exit 130/143 (SIGHUP 129 off Windows)
//	anything else    → exit 1
//
// Unlike print mode there is no "the answer" to print and no exit code
// derived from the assistant message: the client asked for a stream, the
// stream carried the outcome. Diagnostics still go to stderr, so a client
// piping stdout into a JSONL parser never sees them.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/config"
	"ultrac/internal/rpc"
	"ultrac/internal/session"
	"ultrac/internal/transport"
)

// settleWait bounds how long a session swap (new_session) waits for an
// aborted run to settle before proceeding anyway.
const settleWait = 10 * time.Second

// rpcHost implements rpc.Host over one harness. It owns the live
// agent/session pair, which new_session swaps wholesale.
type rpcHost struct {
	h       *harness
	runCtx  context.Context
	persist bool // !--no-transcript

	mu    sync.Mutex
	agent *agent.Agent
	mgr   *session.SessionManager
}

// newRPCHost builds the host and its initial agent/session pair.
func newRPCHost(h *harness, runCtx context.Context) *rpcHost {
	host := &rpcHost{
		h:       h,
		runCtx:  runCtx,
		persist: !h.flags.NoTranscript,
	}
	host.agent = h.newAgent(h.mgr, h.seed, h.flags.Continue)
	host.mgr = h.mgr
	if !h.flags.Continue {
		// A resumed session already carries its metadata; a fresh one
		// needs the model and thinking level before the first message.
		h.recordSessionMeta(host.mgr)
	}
	return host
}

// -- rpc.Host -----------------------------------------------------------------

func (host *rpcHost) Agent() *agent.Agent {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.agent
}

func (host *rpcHost) Session() *session.SessionManager {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.mgr
}

func (host *rpcHost) RunContext() context.Context { return host.runCtx }

// Persist writes one completed message to the session file. It runs on the
// run's goroutine (the event pump), so it takes the lock only to read the
// current manager and never waits on a run.
func (host *rpcHost) Persist(msg ai.Message) {
	if !host.persist || !persistable(msg.Role) {
		return
	}
	host.mu.Lock()
	mgr := host.mgr
	host.mu.Unlock()
	if _, err := mgr.AppendMessage(msg); err != nil && host.h.flags.Debug {
		fmt.Fprintf(os.Stderr, "[debug] session append: %v\n", err)
	}
}

// NewSession replaces the agent and the session with a fresh pair
// (pi: agent-session-runtime.ts:226-256 — a whole new runtime, not a reset
// of the old one, so the new transcript starts with no system message and
// its first prompt re-declares the full sections).
func (host *rpcHost) NewSession(parentSession string) {
	// Abort the current run first — and deliberately without holding the
	// lock: the event pump takes the lock to persist, and WaitForIdle waits
	// for the pump to finish. Holding it here would deadlock the two.
	old := host.Agent()
	old.Abort()
	waitCtx, cancel := context.WithTimeout(context.Background(), settleWait)
	defer cancel()
	_ = old.WaitForIdle(waitCtx)

	var mgr *session.SessionManager
	if parentSession != "" {
		mgr = session.CreateSessionWithParent(host.h.cwd, parentSession)
	} else {
		mgr = session.CreateSession(host.h.cwd)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	// A session created mid-process still needs its metadata, whatever
	// flag the process started with (the startup skip is the resumed-file
	// case only).
	host.h.recordSessionMeta(mgr)
	host.mgr = mgr
	host.agent = host.h.newAgent(mgr, session.SessionContext{}, false)
}

// ListSessions summarizes the transcripts for the harness cwd — the
// host's knowledge is the cwd; the session package owns the layout.
func (host *rpcHost) ListSessions() ([]session.SessionSummary, error) {
	return session.ListSessions(host.h.cwd)
}

// ResumeSession swaps the live agent and session to the transcript at
// path: the same abort→swap shape as NewSession, but loading an existing
// file instead of creating one.
//
// This is the semantic twin of pi's switch_session
// (agent-session-runtime.ts:196-224: open the file, tear down the current
// runtime, apply a fresh one over the loaded manager). Two deliberate
// differences, both forced by what this port's resume can and cannot do:
//
//   - No cwd re-rooting. pi rebuilds the whole runtime around the
//     session's recorded cwd (and hard-errors when that directory is
//     gone, session-cwd.ts). Here the tools, the project settings layer
//     and the session directory were all bound to the harness cwd at
//     startup, so a resume keeps the process cwd; the summary's cwd field
//     (list_sessions) is what lets a client surface a mismatch instead of
//     silently running tools somewhere else.
//   - No session_start event. The event vocabulary this port emits is the
//     agent-level set; a session-level resume event would be a new wire
//     concept for a single client.
//
// The model and thinking level come from the file, the same fallback
// buildHarness applies on --continue: the session's last recorded model
// wins over the harness's current one (the conversation was held with
// it). The gate is set_model's own rule — the model must be one this
// process can select — because resolveModel does not fail on an unknown
// provider/id, and switching to a phantom would only surface as a failed
// request later; an unselectable name keeps the harness's working model
// and the client can set_model afterwards.
func (host *rpcHost) ResumeSession(path string) error {
	// Abort the current run first — and deliberately without holding the
	// lock, for the same deadlock reason NewSession documents.
	old := host.Agent()
	old.Abort()
	waitCtx, cancel := context.WithTimeout(context.Background(), settleWait)
	defer cancel()
	_ = old.WaitForIdle(waitCtx)

	mgr, err := session.OpenSession(path)
	if err != nil {
		return err
	}
	seed := mgr.BuildSessionContext()
	if seed.Model != nil {
		if m, err := resolveModel(host.h.snapshot, seed.Model.Provider+"/"+seed.Model.ModelID, host.h.flags.API, host.h.effective); err == nil &&
			containsModel(host.h.availableModels(), m) {
			host.h.model = m
		}
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	// No recordSessionMeta: the file already carries its own model_change
	// and thinking_level_change entries (it was created by a run that
	// wrote them), and appending fresh ones would rewrite history the
	// resume is supposed to restore — the same skip --continue makes.
	host.mgr = mgr
	host.agent = host.h.newAgent(mgr, seed, true)
	return nil
}

// SetModel validates the selection against the available models, applies
// it to the agent, and persists a model_change entry (pi:
// agent-session.ts:2502-2514). pi also re-checks the provider credential
// here; this port resolves credentials per request through the chain
// (assemble.go's h.credential — models.json apiKey → auth.json, then the
// settings env token), so a switch to a provider without a credential
// surfaces at the next request as an endpoint error, not here.
func (host *rpcHost) SetModel(provider, modelID string) (ai.Model, error) {
	var found ai.Model
	ok := false
	for _, m := range host.h.availableModels() {
		if string(m.Provider) == provider && m.ID == modelID {
			found, ok = m, true
			break
		}
	}
	if !ok {
		// Wire literal must match pi byte-for-byte: rpc-mode.ts:474 sends
		// `Model not found: ${provider}/${modelId}` and docs/rpc.md:78
		// shows the same capitalized form in a response example — the C4
		// field-by-field comparison (and the P7 double-run diff) depends
		// on it. ST1005 disagrees; protocol fidelity wins.
		//lint:ignore ST1005 wire literal matches pi
		return ai.Model{}, fmt.Errorf("Model not found: %s/%s", provider, modelID)
	}
	host.Agent().SetModel(found)
	// The harness carries the model too, so a later new_session records
	// the switched one rather than the startup one.
	host.h.model = found

	host.mu.Lock()
	mgr := host.mgr
	host.mu.Unlock()
	if _, err := mgr.AppendModelChange(string(found.Provider), found.ID); err != nil && host.h.flags.Debug {
		fmt.Fprintf(os.Stderr, "[debug] session append: %v\n", err)
	}
	return found, nil
}

func (host *rpcHost) AvailableModels() []ai.Model { return host.h.availableModels() }

// SetThinkingLevel applies the level (the server already clamped it against
// the model's supported levels) and persists the change.
func (host *rpcHost) SetThinkingLevel(level agent.ThinkingLevel) {
	host.Agent().SetThinkingLevel(level)
	host.mu.Lock()
	mgr := host.mgr
	host.mu.Unlock()
	if _, err := mgr.AppendThinkingLevelChange(string(level)); err != nil && host.h.flags.Debug {
		fmt.Fprintf(os.Stderr, "[debug] session append: %v\n", err)
	}
}

func (host *rpcHost) ThinkingLevels() []ai.ModelThinkingLevel {
	return ai.SupportedThinkingLevels(host.Agent().State().Model)
}

func (host *rpcHost) SetSessionName(name string) error {
	host.mu.Lock()
	mgr := host.mgr
	host.mu.Unlock()
	_, err := mgr.AppendSessionInfo(name)
	return err
}

func (host *rpcHost) BuildPrompt(text string, images []rpc.ImageContent) (rpc.PromptInput, error) {
	return host.h.buildPrompt(host.Agent(), text, images)
}

// Compact runs a manual compaction over the live pair (pi: rpc-mode.ts:533
// → agent-session.compact). The executor owns the abort-first ordering; the
// run context is what makes a termination signal cancel an in-flight
// summarization instead of delaying exit.
func (host *rpcHost) Compact(customInstructions string) (*rpc.CompactResult, error) {
	return host.h.compactor.Compact(host.runCtx, customInstructions)
}

// SetAutoCompaction persists the gate to the user settings layer — pi's
// setAutoCompactionEnabled writes global settings the same way (plan D6) —
// then flips the live executor. A failed write leaves the live gate
// untouched: the file and the running session must not disagree about
// whether auto-compaction is on.
func (host *rpcHost) SetAutoCompaction(enabled bool) error {
	err := host.h.resolver.Update(context.Background(), config.SourceUser, host.h.cwd, func(s *config.Settings) error {
		section := s.Compaction
		if section == nil {
			section = &config.CompactionSettings{}
		}
		// Copy-then-assign: Update hands us the file's own contents, and
		// writing through the shared pointer would alias whatever the
		// layer cache still holds.
		next := *section
		next.Enabled = &enabled
		s.Compaction = &next
		return nil
	})
	if err != nil {
		return err
	}
	// Two writes, one intent: the live executor flips now, and the
	// harness's resolved copy flips too — a new_session rebuilds the
	// executor FROM that copy, so updating only the live one would silently
	// revert the gate on the next session swap.
	host.h.compaction.Enabled = enabled
	host.h.compactor.SetEnabled(enabled)
	return nil
}

func (host *rpcHost) IsCompacting() bool { return host.h.compactor.IsCompacting() }

func (host *rpcHost) AutoCompactionEnabled() bool { return host.h.compactor.AutoEnabled() }

// Fork moves the session leaf to branch from entryID (plan D7: pi's
// navigateTree without the summary, agent-session.ts:4113-4128).
//
// The ordering mirrors every other session-mutating command here: abort
// any active run and wait for it to settle before touching the tree — a
// leaf that moves under a live run would have the run's next append land
// on the abandoned branch. pi gets the same guarantee by tearing the
// runtime down (agent-session-runtime.ts:289-291).
//
// The re-seed is the authority fix in the other direction: the session's
// leaf moved, so the agent's transcript must be the fork point's
// projection, not the history being left behind — otherwise the next
// prompt replays the abandoned branch (pi's _refreshFinalizedContext,
// agent-session.ts:4162). ReplaceMessages is the P-C2 D2 primitive, used
// here for navigation instead of compression.
func (host *rpcHost) Fork(entryID string) (string, string, error) {
	old := host.Agent()
	old.Abort()
	waitCtx, cancel := context.WithTimeout(context.Background(), settleWait)
	defer cancel()
	_ = old.WaitForIdle(waitCtx)

	mgr := host.Session()
	move, err := session.PlanFork(mgr.TreeEntries(), entryID, mgr.GetLeafID())
	if err != nil {
		return "", "", err
	}
	if move.NoOp {
		// Already there: nothing moves, and pi returns no editor text.
		return mgr.GetLeafID(), "", nil
	}
	if move.LeafID == "" {
		mgr.ResetLeaf()
	} else if err := mgr.Branch(move.LeafID); err != nil {
		return "", "", err
	}
	host.Agent().ReplaceMessages(mgr.BuildSessionContext().Messages)
	return move.LeafID, move.EditorText, nil
}

// GetForkMessages lists the fork selector's candidates — every user
// message with text, in file order (pi's getUserMessagesForForking,
// agent-session.ts:4186-4201).
func (host *rpcHost) GetForkMessages() ([]session.ForkMessage, error) {
	return session.UserForkMessages(host.Session().TreeEntries()), nil
}

// GetTree returns the whole session forest plus the current leaf (pi's
// getTree behind rpc-mode.ts:649-651, plan D8's shape).
func (host *rpcHost) GetTree() ([]*session.TreeNode, string, error) {
	mgr := host.Session()
	tree, err := session.BuildTree(mgr.TreeEntries())
	if err != nil {
		return nil, "", err
	}
	return tree, mgr.GetLeafID(), nil
}

// Clone extracts the current root→leaf path into a new session file and
// returns its path (pi's clone → createBranchedSession,
// rpc-mode.ts:619-629).
//
// The live session is deliberately not swapped: pi rebinds its runtime to
// the extracted file here, while this port's session switch is
// resume_session (the switch_session twin, types.go) — so the client gets
// the path and resumes it, and the conversation in flight keeps its
// transcript. No abort either: the extraction reads the tree and writes a
// snapshot, and an append racing it lands in the source file as usual —
// the worst case is a clone that misses the very last entry, which the
// client sees on the next listing.
func (host *rpcHost) Clone() (string, error) {
	mgr := host.Session()
	leaf := mgr.GetLeafID()
	if leaf == "" {
		// Wire literal must match pi byte-for-byte (rpc-mode.ts:622).
		//lint:ignore ST1005 wire literal matches pi
		return "", fmt.Errorf("Cannot clone session: no current entry selected")
	}
	return mgr.CreateBranchedSession(leaf)
}

// -- entry point -----------------------------------------------------------------

// runRPC is the CLI entry for --mode rpc.
func runRPC(f *agentFlags, stdin io.Reader, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "ultrac: %v\n", err)
		return 1
	}
	cwd, code := resolveCWD(f, cwd, stderr)
	if code != 0 {
		return code
	}

	h, code := buildHarness(f, cwd, stderr, nil)
	if code != 0 {
		return code
	}

	ctx, stopWatch, firedSignal := printSignalContext()
	defer stopWatch()

	writer := transport.NewPiJSONWriter(stdout)
	// The compaction executor emits through the same writer the event pump
	// uses, so compaction events interleave with the run's events in wire
	// order instead of racing them (plan D4). Set before the host builds
	// its first agent — the hook closure captures this sink.
	h.emitEvent = writer.WriteEvent
	host := newRPCHost(h, ctx)
	server := rpc.NewServer(host, writer)
	defer server.Close()

	serveErr := server.Serve(ctx, stdin)
	if serveErr != nil && errors.Is(serveErr, context.Canceled) {
		// The signal handler cancelled the run context; the in-flight run
		// has already been given its settle window inside Serve. The exit
		// code names the signal, same convention as print mode.
		if sig := firedSignal(); sig != "" {
			fmt.Fprintf(stderr, "\nultrac: %s\n", sig)
			return signalExitCode(sig)
		}
		return 143
	}
	if serveErr != nil {
		fmt.Fprintf(stderr, "ultrac: rpc: %v\n", serveErr)
		return 1
	}
	if f.Debug {
		fmt.Fprintf(stderr, "[debug] session: %s\n", host.Session().SessionFile())
	}
	return 0
}
