package rpc

// rpc_test.go — the P6 dispatch acceptance, end to end against a real
// agent driven by a scripted stream function.
//
// The pattern is P2/P5's: no request leaves the process. The wiring under
// test is the protocol — response shapes, dispositions, ordering, the
// unknown-command guard — and a scripted provider makes it deterministic.
// The real endpoint is exercised by the acceptance driver (bin/), not here.
//
// The Host under test is a minimal one: it serves scripts in call order
// and keeps its state in an in-memory session. The sections patch (the
// system-prompt diff) is the harness's job and is tested in cmd/ultrac;
// here BuildPrompt returns the user message alone, which is all the
// protocol layer looks at.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/config"
	"ultrac/internal/session"
	"ultrac/internal/transport"
)

// ---------------------------------------------------------------------------
// The test host
// ---------------------------------------------------------------------------

// testHost implements Host over a real agent and an in-memory session.
type testHost struct {
	mu          sync.Mutex
	cwd         string
	agent       *agent.Agent
	session     *session.SessionManager
	models      []ai.Model
	scripts     [][]ai.AssistantEvent
	calls       int32 // atomic: stream invocations, read from the test goroutine
	agentEnds   int32 // atomic: completed runs, counted by a host subscription
	transcripts []ai.TranscriptContext
	runCtx      context.Context

	// listSessions / listErr back the ListSessions method: canned data for
	// the protocol-shape tests, an error for the failure path. The real
	// listing lives in the session package and is tested there.
	listSessions []session.SessionSummary
	listErr      error

	// compactResult / compactErr back the Compact method: canned data for
	// the protocol-shape tests, an error for the failure path. The real
	// compaction (plan → summarize → append → swap) is cmd/ultrac's, tested
	// there against the real harness; what is under test here is the
	// command's wire contract.
	compactResult *CompactResult
	compactErr    error
	compactCalls  []string // customInstructions per call, in order

	// autoCompaction is the gate: SetAutoCompaction records and (unless
	// setAutoErr fires) applies it, get_state reports it back.
	autoCompaction bool
	setAutoErr     error
	setAutoCalls   []bool
	compacting     bool

	// addResult / addErr back the AddCustomModel method: canned data for
	// the protocol-shape tests, an error for the write-failure path. The
	// real write (auth.json + models.json + reload) is cmd/ultrac's, tested
	// there against the real harness; what is under test here is the
	// command's wire contract and its protocol-level validation.
	addResult   *AddCustomModelResult
	addErr      error
	addCalls    []CustomModelSpec // spec as received, in order
	addValidate func(CustomModelSpec) error
}

// AddCustomModel serves the canned result/error and records the spec it was
// given, so the protocol tests can assert both the response shape and that
// the command's default filling and field pass-through reached the host
// verbatim. addValidate, when set, is consulted before the canned answer so
// a test can pin exactly which specs are expected to reach the host.
func (h *testHost) AddCustomModel(spec CustomModelSpec) (AddCustomModelResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.addCalls = append(h.addCalls, spec)
	if h.addValidate != nil {
		if err := h.addValidate(spec); err != nil {
			return AddCustomModelResult{}, err
		}
	}
	if h.addErr != nil {
		return AddCustomModelResult{}, h.addErr
	}
	return *h.addResult, nil
}

func newTestHost(t *testing.T, scripts ...[]ai.AssistantEvent) *testHost {
	t.Helper()
	modelA := ai.Model{ID: "model-a", Provider: "test", API: "anthropic-messages"}
	// model-b is a reasoning model whose map marks minimal/medium
	// unsupported (nil) and omits xhigh/max entirely: the pi support rule
	// then yields exactly [off, low, high].
	none, low, high := "none", "low", "high"
	modelB := ai.Model{
		ID:        "model-b",
		Provider:  "test",
		API:       "anthropic-messages",
		Reasoning: true,
		ThinkingLevelMap: ai.ThinkingLevelMap{
			ai.ModelThinkOff:     &none,
			ai.ModelThinkMinimal: nil,
			ai.ModelThinkLow:     &low,
			ai.ModelThinkMedium:  nil,
			ai.ModelThinkHigh:    &high,
		},
	}
	h := &testHost{
		cwd:     t.TempDir(),
		models:  []ai.Model{modelA, modelB},
		scripts: scripts,
		runCtx:  context.Background(),
	}
	h.reset(modelA)
	return h
}

// reset swaps in a fresh session and agent, keeping the given model.
func (h *testHost) reset(model ai.Model) {
	h.session = session.InMemorySession(h.cwd)
	m := model
	h.agent = agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentInitialState{Model: &m},
		StreamFn:     h.streamFn,
		SessionID:    h.session.SessionID(),
	})
	// A second subscription counts completed runs. It exists for settle():
	// a run can finish before the test looks at it, and "a provider call
	// happened" is then already true — the count of finished runs is the
	// signal that cannot be consumed by a fast run.
	h.agent.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if _, ok := ev.(agent.EvAgentEnd); ok {
			atomic.AddInt32(&h.agentEnds, 1)
		}
		return nil
	})
}

// rebuildAgent replaces the agent with one using the given stream function,
// keeping the current model and session. The gated tests need a held-open
// stream, and StreamFn is fixed at construction.
func (h *testHost) rebuildAgent(streamFn agent.StreamFn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.agent.State().Model
	h.agent = agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentInitialState{Model: &m},
		StreamFn:     streamFn,
		SessionID:    h.session.SessionID(),
	})
}

// streamFn serves one preset script per call, in order. Past the end of the
// script list it fails the call loudly rather than pretending success.
func (h *testHost) streamFn(_ context.Context, model ai.Model, tc ai.TranscriptContext, _ ai.StreamOptions) ai.Stream {
	idx := h.noteCall(tc)
	var script []ai.AssistantEvent
	if idx < len(h.scripts) {
		script = h.scripts[idx]
	}
	if script == nil {
		return ai.StreamOf(errorScript("testHost: no script for this call")...)
	}
	return ai.StreamOf(script...)
}

// noteCall records one provider invocation and returns its index. Both the
// script-serving and the gated stream functions go through it, so the tests
// can wait on "the loop reached the provider" whichever stream is in play.
func (h *testHost) noteCall(tc ai.TranscriptContext) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx := int(atomic.LoadInt32(&h.calls))
	atomic.AddInt32(&h.calls, 1)
	h.transcripts = append(h.transcripts, tc)
	return idx
}

func (h *testHost) Agent() *agent.Agent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agent
}

func (h *testHost) Session() *session.SessionManager {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.session
}

func (h *testHost) RunContext() context.Context { return h.runCtx }

func (h *testHost) Persist(msg ai.Message) {
	if !persistableTestRole(msg.Role) {
		return
	}
	h.mu.Lock()
	mgr := h.session
	h.mu.Unlock()
	_, _ = mgr.AppendMessage(msg)
}

func persistableTestRole(r ai.Role) bool {
	return r == ai.RoleSystem || r == ai.RoleUser || r == ai.RoleAssistant || r == ai.RoleToolResult
}

func (h *testHost) NewSession(parentSession string) {
	old := h.Agent()
	old.Abort()
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = old.WaitForIdle(waitCtx)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reset(h.agent.State().Model)
	_ = parentSession // the in-memory session has no header to carry it
}

func (h *testHost) SetModel(provider, modelID string) (ai.Model, error) {
	for _, m := range h.models {
		if string(m.Provider) == provider && m.ID == modelID {
			h.Agent().SetModel(m)
			h.mu.Lock()
			mgr := h.session
			h.mu.Unlock()
			_, _ = mgr.AppendModelChange(string(m.Provider), m.ID)
			return m, nil
		}
	}
	return ai.Model{}, &modelNotFoundError{provider: provider, modelID: modelID}
}

type modelNotFoundError struct{ provider, modelID string }

func (e *modelNotFoundError) Error() string {
	return "Model not found: " + e.provider + "/" + e.modelID
}

func (h *testHost) AvailableModels() []ai.Model { return h.models }

func (h *testHost) SetThinkingLevel(level agent.ThinkingLevel) {
	h.Agent().SetThinkingLevel(level)
	h.mu.Lock()
	mgr := h.session
	h.mu.Unlock()
	_, _ = mgr.AppendThinkingLevelChange(string(level))
}

func (h *testHost) ThinkingLevels() []ai.ModelThinkingLevel {
	return ai.SupportedThinkingLevels(h.Agent().State().Model)
}

func (h *testHost) SetSessionName(name string) error {
	h.mu.Lock()
	mgr := h.session
	h.mu.Unlock()
	_, err := mgr.AppendSessionInfo(name)
	return err
}

// BuildPrompt serves the user message alone — the harness's sections patch
// is cmd/ultrac's job (tested there); the protocol layer looks only at the
// user message.
func (h *testHost) BuildPrompt(text string, images []ImageContent) (PromptInput, error) {
	blocks := []ai.ContentBlock{ai.TextBlock(text)}
	for _, img := range images {
		blocks = append(blocks, ai.ImageBlock(img.Data, img.MimeType))
	}
	return PromptInput{User: ai.Message{
		Role:        ai.RoleUser,
		Content:     blocks,
		TimestampMS: time.Now().UnixMilli(),
	}}, nil
}

// Compact serves the canned result/error and records the instructions it was
// given, so the protocol tests can assert both the response shape and that
// the custom focus reached the host.
func (h *testHost) Compact(customInstructions string) (*CompactResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.compactCalls = append(h.compactCalls, customInstructions)
	return h.compactResult, h.compactErr
}

// SetAutoCompaction records the gate and applies it unless the canned error
// fires — in which case the live gate must stay untouched.
func (h *testHost) SetAutoCompaction(enabled bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.setAutoErr != nil {
		return h.setAutoErr
	}
	h.setAutoCalls = append(h.setAutoCalls, enabled)
	h.autoCompaction = enabled
	return nil
}

func (h *testHost) IsCompacting() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.compacting
}

func (h *testHost) AutoCompactionEnabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.autoCompaction
}

// ListSessions serves the canned summaries, mirroring what the real host
// (cmd/ultrac's rpcHost) delegates to the session package.
func (h *testHost) ListSessions() ([]session.SessionSummary, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listSessions, h.listErr
}

// ResumeSession loads a real transcript file and swaps the agent over to
// it — the protocol half of what the real host does, minus the model and
// thinking-level restoration (tested in cmd/ultrac against the harness).
func (h *testHost) ResumeSession(path string) error {
	old := h.Agent()
	old.Abort()
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = old.WaitForIdle(waitCtx)

	mgr, err := session.OpenSession(path)
	if err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.session = mgr
	m := h.agent.State().Model
	h.agent = agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentInitialState{
			Model:    &m,
			Messages: mgr.BuildSessionContext().Messages,
		},
		StreamFn:  h.streamFn,
		SessionID: mgr.SessionID(),
	})
	// The settled-run counter rides along with every agent this host
	// builds (reset does the same), so settle() keeps working after a swap.
	h.agent.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if _, ok := ev.(agent.EvAgentEnd); ok {
			atomic.AddInt32(&h.agentEnds, 1)
		}
		return nil
	})
	return nil
}

// Fork applies pi's navigateTree decision to the live session and re-seeds
// the agent from the fork-point projection — the real host's shape
// (cmd/ultrac's rpcHost.Fork) at protocol scale. The abort wait is the one
// piece skipped: no protocol test forks mid-run (serve settles a prompt
// batch before returning).
func (h *testHost) Fork(entryID string) (leafID, editorText string, err error) {
	mgr := h.Session()
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
	// D2: the session is authoritative — the agent's transcript becomes the
	// fork point's projection, not the history being left behind, so the
	// next prompt cannot replay the abandoned branch.
	h.Agent().ReplaceMessages(mgr.BuildSessionContext().Messages)
	return move.LeafID, move.EditorText, nil
}

// GetForkMessages lists the selector's candidates straight from the session
// (pi's getUserMessagesForForking) — the real host delegates the same way.
func (h *testHost) GetForkMessages() ([]session.ForkMessage, error) {
	return session.UserForkMessages(h.Session().TreeEntries()), nil
}

// GetTree returns the forest plus the leaf, straight from the session (pi's
// getTree) — again the real host's delegation.
func (h *testHost) GetTree() ([]*session.TreeNode, string, error) {
	mgr := h.Session()
	tree, err := session.BuildTree(mgr.TreeEntries())
	if err != nil {
		return nil, "", err
	}
	return tree, mgr.GetLeafID(), nil
}

// Clone delegates to the real extraction (CreateBranchedSession): the
// protocol half is just the path, and the extraction is the session
// package's, tested there. The leaf check comes first because it is the
// client-visible shape: pi's own error text for the no-leaf case
// (rpc-mode.ts:622), before the extraction's no-file refusal.
func (h *testHost) Clone() (string, error) {
	mgr := h.Session()
	leaf := mgr.GetLeafID()
	if leaf == "" {
		// The wire literal matches pi byte-for-byte (rpc-mode.ts:622).
		// staticcheck skips ST1005 in test files, so no directive here.
		return "", errors.New("Cannot clone session: no current entry selected")
	}
	return mgr.CreateBranchedSession(leaf)
}

// useFileSession swaps in a file-backed session with one user/assistant
// exchange per pair, so the clone tests run against a real transcript —
// the in-memory default has no file to branch. The config home is
// redirected so no test touches ~/.ultrac, the same recipe as
// writeSessionFile.
func (h *testHost) useFileSession(t *testing.T, pairs ...[2]string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".ultrac")
	t.Setenv(config.EnvConfigDir, home)
	m := session.CreateSession(h.cwd)
	for _, p := range pairs {
		now := time.Now().UnixMilli()
		for _, msg := range []ai.Message{
			{Role: ai.RoleUser, Content: []ai.ContentBlock{ai.TextBlock(p[0])}, TimestampMS: now},
			{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock(p[1])}, TimestampMS: now},
		} {
			if _, err := m.AppendMessage(msg); err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.session = m
}

// ---------------------------------------------------------------------------
// Scripts
// ---------------------------------------------------------------------------

// textScript builds a plain text response. Every update event carries the
// partial-so-far, the way the real adapters do — the loop reads the partial
// off the event, so a script without one is not a valid script.
func textScript(text string) []ai.AssistantEvent {
	partial := func(t string) ai.Message {
		m := ai.Message{Role: ai.RoleAssistant}
		if t != "" {
			m.Content = []ai.ContentBlock{ai.TextBlock(t)}
		}
		return m
	}
	final := ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.ContentBlock{ai.TextBlock(text)},
		API:        "anthropic-messages",
		Provider:   "test",
		Model:      "model-a",
		StopReason: ai.StopStop,
		Usage:      &ai.Usage{Input: 10, Output: 4, TotalTokens: 14},
	}
	return []ai.AssistantEvent{
		ai.EvStart{Partial: partial("")},
		ai.EvTextStart{ContentIndex: 0, Partial: partial("")},
		ai.EvTextDelta{ContentIndex: 0, Delta: text, Partial: partial(text)},
		ai.EvTextEnd{ContentIndex: 0, Content: text, Partial: partial(text)},
		ai.EvDone{Reason: ai.StopStop, Message: final},
	}
}

func errorScript(msg string) []ai.AssistantEvent {
	m := ai.Message{
		Role:         ai.RoleAssistant,
		API:          "anthropic-messages",
		Provider:     "test",
		Model:        "model-a",
		StopReason:   ai.StopError,
		ErrorMessage: msg,
		Usage:        &ai.Usage{},
	}
	return []ai.AssistantEvent{
		ai.EvStart{Partial: m},
		ai.EvError{Reason: ai.StopError, Error: m},
	}
}

// gatedStreamFn returns a stream function whose stream stays open until the
// returned release fires or the run's context is cancelled: the model
// "thinks" for as long as the test needs it to, which is what makes the
// streaming-state commands testable. release is a sync.Once so a test may
// fire it explicitly and still defer it.
//
// The context arm mirrors the real adapters: a cancelled request ends the
// stream with an aborted error, because ai.Stream consumption is not
// context-aware — ending the stream is the provider's job (the same shape
// P2's abortedStream fakes).
func gatedStreamFn(h *testHost, text string) (agent.StreamFn, func()) {
	var once sync.Once
	gate := make(chan struct{})
	fn := func(ctx context.Context, _ ai.Model, tc ai.TranscriptContext, _ ai.StreamOptions) ai.Stream {
		h.noteCall(tc)
		s := ai.NewEventStream()
		go func() {
			select {
			case <-gate:
			case <-ctx.Done():
				for _, ev := range abortedScript() {
					s.Push(ev)
				}
				s.End()
				return
			}
			for _, ev := range textScript(text) {
				s.Push(ev)
			}
			s.End()
		}()
		return s
	}
	return fn, func() { once.Do(func() { close(gate) }) }
}

// abortedScript is a terminal aborted-error stream: what a provider emits
// when the request is cancelled mid-flight.
func abortedScript() []ai.AssistantEvent {
	m := ai.Message{
		Role:       ai.RoleAssistant,
		API:        "anthropic-messages",
		Provider:   "test",
		Model:      "model-a",
		StopReason: ai.StopAborted,
		Usage:      &ai.Usage{},
	}
	return []ai.AssistantEvent{
		ai.EvStart{Partial: m},
		ai.EvError{Reason: ai.StopAborted, Error: m},
	}
}

// ---------------------------------------------------------------------------
// Drivers
// ---------------------------------------------------------------------------

// serve runs a batch of commands through a server over the host and returns
// the parsed stdout lines in order. When the batch contains a prompt, it
// waits for that run to settle before returning, so trailing events are
// never missed.
func (h *testHost) serve(t *testing.T, commands ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	w := transport.NewPiJSONWriter(&out)
	s := NewServer(h, w)
	defer s.Close()
	in := strings.NewReader(strings.Join(commands, "\n") + "\n")
	if err := s.Serve(context.Background(), in); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if commandsIncludePrompt(commands) {
		h.settle(t)
	}
	return parseLines(t, out.String())
}

// settle waits for the prompt run to finish.
//
// The wait is two-stage on purpose. WaitForIdle returns immediately when
// the run has not registered yet — and the run registers asynchronously,
// after the response is already on the wire — so the first stage waits for
// evidence the run exists at all: a provider call (the loop reaches it
// after agent_start) or a completed run (a failure path emits agent_end
// without ever calling the provider). Both signals are baselined at entry,
// so a run that finished before settle looked is still detected.
//
// Once either signal fires, the run is registered (streamFn is called from
// inside the run's goroutine, after the lifecycle recorded it), so the
// second stage sees the run rather than nothing — and a run that already
// finished makes WaitForIdle a no-op with every event already written.
func (h *testHost) settle(t *testing.T) {
	t.Helper()
	beforeCalls := atomic.LoadInt32(&h.calls)
	beforeEnds := atomic.LoadInt32(&h.agentEnds)
	waitFor(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&h.calls) > beforeCalls || atomic.LoadInt32(&h.agentEnds) > beforeEnds
	})
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Agent().WaitForIdle(waitCtx)
}

// commandsIncludePrompt reports whether the batch starts a run.
func commandsIncludePrompt(commands []string) bool {
	for _, c := range commands {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(c), &probe) == nil && probe.Type == CmdPrompt {
			return true
		}
	}
	return false
}

// writeSessionFile writes a real transcript with one exchange and returns
// its path — the file ResumeSession opens, so the swap under test loads a
// genuine session rather than a struct. The config home is redirected so
// no test touches ~/.ultrac.
func writeSessionFile(t *testing.T, cwd, prompt, answer string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".ultrac")
	t.Setenv(config.EnvConfigDir, home)
	m := session.CreateSession(cwd)
	for _, msg := range []ai.Message{
		{Role: ai.RoleUser, Content: []ai.ContentBlock{ai.TextBlock(prompt)}, TimestampMS: time.Now().UnixMilli()},
		{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock(answer)}, TimestampMS: time.Now().UnixMilli()},
	} {
		if _, err := m.AppendMessage(msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	return m.SessionFile()
}

func parseLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(raw, "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", l, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// serverDriver drives a running server over a pipe, so a test can observe
// state mid-run — the batch serve() helper cannot pause between commands.
//
// The splitter is an io.Writer the server's PiJSONWriter feeds: each Write
// is exactly one line (the writer appends the terminator itself), so
// response lines can be routed to a channel while every line is kept in
// arrival order for whole-stream assertions.
type serverDriver struct {
	t      *testing.T
	host   *testHost
	pipe   *io.PipeWriter
	respCh chan map[string]any
}

func startServer(t *testing.T, h *testHost) *serverDriver {
	t.Helper()
	pr, pw := io.Pipe()
	sp := &lineSplitter{respCh: make(chan map[string]any, 64)}
	s := NewServer(h, transport.NewPiJSONWriter(sp))
	d := &serverDriver{t: t, host: h, pipe: pw, respCh: sp.respCh}
	go func() { _ = s.Serve(context.Background(), pr) }()
	t.Cleanup(func() { _ = pw.Close() })
	return d
}

// send writes one command line.
func (d *serverDriver) send(line string) {
	if _, err := fmt.Fprintln(d.pipe, line); err != nil {
		d.t.Fatalf("send: %v", err)
	}
}

// next returns the next response line, failing the test on timeout.
func (d *serverDriver) next() map[string]any {
	d.t.Helper()
	select {
	case m := <-d.respCh:
		return m
	case <-time.After(5 * time.Second):
		d.t.Fatal("no response within 5s")
		return nil
	}
}

type lineSplitter struct {
	respCh chan map[string]any
}

func (ls *lineSplitter) Write(p []byte) (int, error) {
	line := strings.TrimSuffix(string(p), "\n")
	if line == "" {
		return len(p), nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err == nil && m["type"] == "response" {
		select {
		case ls.respCh <- m:
		default:
		}
	}
	return len(p), nil
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

// cmd builds one command line. A nil fields map is allowed: the command
// carries only its type.
func cmd(typ string, fields map[string]any) string {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["type"] = typ
	data, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// responses filters the response lines.
func responses(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["type"] == "response" {
			out = append(out, l)
		}
	}
	return out
}

// response finds the first response for a command.
func response(t *testing.T, lines []map[string]any, command string) map[string]any {
	t.Helper()
	return nthResponse(t, lines, command, 0)
}

// nthResponse finds the n-th response for a command (0-based) — a batch can
// legitimately contain several, some failing and some not.
func nthResponse(t *testing.T, lines []map[string]any, command string, n int) map[string]any {
	t.Helper()
	seen := 0
	for _, l := range responses(lines) {
		if l["command"] == command {
			if seen == n {
				return l
			}
			seen++
		}
	}
	t.Fatalf("response #%d for command %q not found in %v", n, command, lines)
	return nil
}

// dataMap extracts the data object of a response.
func dataMap(t *testing.T, r map[string]any) map[string]any {
	t.Helper()
	d, ok := r["data"].(map[string]any)
	if !ok {
		t.Fatalf("response %v has no data object", r)
	}
	return d
}

// eventTypes lists the event discriminants in order.
func eventTypes(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		if l["type"] != "response" {
			out = append(out, l["type"].(string))
		}
	}
	return out
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if os.Getenv("RPC_TEST_DUMP_STACKS") != "" {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Logf("STACKS:\n%s", buf[:n])
	}
	t.Fatal("condition not met within timeout")
}

// ---------------------------------------------------------------------------
// Protocol guards
// ---------------------------------------------------------------------------

func TestUnknownCommandGetsErrorNotCrash(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t,
		cmd("frobnicate", map[string]any{"id": "9"}),
		cmd("get_state", map[string]any{"id": "10"}),
	)
	if len(responses(lines)) != 2 {
		t.Fatalf("got %d responses, want 2 — the loop must survive an unknown command", len(responses(lines)))
	}
	bad := response(t, lines, "frobnicate")
	if bad["success"] != false {
		t.Errorf("success = %v, want false", bad["success"])
	}
	if msg, _ := bad["error"].(string); msg != "Unknown command: frobnicate" {
		t.Errorf("error = %q, want %q", msg, "Unknown command: frobnicate")
	}
	if bad["id"] != "9" {
		t.Errorf("id = %v, want 9 (the echo is what lets the client correlate)", bad["id"])
	}
	if _, has := bad["data"]; has {
		t.Error("an error response must not carry data")
	}
	if response(t, lines, "get_state")["success"] != true {
		t.Error("the loop must survive an unknown command")
	}
}

func TestParseErrorResponseKeepsLoopAlive(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t,
		`{not json at all`,
		cmd("get_state", nil),
	)
	rs := responses(lines)
	if len(rs) != 2 {
		t.Fatalf("got %d responses, want 2", len(rs))
	}
	if rs[0]["command"] != "parse" {
		t.Errorf("command = %v, want parse", rs[0]["command"])
	}
	if rs[0]["success"] != false {
		t.Errorf("parse success = %v, want false", rs[0]["success"])
	}
	if _, has := rs[0]["id"]; has {
		t.Error("a parse failure cannot echo an id — there is nothing to echo")
	}
	if msg, _ := rs[0]["error"].(string); !strings.HasPrefix(msg, "Failed to parse command:") {
		t.Errorf("error = %q", msg)
	}
	if rs[1]["success"] != true {
		t.Error("the loop must survive a malformed line")
	}
}

func TestResponseShapeSuccessWithoutData(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t, cmd("abort", map[string]any{"id": "a1"}))
	r := response(t, lines, "abort")
	if r["type"] != "response" || r["command"] != "abort" || r["success"] != true {
		t.Fatalf("response = %v", r)
	}
	if _, has := r["data"]; has {
		t.Error("a command with nothing to report must omit data, not send null")
	}
	if _, has := r["error"]; has {
		t.Error("a success response must not carry error")
	}
}

func TestResponseOmitsIDWhenCommandHadNone(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t, cmd("get_state", nil))
	r := response(t, lines, "get_state")
	if _, has := r["id"]; has {
		t.Errorf("id = %v, want absent (pi echoes only what was sent)", r["id"])
	}
}

// ---------------------------------------------------------------------------
// Prompting
// ---------------------------------------------------------------------------

func TestPromptStartsRunResponseBeforeEvents(t *testing.T) {
	h := newTestHost(t, textScript("RPC-OK"))
	lines := h.serve(t, cmd("prompt", map[string]any{"id": "p1", "message": "hello"}))

	if len(lines) == 0 || lines[0]["type"] != "response" {
		t.Fatalf("first line = %v, want the prompt response — it must precede every event", lines[0])
	}
	r := lines[0]
	if r["command"] != "prompt" || r["success"] != true {
		t.Fatalf("response = %v", r)
	}
	if dataMap(t, r)["disposition"] != "started" {
		t.Errorf("disposition = %v, want started", dataMap(t, r)["disposition"])
	}

	types := eventTypes(lines)
	if len(types) == 0 || types[0] != "agent_start" {
		t.Fatalf("event types = %v, want agent_start first", types)
	}
	if len(types) < 2 || types[len(types)-2] != "agent_end" || types[len(types)-1] != "agent_settled" {
		t.Errorf("event types = %v, want agent_end then agent_settled last", types)
	}

	// The run actually happened: the transcript carries the user message
	// and the scripted answer.
	msgs := h.Agent().State().Messages
	if len(msgs) != 2 {
		t.Fatalf("transcript has %d messages, want 2 (user + assistant)", len(msgs))
	}
	if ai.MessageText(msgs[0]) != "hello" {
		t.Errorf("user message = %q", ai.MessageText(msgs[0]))
	}
	if ai.MessageText(msgs[1]) != "RPC-OK" {
		t.Errorf("assistant message = %q", ai.MessageText(msgs[1]))
	}
}

func TestPromptDuringStreamingQueuesOrErrors(t *testing.T) {
	h := newTestHost(t)
	streamFn, release := gatedStreamFn(h, "late answer")
	defer release()

	h.rebuildAgent(streamFn)
	d := startServer(t, h)

	d.send(cmd("prompt", map[string]any{"id": "p1", "message": "hold"}))
	if dataMap(t, d.next())["disposition"] != "started" {
		t.Fatal("the first prompt must start")
	}
	// Wait until the loop is past its initial steering poll and blocked in
	// the gated stream (the provider call proves both). Waiting only for
	// isStreaming would race the poll: a message steered before it runs is
	// drained into the current turn instead of sitting in the queue.
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&h.calls) > 0 })
	waitFor(t, 2*time.Second, func() bool { return h.Agent().State().IsStreaming })

	// A second prompt without streamingBehavior is an error, exactly as pi
	// throws (agent-session.ts:2063-2067).
	d.send(cmd("prompt", map[string]any{"id": "p2", "message": "queued?"}))
	noBehavior := d.next()
	if noBehavior["success"] != false {
		t.Errorf("success = %v, want false", noBehavior["success"])
	}
	if msg, _ := noBehavior["error"].(string); !strings.Contains(msg, "streamingBehavior") {
		t.Errorf("error = %q, want the streamingBehavior explanation", msg)
	}

	// With a behavior named, the message is queued and reported as such.
	d.send(cmd("prompt", map[string]any{"id": "p3", "message": "steer me", "streamingBehavior": "steer"}))
	if dataMap(t, d.next())["disposition"] != "queued" {
		t.Error("a prompt during streaming with a behavior must queue")
	}

	// While streaming, get_state reports it and the pending queue.
	d.send(cmd("get_state", nil))
	sd := dataMap(t, d.next())
	if sd["isStreaming"] != true {
		t.Error("isStreaming = false, want true mid-run")
	}
	if sd["pendingMessageCount"].(float64) != 1 {
		t.Errorf("pendingMessageCount = %v, want 1", sd["pendingMessageCount"])
	}

	// Release the run: the queued steering message drains into the next
	// turn, so the queue empties.
	release()
	waitFor(t, 5*time.Second, func() bool { return !h.Agent().State().IsStreaming })
	waitFor(t, 2*time.Second, func() bool { return len(h.Agent().SteeringMessages()) == 0 })
}

func TestSteerFollowUpClearQueue(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t,
		cmd("steer", map[string]any{"message": "s1"}),
		cmd("follow_up", map[string]any{"message": "f1"}),
		cmd("get_state", nil),
		cmd("clear_queue", nil),
		cmd("get_state", nil),
	)

	if dataMap(t, response(t, lines, "steer"))["disposition"] != "queued" {
		t.Error("steer disposition must be queued")
	}
	if dataMap(t, response(t, lines, "follow_up"))["disposition"] != "queued" {
		t.Error("follow_up disposition must be queued")
	}

	// The first get_state sees both queued messages.
	if sd := dataMap(t, nthResponse(t, lines, "get_state", 0)); sd["pendingMessageCount"].(float64) != 2 {
		t.Errorf("pendingMessageCount = %v, want 2", sd["pendingMessageCount"])
	}

	// clear_queue reports what it removed, per queue.
	cd := dataMap(t, response(t, lines, "clear_queue"))
	if steering, _ := cd["steering"].([]any); len(steering) != 1 || steering[0] != "s1" {
		t.Errorf("cleared steering = %v, want [s1]", cd["steering"])
	}
	if followUp, _ := cd["followUp"].([]any); len(followUp) != 1 || followUp[0] != "f1" {
		t.Errorf("cleared followUp = %v, want [f1]", cd["followUp"])
	}

	// The second get_state reports an empty queue.
	if sd := dataMap(t, nthResponse(t, lines, "get_state", 1)); sd["pendingMessageCount"].(float64) != 0 {
		t.Errorf("pendingMessageCount after clear = %v, want 0", sd["pendingMessageCount"])
	}
}

func TestAbortSettlesRunAndLoopContinues(t *testing.T) {
	h := newTestHost(t)
	streamFn, release := gatedStreamFn(h, "never mind")
	defer release()

	h.rebuildAgent(streamFn)
	d := startServer(t, h)

	d.send(cmd("prompt", map[string]any{"id": "p1", "message": "hold"}))
	if dataMap(t, d.next())["disposition"] != "started" {
		t.Fatal("the first prompt must start")
	}
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&h.calls) > 0 })
	waitFor(t, 2*time.Second, func() bool { return h.Agent().State().IsStreaming })

	// abort awaits the run's settlement (pi: rpc-mode.ts:426-429): the
	// response only arrives once the run has actually stopped.
	d.send(cmd("abort", map[string]any{"id": "a1"}))
	r := d.next()
	if r["success"] != true {
		t.Fatalf("abort = %v", r)
	}
	if h.Agent().State().IsStreaming {
		t.Error("the run must be settled by the time abort responds")
	}

	// The loop still serves after an abort.
	d.send(cmd("get_state", nil))
	if sd := dataMap(t, d.next()); sd["isStreaming"] != false {
		t.Error("get_state must answer after an abort")
	}
}

// ---------------------------------------------------------------------------
// Session and state
// ---------------------------------------------------------------------------

func TestGetStateShape(t *testing.T) {
	h := newTestHost(t, textScript("done"))
	h.serve(t, cmd("prompt", map[string]any{"message": "hi"}))
	lines := h.serve(t, cmd("get_state", nil))
	sd := dataMap(t, response(t, lines, "get_state"))

	checks := map[string]any{
		"thinkingLevel":         "off",
		"isStreaming":           false,
		"isCompacting":          false,
		"steeringMode":          "one-at-a-time",
		"followUpMode":          "one-at-a-time",
		"autoCompactionEnabled": false,
		"messageCount":          float64(2),
		"pendingMessageCount":   float64(0),
	}
	for k, want := range checks {
		if sd[k] != want {
			t.Errorf("state[%q] = %v, want %v", k, sd[k], want)
		}
	}
	if model, _ := sd["model"].(map[string]any); model == nil || model["id"] != "model-a" {
		t.Errorf("state.model = %v, want model-a", sd["model"])
	}
	if id, _ := sd["sessionId"].(string); id == "" {
		t.Error("sessionId must name the live session")
	}
	if _, has := sd["sessionName"]; has {
		t.Error("sessionName must be absent, not empty, before one is set")
	}
}

// ---------------------------------------------------------------------------
// Compaction commands (P-C2: docs/compaction-branch-plan.md)
//
// The real compaction (plan → summarize → append → swap) is cmd/ultrac's,
// tested there against the real harness. What is under test here is the
// wire contract: the response shapes, the required-argument guard, and
// get_state's two live fields.
// ---------------------------------------------------------------------------

func TestCompactCommandShape(t *testing.T) {
	h := newTestHost(t)
	h.compactResult = &CompactResult{
		Summary:              "## Goal\nship it",
		FirstKeptEntryID:     "entry-42",
		TokensBefore:         182340,
		EstimatedTokensAfter: 24110,
		Usage:                ai.Usage{Input: 12345, Output: 678, TotalTokens: 13023},
		Details:              session.CompactionDetails{ReadFiles: []string{"go.mod"}, ModifiedFiles: []string{"main.go"}},
	}

	lines := h.serve(t, cmd("compact", map[string]any{
		"id":                 "c1",
		"customInstructions": "keep the deployment notes",
	}))
	r := response(t, lines, "compact")
	if r["success"] != true {
		t.Fatalf("compact = %v", r)
	}
	d := dataMap(t, r)
	checks := map[string]any{
		"summary":              "## Goal\nship it",
		"firstKeptEntryId":     "entry-42",
		"tokensBefore":         float64(182340),
		"estimatedTokensAfter": float64(24110),
	}
	for k, want := range checks {
		if d[k] != want {
			t.Errorf("data[%q] = %v, want %v", k, d[k], want)
		}
	}
	// usage and details ride along whole (pi's CompactionResult).
	usage, _ := d["usage"].(map[string]any)
	if usage == nil || usage["totalTokens"] != float64(13023) {
		t.Errorf("data.usage = %v, want the summary call's usage", d["usage"])
	}
	details, _ := d["details"].(map[string]any)
	if details == nil {
		t.Fatalf("data.details = %v, want the file-operation lists", d["details"])
	}
	read, _ := details["readFiles"].([]any)
	if len(read) != 1 || read[0] != "go.mod" {
		t.Errorf("details.readFiles = %v, want [go.mod]", details["readFiles"])
	}
	// The custom focus reached the host (it is appended to the prompt by
	// the engine; the command's job is to carry it).
	if len(h.compactCalls) != 1 || h.compactCalls[0] != "keep the deployment notes" {
		t.Errorf("customInstructions = %v, want the command's value", h.compactCalls)
	}
}

func TestCompactCommandError(t *testing.T) {
	// pi's idempotence errors surface verbatim: a client must be able to
	// tell "already compacted" from a summarization failure.
	for _, message := range []string{"Already compacted", "Nothing to compact (session too small)"} {
		h := newTestHost(t)
		h.compactErr = errors.New(message)
		lines := h.serve(t, cmd("compact", map[string]any{"id": "c1"}))
		r := response(t, lines, "compact")
		if r["success"] != false {
			t.Errorf("compact with %q: success = %v, want false", message, r["success"])
		}
		if got, _ := r["error"].(string); got != message {
			t.Errorf("error = %q, want %q", got, message)
		}
		if _, has := r["data"]; has {
			t.Error("an error response must not carry data")
		}
	}
}

// ---------------------------------------------------------------------------
// add_custom_model (custom-model-plan.md)
// ---------------------------------------------------------------------------

// validCustomModelFields is a well-formed command: every test in this
// section starts from it and breaks exactly one thing.
func validCustomModelFields() map[string]any {
	return map[string]any{
		"provider": "api.deepseek.com",
		"baseUrl":  "https://api.deepseek.com/v1",
		"apiKey":   "sk-test",
		"api":      string(ai.APIOpenAICompletions),
		"modelId":  "deepseek-chat",
	}
}

func TestAddCustomModelCommandShape(t *testing.T) {
	h := newTestHost(t)
	h.addResult = &AddCustomModelResult{
		Models: []ai.Model{{
			ID:            "deepseek-chat",
			Name:          "deepseek-chat",
			API:           ai.APIOpenAICompletions,
			Provider:      "api.deepseek.com",
			BaseURL:       "https://api.deepseek.com/v1",
			ContextWindow: 128000,
			MaxTokens:     8192,
		}},
		Providers: []CustomModelProviderInfo{{
			ID:       "api.deepseek.com",
			BaseURL:  "https://api.deepseek.com/v1",
			API:      string(ai.APIOpenAICompletions),
			ModelIDs: []string{"deepseek-chat"},
		}},
	}

	lines := h.serve(t, cmd("add_custom_model", validCustomModelFields()))
	r := response(t, lines, "add_custom_model")
	if r["success"] != true {
		t.Fatalf("add_custom_model = %v", r)
	}
	d := dataMap(t, r)
	models, _ := d["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("data.models = %v, want the fresh catalog with the new model", d["models"])
	}
	entry, _ := models[0].(map[string]any)
	for k, want := range map[string]any{
		"id":            "deepseek-chat",
		"provider":      "api.deepseek.com",
		"baseUrl":       "https://api.deepseek.com/v1",
		"api":           string(ai.APIOpenAICompletions),
		"contextWindow": float64(128000),
		"maxTokens":     float64(8192),
	} {
		if entry[k] != want {
			t.Errorf("data.models[0][%q] = %v, want %v", k, entry[k], want)
		}
	}
	providers, _ := d["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("data.providers = %v, want the user-declared providers", d["providers"])
	}
	info, _ := providers[0].(map[string]any)
	for k, want := range map[string]any{
		"id":      "api.deepseek.com",
		"baseUrl": "https://api.deepseek.com/v1",
		"api":     string(ai.APIOpenAICompletions),
	} {
		if info[k] != want {
			t.Errorf("data.providers[0][%q] = %v, want %v", k, info[k], want)
		}
	}
	ids, _ := info["modelIds"].([]any)
	if len(ids) != 1 || ids[0] != "deepseek-chat" {
		t.Errorf("data.providers[0].modelIds = %v, want [deepseek-chat]", info["modelIds"])
	}

	// The plan's defaults (R1): an absent contextWindow / maxTokens means
	// the default, never zero — a zero window would silently disable
	// auto-compaction. The rest of the command reaches the host verbatim.
	if len(h.addCalls) != 1 {
		t.Fatalf("host calls = %d, want 1", len(h.addCalls))
	}
	spec := h.addCalls[0]
	if spec.ContextWindow != defaultContextWindow || spec.MaxTokens != defaultMaxTokens {
		t.Errorf("limits = (%d, %d), want the defaults (%d, %d)",
			spec.ContextWindow, spec.MaxTokens, defaultContextWindow, defaultMaxTokens)
	}
	if spec.Provider != "api.deepseek.com" || spec.BaseURL != "https://api.deepseek.com/v1" ||
		spec.APIKey != "sk-test" || spec.API != string(ai.APIOpenAICompletions) ||
		spec.ModelID != "deepseek-chat" {
		t.Errorf("spec = %+v, want the command's fields verbatim", spec)
	}

	// A client that knows the real limits passes them through, and the
	// protocol trims surrounding whitespace (a paste with a newline is a
	// common clipboard artifact).
	h2 := newTestHost(t)
	h2.addResult = &AddCustomModelResult{}
	explicit := validCustomModelFields()
	explicit["id"] = "m2"
	explicit["provider"] = "  api.deepseek.com\n"
	explicit["contextWindow"] = 64000
	explicit["maxTokens"] = 4096
	explicit["modelName"] = "DeepSeek Chat"
	lines = h2.serve(t, cmd("add_custom_model", explicit))
	if r := response(t, lines, "add_custom_model"); r["success"] != true {
		t.Fatalf("add_custom_model (explicit limits) = %v", r)
	}
	if got := h2.addCalls[0]; got.ContextWindow != 64000 || got.MaxTokens != 4096 ||
		got.Provider != "api.deepseek.com" || got.ModelName != "DeepSeek Chat" {
		t.Errorf("spec = %+v, want the explicit limits and the trimmed fields", got)
	}
}

func TestAddCustomModelValidationFailures(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any // merged over the valid base
		remove []string       // keys to drop before merging fields
		want   string
	}{
		{
			name:   "missing provider",
			remove: []string{"provider"},
			want:   "provider is required",
		},
		{
			name:   "provider with a slash",
			fields: map[string]any{"provider": "deepseek/chat"},
			want:   `must not contain "/" or whitespace`,
		},
		{
			name:   "provider with whitespace",
			fields: map[string]any{"provider": "deep seek"},
			want:   `must not contain "/" or whitespace`,
		},
		{
			name:   "missing baseUrl",
			remove: []string{"baseUrl"},
			want:   "baseUrl is required",
		},
		{
			name:   "non-http baseUrl",
			fields: map[string]any{"baseUrl": "ftp://api.deepseek.com"},
			want:   "must be an http(s) URL with a host",
		},
		{
			name:   "baseUrl without a host",
			fields: map[string]any{"baseUrl": "https://"},
			want:   "must be an http(s) URL with a host",
		},
		{
			name:   "missing apiKey",
			remove: []string{"apiKey"},
			want:   "apiKey is required",
		},
		{
			name:   "missing modelId",
			remove: []string{"modelId"},
			want:   "modelId is required",
		},
		{
			name:   "unregistered dialect",
			fields: map[string]any{"api": "bedrock-converse-stream"},
			want:   `api "bedrock-converse-stream" is not a supported dialect`,
		},
		{
			name:   "zero contextWindow",
			fields: map[string]any{"contextWindow": 0},
			want:   "contextWindow must be > 0",
		},
		{
			name:   "negative maxTokens",
			fields: map[string]any{"maxTokens": -1},
			want:   "maxTokens must be > 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHost(t)
			h.addResult = &AddCustomModelResult{}
			fields := validCustomModelFields()
			for _, k := range tc.remove {
				delete(fields, k)
			}
			fields["id"] = "bad"
			for k, v := range tc.fields {
				fields[k] = v
			}
			lines := h.serve(t, cmd("add_custom_model", fields))
			r := response(t, lines, "add_custom_model")
			if r["success"] != false {
				t.Fatalf("success = %v, want false: %v", r["success"], r)
			}
			if got, _ := r["error"].(string); !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to contain %q", got, tc.want)
			}
			if _, has := r["data"]; has {
				t.Error("an error response must not carry data")
			}
			// A malformed command never reaches the host: the write
			// side is not a validator, and half-writing a provider
			// from a bad command is the failure D7 warns about.
			if len(h.addCalls) != 0 {
				t.Errorf("host calls = %v, want none for a malformed command", h.addCalls)
			}
		})
	}
}

func TestAddCustomModelHostError(t *testing.T) {
	// Everything the protocol can check has passed; what fails now is the
	// filesystem — auth.json unwritable, models.json invalid, a dialect
	// the host cannot register. The command carries the host's message
	// verbatim (the dialog shows it) and answers with no data.
	h := newTestHost(t)
	h.addErr = errors.New("write auth.json: Access is denied")
	fields := validCustomModelFields()
	fields["id"] = "m1"
	lines := h.serve(t, cmd("add_custom_model", fields))
	r := response(t, lines, "add_custom_model")
	if r["success"] != false {
		t.Fatalf("success = %v, want false: %v", r["success"], r)
	}
	if got, _ := r["error"].(string); got != "write auth.json: Access is denied" {
		t.Errorf("error = %q, want it verbatim", got)
	}
	if _, has := r["data"]; has {
		t.Error("an error response must not carry data")
	}
	if len(h.addCalls) != 1 {
		t.Errorf("host calls = %d, want 1 — a valid command must reach the host", len(h.addCalls))
	}
}

func TestSetAutoCompactionCommand(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t,
		cmd("set_auto_compaction", map[string]any{"id": "s1", "enabled": true}),
		cmd("get_state", map[string]any{"id": "after"}),
	)
	r := response(t, lines, "set_auto_compaction")
	if r["success"] != true {
		t.Fatalf("set_auto_compaction = %v", r)
	}
	if _, has := r["data"]; has {
		t.Error("set_auto_compaction answers with no data (pi: success(id, cmd))")
	}
	// The gate flipped live: get_state reports the new value, and the host
	// recorded the exact value it was asked for.
	if got := dataMap(t, response(t, lines, "get_state"))["autoCompactionEnabled"]; got != true {
		t.Errorf("autoCompactionEnabled after set = %v, want true", got)
	}
	if len(h.setAutoCalls) != 1 || h.setAutoCalls[0] != true {
		t.Errorf("host recorded %v, want [true]", h.setAutoCalls)
	}

	// A missing enabled is a malformed command, not a request to disable:
	// answering it with a silent "off" is how a client typo becomes a
	// setting.
	lines = h.serve(t, cmd("set_auto_compaction", map[string]any{"id": "s2"}))
	bad := response(t, lines, "set_auto_compaction")
	if bad["success"] != false {
		t.Error("a missing enabled must fail")
	}
	if got, _ := bad["error"].(string); got != "enabled is required" {
		t.Errorf("error = %q, want %q", got, "enabled is required")
	}
	if len(h.setAutoCalls) != 1 {
		t.Error("a malformed command must not reach the host")
	}
}

func TestSetAutoCompactionWriteFailureKeepsLiveGate(t *testing.T) {
	h := newTestHost(t)
	h.autoCompaction = true
	h.setAutoErr = errors.New("config: refusing to write settings.json")
	lines := h.serve(t, cmd("set_auto_compaction", map[string]any{"id": "s1", "enabled": false}))
	r := response(t, lines, "set_auto_compaction")
	if r["success"] != false {
		t.Error("a failed settings write must surface as an error response")
	}
	if !strings.Contains(fmt.Sprint(r["error"]), "refusing to write") {
		t.Errorf("error = %v, want the write failure", r["error"])
	}
	// The live gate is untouched: file and session must not disagree.
	if !h.autoCompaction {
		t.Error("the live gate flipped despite the failed write")
	}
}

func TestGetStateReportsCompactionFields(t *testing.T) {
	h := newTestHost(t)
	h.compacting = true
	h.autoCompaction = true
	lines := h.serve(t, cmd("get_state", nil))
	sd := dataMap(t, response(t, lines, "get_state"))
	if sd["isCompacting"] != true {
		t.Errorf("isCompacting = %v, want true", sd["isCompacting"])
	}
	if sd["autoCompactionEnabled"] != true {
		t.Errorf("autoCompactionEnabled = %v, want true", sd["autoCompactionEnabled"])
	}
}

func TestNewSessionSwapsAgentAndSession(t *testing.T) {
	h := newTestHost(t, textScript("first"), textScript("second"))
	// The prompt run is asynchronous by protocol (the response precedes
	// the events), so a batch that queries state after it must be a
	// separate serve — otherwise the query races the run.
	h.serve(t, cmd("prompt", map[string]any{"message": "one"}))
	lines := h.serve(t,
		cmd("get_state", map[string]any{"id": "before"}),
		cmd("new_session", map[string]any{"id": "ns"}),
		cmd("get_state", map[string]any{"id": "after"}),
	)

	r := response(t, lines, "new_session")
	if r["success"] != true {
		t.Fatalf("new_session = %v", r)
	}
	if dataMap(t, r)["cancelled"] != false {
		t.Error("cancelled must be false in a headless session")
	}

	var before, after map[string]any
	for _, l := range responses(lines) {
		if l["command"] == "get_state" {
			switch l["id"] {
			case "before":
				before = dataMap(t, l)
			case "after":
				after = dataMap(t, l)
			}
		}
	}
	if before["sessionId"] == after["sessionId"] {
		t.Error("new_session must start a new session identity")
	}
	if after["messageCount"].(float64) != 0 {
		t.Errorf("messageCount after new_session = %v, want 0", after["messageCount"])
	}

	// The pump followed the swap: the next prompt's events belong to the
	// new agent, and get_messages reports only the new run.
	h.serve(t, cmd("prompt", map[string]any{"message": "two"}))
	lines = h.serve(t, cmd("get_messages", nil))
	list, _ := dataMap(t, response(t, lines, "get_messages"))["messages"].([]any)
	if len(list) != 2 {
		t.Fatalf("messages = %d, want 2 (the second run only)", len(list))
	}
	if !strings.Contains(ai.MessageText(mustMessage(t, list[1])), "second") {
		t.Errorf("assistant message = %q, want the second script's answer", ai.MessageText(mustMessage(t, list[1])))
	}
}

// ---------------------------------------------------------------------------
// Session listing and resume (P-D0: the desktop shell's sidebar)
// ---------------------------------------------------------------------------

func TestListSessionsShape(t *testing.T) {
	h := newTestHost(t)
	h.listSessions = []session.SessionSummary{
		{
			SessionID: "019aaa", Path: `C:\sessions\a.jsonl`, Title: "fix the login bug",
			Timestamp: "2026-10-19T08:00:00.000Z", MessageCount: 4, Name: "auth work", CWD: `D:\repo`,
		},
		{
			SessionID: "019bbb", Path: `C:\sessions\b.jsonl`, Title: "(no user message)",
			Timestamp: "2026-10-18T08:00:00.000Z",
		},
	}
	lines := h.serve(t, cmd("list_sessions", map[string]any{"id": "ls"}))
	r := response(t, lines, "list_sessions")
	if r["success"] != true {
		t.Fatalf("list_sessions = %v", r)
	}
	list, ok := dataMap(t, r)["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions = %v, want a list", dataMap(t, r)["sessions"])
	}
	if len(list) != 2 {
		t.Fatalf("listed %d sessions, want 2 (host order preserved)", len(list))
	}
	first, _ := list[0].(map[string]any)
	for k, want := range map[string]any{
		"sessionId":    "019aaa",
		"path":         `C:\sessions\a.jsonl`,
		"title":        "fix the login bug",
		"timestamp":    "2026-10-19T08:00:00.000Z",
		"messageCount": float64(4),
		"name":         "auth work",
		"cwd":          `D:\repo`,
	} {
		if first[k] != want {
			t.Errorf("session[%q] = %v, want %v", k, first[k], want)
		}
	}
	// A session with no display name omits the field rather than sending "".
	second, _ := list[1].(map[string]any)
	if _, has := second["name"]; has {
		t.Errorf("name = %v, want absent (omitempty) when unset", second["name"])
	}
}

func TestListSessionsEmptyIsSuccess(t *testing.T) {
	h := newTestHost(t) // no canned sessions: the nil list
	lines := h.serve(t, cmd("list_sessions", nil))
	d := dataMap(t, response(t, lines, "list_sessions"))
	list, ok := d["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions = %v, want a list — null would force the client to special-case it", d["sessions"])
	}
	if len(list) != 0 {
		t.Errorf("listed %d sessions, want 0", len(list))
	}
}

func TestListSessionsErrorResponse(t *testing.T) {
	h := newTestHost(t)
	h.listErr = errors.New("sessions directory unreadable")
	lines := h.serve(t, cmd("list_sessions", map[string]any{"id": "ls"}))
	r := response(t, lines, "list_sessions")
	if r["success"] != false {
		t.Errorf("success = %v, want false", r["success"])
	}
	if msg, _ := r["error"].(string); msg != "sessions directory unreadable" {
		t.Errorf("error = %q, want the host's message verbatim", msg)
	}
}

func TestResumeSessionSwapsToLoadedTranscript(t *testing.T) {
	path := writeSessionFile(t, `D:\repo`, "earlier question", "earlier answer")
	h := newTestHost(t, textScript("after resume"))

	lines := h.serve(t,
		cmd("get_state", map[string]any{"id": "before"}),
		cmd("resume_session", map[string]any{"id": "rs", "sessionPath": path}),
		cmd("get_state", map[string]any{"id": "after"}),
	)
	r := response(t, lines, "resume_session")
	if r["success"] != true {
		t.Fatalf("resume_session = %v", r)
	}
	if dataMap(t, r)["cancelled"] != false {
		t.Error("cancelled must be false in a headless session")
	}

	var before, after map[string]any
	for _, l := range responses(lines) {
		if l["command"] == "get_state" {
			switch l["id"] {
			case "before":
				before = dataMap(t, l)
			case "after":
				after = dataMap(t, l)
			}
		}
	}
	if before["sessionId"] == after["sessionId"] {
		t.Error("resume_session must load another session identity")
	}
	if after["sessionFile"] != path {
		t.Errorf("sessionFile = %v, want the resumed path", after["sessionFile"])
	}
	if after["messageCount"].(float64) != 2 {
		t.Errorf("messageCount = %v, want 2 (the replayed exchange)", after["messageCount"])
	}

	// The replay is real: the next run's transcript starts from the loaded
	// conversation, and its events reach the client through the rebound
	// pump (the pump that stayed bound to the abandoned agent would show
	// nothing).
	h.serve(t, cmd("prompt", map[string]any{"message": "next"}))
	lines = h.serve(t, cmd("get_messages", nil))
	list, _ := dataMap(t, response(t, lines, "get_messages"))["messages"].([]any)
	if len(list) != 4 {
		t.Fatalf("messages = %d, want 4 (the replayed exchange + the new run)", len(list))
	}
	if ai.MessageText(mustMessage(t, list[0])) != "earlier question" {
		t.Errorf("first message = %q, want the replayed user message", ai.MessageText(mustMessage(t, list[0])))
	}
	if ai.MessageText(mustMessage(t, list[3])) != "after resume" {
		t.Errorf("last message = %q, want the new answer", ai.MessageText(mustMessage(t, list[3])))
	}
}

func TestResumeSessionErrors(t *testing.T) {
	h := newTestHost(t)
	// Entries without a header: the shape OpenSession refuses — what a
	// truncated or foreign transcript looks like.
	headerless := filepath.Join(t.TempDir(), "headerless.jsonl")
	if err := os.WriteFile(headerless,
		[]byte(`{"type":"message","id":"x1","parentId":null,"timestamp":"2026-10-19T08:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"orphan"}]}}`+"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	lines := h.serve(t,
		cmd("resume_session", map[string]any{"id": "blank", "sessionPath": "   "}),
		cmd("resume_session", map[string]any{"id": "bad", "sessionPath": headerless}),
		cmd("get_state", map[string]any{"id": "alive"}),
	)
	blank := response(t, lines, "resume_session")
	if blank["success"] != false {
		t.Error("a blank sessionPath must be rejected")
	} else if msg, _ := blank["error"].(string); msg != "sessionPath is required" {
		t.Errorf("error = %q, want the wire-contract message", msg)
	}
	bad := nthResponse(t, lines, "resume_session", 1)
	if bad["success"] != false {
		t.Error("an invalid session file must be rejected")
	} else if msg, _ := bad["error"].(string); !strings.Contains(msg, "not a valid session file") {
		t.Errorf("error = %q, want the open failure", msg)
	}
	// A failed resume leaves the live session untouched and the loop
	// serving: the swap never happened, so nothing to rebind away from.
	// (sessionFile is omitempty, so an in-memory session reports the key
	// absent — a failed resume must not have put a path there.)
	if sf, _ := dataMap(t, response(t, lines, "get_state"))["sessionFile"].(string); sf != "" {
		t.Errorf("sessionFile = %q, want the live in-memory session untouched", sf)
	}
}

func TestGetMessagesAndLastAssistantText(t *testing.T) {
	h := newTestHost(t, textScript("the answer"))
	// Before any prompt: no messages, no last text.
	lines := h.serve(t,
		cmd("get_messages", nil),
		cmd("get_last_assistant_text", nil),
	)
	if list, _ := dataMap(t, response(t, lines, "get_messages"))["messages"].([]any); len(list) != 0 {
		t.Errorf("messages = %v, want empty", list)
	}
	if d := dataMap(t, response(t, lines, "get_last_assistant_text")); d["text"] != nil {
		t.Errorf("text = %v, want null (absent, not empty)", d["text"])
	}

	// After a prompt: both report the run. Separate serve: the query must
	// not race the asynchronous run.
	h.serve(t, cmd("prompt", map[string]any{"message": "ask"}))
	lines = h.serve(t,
		cmd("get_messages", nil),
		cmd("get_last_assistant_text", nil),
	)
	if list, _ := dataMap(t, response(t, lines, "get_messages"))["messages"].([]any); len(list) != 2 {
		t.Errorf("messages = %d, want 2", len(list))
	}
	if d := dataMap(t, response(t, lines, "get_last_assistant_text")); d["text"] != "the answer" {
		t.Errorf("text = %v, want %q", d["text"], "the answer")
	}
}

func TestSetSessionName(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t,
		cmd("set_session_name", map[string]any{"name": "   "}),
		cmd("set_session_name", map[string]any{"name": "My Session"}),
		cmd("get_state", nil),
	)
	blank := response(t, lines, "set_session_name")
	if blank["success"] != false {
		t.Error("a blank name must be rejected")
	} else if msg, _ := blank["error"].(string); msg != "Session name cannot be empty" {
		t.Errorf("error = %q", msg)
	}
	if nthResponse(t, lines, "set_session_name", 1)["success"] != true {
		t.Error("a real name must be accepted")
	}
	if sd := dataMap(t, response(t, lines, "get_state")); sd["sessionName"] != "My Session" {
		t.Errorf("sessionName = %v, want My Session", sd["sessionName"])
	}
}

func TestGetEntriesSinceAndLeaf(t *testing.T) {
	h := newTestHost(t, textScript("x"))
	h.serve(t, cmd("prompt", map[string]any{"message": "one"}))
	h.serve(t, cmd("set_session_name", map[string]any{"name": "named"}))
	lines := h.serve(t, cmd("get_entries", nil))
	d := dataMap(t, response(t, lines, "get_entries"))
	entries, _ := d["entries"].([]any)
	if len(entries) < 3 {
		t.Fatalf("entries = %d, want at least user+assistant+session_info", len(entries))
	}
	firstID := entries[0].(map[string]any)["id"].(string)
	if d["leafId"] == nil {
		t.Error("leafId must be set once entries exist")
	}

	// since slices after the named entry.
	since := h.serve(t, cmd("get_entries", map[string]any{"since": firstID}))
	sd := dataMap(t, response(t, since, "get_entries"))
	sinceEntries, _ := sd["entries"].([]any)
	if len(sinceEntries) != len(entries)-1 {
		t.Errorf("since entries = %d, want %d", len(sinceEntries), len(entries)-1)
	}

	// An unknown id is an error, not an empty list.
	bad := h.serve(t, cmd("get_entries", map[string]any{"since": "nope"}))
	if r := response(t, bad, "get_entries"); r["success"] != false {
		t.Error("an unknown since id must fail")
	} else if msg, _ := r["error"].(string); msg != "Entry not found: nope" {
		t.Errorf("error = %q", msg)
	}
}

func TestGetCommandsIsEmpty(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t, cmd("get_commands", nil))
	d := dataMap(t, response(t, lines, "get_commands"))
	commands, ok := d["commands"].([]any)
	if !ok {
		t.Fatalf("commands = %v, want a list", d["commands"])
	}
	if len(commands) != 0 {
		t.Errorf("commands = %v, want empty (no extensions/templates/skills in this port)", commands)
	}
}

// ---------------------------------------------------------------------------
// Model and thinking
// ---------------------------------------------------------------------------

func TestSetModel(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t,
		cmd("set_model", map[string]any{"provider": "test", "modelId": "model-b"}),
		cmd("set_model", map[string]any{"provider": "test", "modelId": "nope"}),
		cmd("get_state", nil),
		cmd("get_entries", nil),
	)
	ok := response(t, lines, "set_model")
	if ok["success"] != true {
		t.Fatalf("set_model = %v", ok)
	}
	if m, _ := ok["data"].(map[string]any); m["id"] != "model-b" {
		t.Errorf("data = %v, want model-b", ok["data"])
	}
	if sd := dataMap(t, response(t, lines, "get_state")); sd["model"].(map[string]any)["id"] != "model-b" {
		t.Error("get_state must report the switched model")
	}
	bad := nthResponse(t, lines, "set_model", 1)
	if bad["success"] != false {
		t.Error("an unknown model must fail")
	} else if msg, _ := bad["error"].(string); msg != "Model not found: test/nope" {
		t.Errorf("error = %q", msg)
	}

	// The switch is persisted: a model_change entry sits in the session.
	entries, _ := dataMap(t, response(t, lines, "get_entries"))["entries"].([]any)
	var found bool
	for _, e := range entries {
		if e.(map[string]any)["type"] == "model_change" {
			found = true
		}
	}
	if !found {
		t.Error("set_model must append a model_change entry")
	}
}

func TestGetAvailableModels(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t, cmd("get_available_models", nil))
	models, _ := dataMap(t, response(t, lines, "get_available_models"))["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
}

func TestThinkingLevelsClampAndList(t *testing.T) {
	h := newTestHost(t)
	// model-a is not a reasoning model: the only supported level is off,
	// so any request clamps to it (pi: models.ts:1230-1239).
	lines := h.serve(t,
		cmd("get_available_thinking_levels", nil),
		cmd("set_thinking_level", map[string]any{"level": "max"}),
		cmd("get_state", nil),
	)
	levels, _ := dataMap(t, response(t, lines, "get_available_thinking_levels"))["levels"].([]any)
	if len(levels) != 1 || levels[0] != "off" {
		t.Errorf("levels = %v, want [off]", levels)
	}
	if sd := dataMap(t, response(t, lines, "get_state")); sd["thinkingLevel"] != "off" {
		t.Errorf("thinkingLevel = %v, want off (clamped)", sd["thinkingLevel"])
	}

	// model-b supports off/low/high: the list reflects it and a supported
	// level passes through unchanged.
	lines = h.serve(t,
		cmd("set_model", map[string]any{"provider": "test", "modelId": "model-b"}),
		cmd("get_available_thinking_levels", nil),
		cmd("set_thinking_level", map[string]any{"level": "high"}),
		cmd("get_state", nil),
	)
	levels, _ = dataMap(t, nthResponse(t, lines, "get_available_thinking_levels", 0))["levels"].([]any)
	if len(levels) != 3 || levels[0] != "off" || levels[1] != "low" || levels[2] != "high" {
		t.Errorf("levels = %v, want [off low high]", levels)
	}
	if sd := dataMap(t, response(t, lines, "get_state")); sd["thinkingLevel"] != "high" {
		t.Errorf("thinkingLevel = %v, want high", sd["thinkingLevel"])
	}
}

func TestSetQueueModes(t *testing.T) {
	h := newTestHost(t)
	lines := h.serve(t,
		cmd("set_steering_mode", map[string]any{"mode": "bogus"}),
		cmd("set_steering_mode", map[string]any{"mode": "all"}),
		cmd("set_follow_up_mode", map[string]any{"mode": "all"}),
		cmd("get_state", nil),
	)
	bad := response(t, lines, "set_steering_mode")
	if bad["success"] != false {
		t.Error("a bogus mode must fail")
	} else if msg, _ := bad["error"].(string); msg != "mode must be 'all' or 'one-at-a-time'" {
		t.Errorf("error = %q", msg)
	}
	if nthResponse(t, lines, "set_steering_mode", 1)["success"] != true {
		t.Error("a valid mode must be accepted")
	}
	if sd := dataMap(t, response(t, lines, "get_state")); sd["steeringMode"] != "all" || sd["followUpMode"] != "all" {
		t.Errorf("modes = %v/%v, want all/all", sd["steeringMode"], sd["followUpMode"])
	}
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

func TestPromptImagesRideAlong(t *testing.T) {
	h := newTestHost(t, textScript("saw it"))
	h.serve(t, cmd("prompt", map[string]any{
		"message": "what is this",
		"images":  []map[string]any{{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"}},
	}))
	msgs := h.Agent().State().Messages
	if len(msgs) != 2 {
		t.Fatalf("transcript has %d messages, want 2", len(msgs))
	}
	blocks := msgs[0].Content
	if len(blocks) != 2 {
		t.Fatalf("user message has %d blocks, want 2 (text + image)", len(blocks))
	}
	if blocks[1].Type != ai.BlockImage || blocks[1].Data != "aGVsbG8=" || blocks[1].MimeType != "image/png" {
		t.Errorf("image block = %+v", blocks[1])
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestEOFEndsServe(t *testing.T) {
	h := newTestHost(t)
	var out strings.Builder
	s := NewServer(h, transport.NewPiJSONWriter(&out))
	defer s.Close()
	if err := s.Serve(context.Background(), strings.NewReader("")); err != nil {
		t.Errorf("Serve at EOF = %v, want nil", err)
	}
}

func TestContextCancelEndsServe(t *testing.T) {
	h := newTestHost(t)
	var out strings.Builder
	s := NewServer(h, transport.NewPiJSONWriter(&out))
	defer s.Close()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// A reader that never produces a line: only the cancellation can end
	// the loop.
	go func() { done <- s.Serve(ctx, blockingReader{release: release}) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("Serve err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// blockingReader blocks until released, then reports EOF.
type blockingReader struct{ release chan struct{} }

func (b blockingReader) Read(p []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

// mustMessage decodes one wire message from a get_messages entry.
func mustMessage(t *testing.T, v any) ai.Message {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m ai.Message
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Branch tree (P-C4): fork / get_fork_messages / get_tree / clone
// ---------------------------------------------------------------------------

// runTwoExchanges drives two prompts so the session holds
// user "one" → assistant → user "two" → assistant, leaf on the last
// assistant message. Every branch test starts from this shape, and the
// two scripts answer "a1"/"a2" so the branches stay distinguishable.
func runTwoExchanges(t *testing.T, h *testHost) {
	t.Helper()
	h.serve(t, cmd("prompt", map[string]any{"message": "one"}))
	h.serve(t, cmd("prompt", map[string]any{"message": "two"}))
}

// entryIDs lists the entry ids in file order from a get_entries response.
func entryIDs(t *testing.T, lines []map[string]any, command string) []string {
	t.Helper()
	entries, _ := dataMap(t, response(t, lines, command))["entries"].([]any)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.(map[string]any)["id"].(string))
	}
	return out
}

// treeNodeByID finds one node in a get_tree forest by entry id.
func treeNodeByID(t *testing.T, forest any, id string) map[string]any {
	t.Helper()
	nodes, _ := forest.([]any)
	for _, n := range nodes {
		m := n.(map[string]any)
		entry, _ := m["entry"].(map[string]any)
		if entry != nil && entry["id"] == id {
			return m
		}
		if child := treeNodeByID(t, m["children"], id); child != nil {
			return child
		}
	}
	return nil
}

// transcriptTexts is the text of every message in one recorded provider
// call's transcript (h.transcripts), read under the host lock.
func (h *testHost) transcriptTexts(call int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if call >= len(h.transcripts) {
		return nil
	}
	var out []string
	for _, m := range h.transcripts[call].Messages {
		out = append(out, ai.MessageText(m))
	}
	return out
}

func TestForkToRootUserMessageResetsLeafAndSeedsEditor(t *testing.T) {
	h := newTestHost(t, textScript("a1"), textScript("a2"), textScript("a3"))
	runTwoExchanges(t, h)

	// The selector's candidates: both user messages, in file order.
	fm := dataMap(t, response(t, h.serve(t, cmd("get_fork_messages", nil)), "get_fork_messages"))
	msgs, _ := fm["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("fork messages = %d, want 2", len(msgs))
	}
	if got := msgs[0].(map[string]any)["text"]; got != "one" {
		t.Errorf("first candidate text = %v, want one", got)
	}
	if got := msgs[1].(map[string]any)["text"]; got != "two" {
		t.Errorf("second candidate text = %v, want two", got)
	}
	firstUserID := msgs[0].(map[string]any)["entryId"].(string)

	// Fork to the first user message: it is a root, so the leaf lands in
	// the before-any-entry position — leafId null, not "" — and the
	// message's text comes back for the editor.
	fk := dataMap(t, response(t, h.serve(t, cmd("fork", map[string]any{"entryId": firstUserID})), "fork"))
	if fk["leafId"] != nil {
		t.Errorf("leafId = %v, want null (a root user message resets the leaf)", fk["leafId"])
	}
	if fk["editorText"] != "one" {
		t.Errorf("editorText = %v, want one", fk["editorText"])
	}

	// A follow-up prompt from the reset leaf starts a second root: the
	// forest has two trees now, the abandoned branch and the new one.
	h.serve(t, cmd("prompt", map[string]any{"message": "three"}))
	gt := dataMap(t, response(t, h.serve(t, cmd("get_tree", nil)), "get_tree"))
	tree, _ := gt["tree"].([]any)
	if len(tree) != 2 {
		t.Fatalf("tree roots = %d, want 2 (the fork opened a new root)", len(tree))
	}
	if gt["leafId"] == nil {
		t.Error("leafId must be set once the new branch has entries")
	}

	// D2, observed on the wire: the third run's transcript is the fork
	// point's projection plus the new message — the abandoned tail
	// ("two", "a2") must not ride along.
	texts := h.transcriptTexts(2)
	if len(texts) != 1 || texts[0] != "three" {
		t.Errorf("third run transcript = %v, want just the new message", texts)
	}
}

func TestForkToNonUserMessageMovesLeafToTarget(t *testing.T) {
	h := newTestHost(t, textScript("a1"), textScript("a2"), textScript("a3"))
	runTwoExchanges(t, h)
	ids := entryIDs(t, h.serve(t, cmd("get_entries", nil)), "get_entries")
	// File order: user one, assistant one, user two, assistant two.
	if len(ids) != 4 {
		t.Fatalf("entries = %d, want 4", len(ids))
	}
	assistantOne := ids[1]

	fk := dataMap(t, response(t, h.serve(t, cmd("fork", map[string]any{"entryId": assistantOne})), "fork"))
	if fk["leafId"] != assistantOne {
		t.Errorf("leafId = %v, want the assistant entry %s", fk["leafId"], assistantOne)
	}
	if _, present := fk["editorText"]; present {
		t.Errorf("editorText = %v, want absent (pi drops an undefined selectedText)", fk["editorText"])
	}

	// The next prompt appends as a child of the fork point: one root, and
	// the fork point carries two children — the abandoned tail and the
	// new branch.
	h.serve(t, cmd("prompt", map[string]any{"message": "three"}))
	gt := dataMap(t, response(t, h.serve(t, cmd("get_tree", nil)), "get_tree"))
	tree, _ := gt["tree"].([]any)
	if len(tree) != 1 {
		t.Fatalf("tree roots = %d, want 1", len(tree))
	}
	node := treeNodeByID(t, gt["tree"], assistantOne)
	if node == nil {
		t.Fatalf("fork point %s missing from the tree", assistantOne)
	}
	children, _ := node["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("fork point children = %d, want 2 (abandoned tail + new branch)", len(children))
	}
	if gt["leafId"] == nil {
		t.Error("leafId must be set on the new branch")
	}

	// D2 again: the third run sees the prefix up to the fork point, not
	// the abandoned second exchange.
	texts := h.transcriptTexts(2)
	if len(texts) != 3 || texts[0] != "one" || texts[1] != "a1" || texts[2] != "three" {
		t.Errorf("third run transcript = %v, want [one a1 three]", texts)
	}
}

func TestForkNoOpAndUnknownEntry(t *testing.T) {
	h := newTestHost(t, textScript("a1"), textScript("a2"))
	runTwoExchanges(t, h)
	leaf := dataMap(t, response(t, h.serve(t, cmd("get_entries", nil)), "get_entries"))["leafId"]

	// Forking to the current leaf moves nothing (pi's early return): the
	// leaf comes back and no editor text.
	fk := dataMap(t, response(t, h.serve(t, cmd("fork", map[string]any{"entryId": leaf})), "fork"))
	if fk["leafId"] != leaf {
		t.Errorf("leafId = %v, want the unchanged leaf %v", fk["leafId"], leaf)
	}
	if _, present := fk["editorText"]; present {
		t.Errorf("editorText = %v, want absent for a no-op", fk["editorText"])
	}

	// An unknown id is pi's wire literal, verbatim.
	bad := response(t, h.serve(t, cmd("fork", map[string]any{"entryId": "nope"})), "fork")
	if bad["success"] != false {
		t.Error("an unknown entry id must fail")
	} else if msg, _ := bad["error"].(string); msg != "Invalid entry ID for forking" {
		t.Errorf("error = %q, want pi's literal", msg)
	}
}

func TestGetTreeCarriesEntryBodiesAndLeaf(t *testing.T) {
	h := newTestHost(t, textScript("a1"))
	h.serve(t, cmd("prompt", map[string]any{"message": "one"}))

	gt := dataMap(t, response(t, h.serve(t, cmd("get_tree", nil)), "get_tree"))
	tree, _ := gt["tree"].([]any)
	if len(tree) != 1 {
		t.Fatalf("tree roots = %d, want 1", len(tree))
	}
	root := tree[0].(map[string]any)
	entry, _ := root["entry"].(map[string]any)
	if entry == nil {
		t.Fatal("the root node carries no entry")
	}
	if entry["type"] != "message" {
		t.Errorf("entry type = %v, want message", entry["type"])
	}
	// The entry body rides on the wire: SessionEntry.Extra is json:"-",
	// so a struct marshal would have dropped it (pi's node is the whole
	// line, and get_entries was fixed the same way).
	msg, _ := entry["message"].(map[string]any)
	if msg == nil || msg["role"] != "user" {
		t.Fatalf("entry message = %v, want a user message", entry["message"])
	}
	children, _ := root["children"].([]any)
	if len(children) != 1 {
		t.Fatalf("root children = %d, want 1 (the assistant reply)", len(children))
	}
	child := children[0].(map[string]any)
	childEntry, _ := child["entry"].(map[string]any)
	if childEntry == nil || childEntry["id"] != gt["leafId"] {
		t.Errorf("leafId = %v, want the assistant entry %v", gt["leafId"], childEntry["id"])
	}
}

func TestCloneWritesBranchedFileAndItResumes(t *testing.T) {
	h := newTestHost(t, textScript("a1"))
	h.useFileSession(t, [2]string{"kept question", "kept answer"})
	src := h.Session().SessionFile()
	if src == "" {
		t.Fatal("useFileSession must leave a file behind")
	}

	path, _ := dataMap(t, response(t, h.serve(t, cmd("clone", nil)), "clone"))["sessionPath"].(string)
	if path == "" {
		t.Fatal("clone answered no sessionPath")
	}
	if path == src {
		t.Fatal("clone must write a new file, not answer the source's path")
	}

	// The extraction is the source's root→leaf path: reopening the clone
	// yields the same entries in the same order, header linked to the
	// file they came from (pi's parentSession).
	cloneMgr, err := session.OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession(clone): %v", err)
	}
	srcEntries := entryIDs(t, h.serve(t, cmd("get_entries", nil)), "get_entries")
	cloneIDs := make([]string, 0)
	for _, e := range cloneMgr.Entries() {
		cloneIDs = append(cloneIDs, e.ID)
	}
	if len(cloneIDs) != len(srcEntries) {
		t.Fatalf("clone entries = %v, want the source's %v", cloneIDs, srcEntries)
	}
	for i := range srcEntries {
		if cloneIDs[i] != srcEntries[i] {
			t.Errorf("clone entry %d = %s, want %s (ids preserved verbatim)", i, cloneIDs[i], srcEntries[i])
		}
	}
	if got := cloneMgr.Header().ParentSession; got != src {
		t.Errorf("clone parentSession = %q, want %q", got, src)
	}

	// And it is resumable through the protocol — the client's next step
	// after clone (pi rebinds its runtime here; this port's twin of
	// switch_session is resume_session).
	r := response(t, h.serve(t, cmd("resume_session", map[string]any{"sessionPath": path})), "resume_session")
	if r["success"] != true {
		t.Fatalf("resume_session(clone) = %v", r)
	}
	after := entryIDs(t, h.serve(t, cmd("get_entries", nil)), "get_entries")
	if len(after) != len(srcEntries) {
		t.Errorf("resumed entries = %v, want %v", after, srcEntries)
	}
}

func TestCloneErrors(t *testing.T) {
	// No entries at all: pi's literal for the no-leaf case (rpc-mode.ts:622).
	h := newTestHost(t)
	bad := response(t, h.serve(t, cmd("clone", nil)), "clone")
	if bad["success"] != false {
		t.Error("cloning an empty session must fail")
	} else if msg, _ := bad["error"].(string); msg != "Cannot clone session: no current entry selected" {
		t.Errorf("error = %q, want pi's literal", msg)
	}

	// Entries but no file behind them (the in-memory default): the
	// extraction's own refusal, surfaced verbatim.
	h.serve(t, cmd("prompt", map[string]any{"message": "one"}))
	bad = response(t, h.serve(t, cmd("clone", nil)), "clone")
	if bad["success"] != false {
		t.Error("cloning a session with no file must fail")
	} else if msg, _ := bad["error"].(string); msg != "session: cannot branch a session with no file behind it" {
		t.Errorf("error = %q, want the extraction's refusal", msg)
	}
}
