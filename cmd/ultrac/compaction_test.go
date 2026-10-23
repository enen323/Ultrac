package main

// compaction_test.go — the P-C2 acceptance, end to end against the real
// harness with a fake provider (E-C4/E-C5 of docs/compaction-branch-plan.md).
//
// The plan's P2/P5 gate carries through: no request leaves the process. What
// is under test is the wiring the protocol-layer tests cannot see — the
// PrepareRequest hook firing on a real loop, the checkpoint landing in a real
// session file, the agent state swap, and the settings gate persisting and
// re-reading.
//
// Sizes are chosen against deepseek-chat's catalog window (128000 tokens)
// and pi's defaults (reserve 16384, keepRecent 20000): the trigger line sits
// at ~111,600 tokens (~390KB of text at the 3.5 chars/token estimate), and
// the keep-recent window at ~20,000 tokens (~70KB). A 140KB message is
// ~40,000 estimated tokens — three of them cross the line, and one 80KB
// message (~23,000 tokens) is enough for a manual compaction to have
// something to summarize.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/compact"
	"ultrac/internal/config"
	"ultrac/internal/rpc"
	"ultrac/internal/session"
	"ultrac/internal/transport"
)

// bigText builds a large text block of approximately kb kilobytes.
func bigText(kb int) string { return strings.Repeat("compact-me ", kb*128) }

// textOnlyScript builds a plain text response WITHOUT usage: the estimate
// must stay character-based, the way a provider that reports no usage leaves
// it. (A scripted usage would make EstimateContextTokens trust a fake exact
// prefix and the threshold math would silently change.)
func textOnlyScript(text string) []ai.AssistantEvent {
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
		Provider:   "deepseek",
		Model:      "deepseek-chat",
		StopReason: ai.StopStop,
	}
	return []ai.AssistantEvent{
		ai.EvStart{Partial: partial("")},
		ai.EvTextStart{ContentIndex: 0, Partial: partial("")},
		ai.EvTextDelta{ContentIndex: 0, Delta: text, Partial: partial(text)},
		ai.EvTextEnd{ContentIndex: 0, Content: text, Partial: partial(text)},
		ai.EvDone{Reason: ai.StopStop, Message: final},
	}
}

// abortedScript is a terminal aborted-error stream: what a provider emits
// when the request is cancelled mid-flight.
func abortedScript() []ai.AssistantEvent {
	m := ai.Message{
		Role:       ai.RoleAssistant,
		API:        "anthropic-messages",
		Provider:   "deepseek",
		Model:      "deepseek-chat",
		StopReason: ai.StopAborted,
		Usage:      &ai.Usage{},
	}
	return []ai.AssistantEvent{
		ai.EvStart{Partial: m},
		ai.EvError{Reason: ai.StopAborted, Error: m},
	}
}

// parseJSONLines parses a print-json stdout stream.
func parseJSONLines(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line is not JSON (%q): %v", l, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// eventIndex returns the index of the first line whose type matches and
// whose extra predicate holds, or -1.
func eventIndex(lines []map[string]any, typ string, pred func(map[string]any) bool) int {
	for i, l := range lines {
		if l["type"] != typ {
			continue
		}
		if pred == nil || pred(l) {
			return i
		}
	}
	return -1
}

// compactionEntry extracts the single compaction entry of a session, failing
// the test when there is not exactly one.
func compactionEntry(t *testing.T, mgr *session.SessionManager) session.SessionEntry {
	t.Helper()
	var found []session.SessionEntry
	for _, e := range mgr.Entries() {
		if e.Type == session.TypeCompaction {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("session carries %d compaction entries, want 1", len(found))
	}
	return found[0]
}

// entryIDWithText finds the NEWEST entry whose context message carries
// text — the one a cut point keeps when several entries match.
func entryIDWithText(t *testing.T, mgr *session.SessionManager, text string) string {
	t.Helper()
	var last string
	for _, e := range mgr.Entries() {
		for _, m := range session.EntryContextMessages(&e) {
			if strings.Contains(ai.MessageText(m), text) {
				last = e.ID
			}
		}
	}
	if last == "" {
		t.Fatalf("no entry carries the text %q", truncateForError(text))
	}
	return last
}

func truncateForError(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// E-C4: threshold auto-compaction, end to end
// ---------------------------------------------------------------------------

func TestAutoCompactionThresholdEndToEnd(t *testing.T) {
	env := newPrintEnv(t)
	env.flags.Mode = "json"

	// The containment probe: a slice of the big text that survives the
	// print path's TrimSpace (the flag layer strips the trailing separator
	// space, so the stored user message is one character shorter than the
	// scripted assistant text). Every assertion below asks "did this
	// content ride along", and a 1000-char slice answers it exactly.
	bigNeedle := bigText(140)[:1000]

	// Run 1: one big exchange. The hook sees only system + user1 here, so
	// nothing fires yet — but the session now holds ~80,000 estimated
	// tokens.
	env.flags.setPrompt(bigText(140))
	env.provider.scripts = [][]ai.AssistantEvent{textOnlyScript(bigText(140))}
	if code, _, stderr := env.run(); code != 0 {
		t.Fatalf("run 1 exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if len(env.provider.contexts) != 1 {
		t.Fatalf("run 1 made %d provider calls, want 1", len(env.provider.contexts))
	}

	// Run 2: the second big prompt pushes the projection past
	// window - reserve. Script 1 is the summarization call the hook makes;
	// script 2 is the answer to the compressed context.
	env.flags.Continue = true
	env.flags.setPrompt(bigText(140))
	env.provider.scripts = append(env.provider.scripts,
		textOnlyScript("CHECKPOINT-KEEP-DEPLOY-NOTES"),
		textOnlyScript("second answer"),
	)
	code, stdout, stderr := env.run()
	if code != 0 {
		t.Fatalf("run 2 exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	// ③ The events are paired and carry reason=threshold, between the
	// user prompt and the assistant answer of run 2.
	lines := parseJSONLines(t, stdout)
	start := eventIndex(lines, "compaction_start", func(m map[string]any) bool {
		return m["reason"] == "threshold"
	})
	end := eventIndex(lines, "compaction_end", func(m map[string]any) bool {
		return m["reason"] == "threshold"
	})
	if start < 0 || end < 0 || start > end {
		t.Fatalf("compaction events missing or misordered (start=%d end=%d): %v", start, end, eventTypes(lines))
	}
	if n := eventIndex(lines[:start], "compaction_start", nil); n >= 0 {
		t.Error("compaction_start appears twice")
	}
	// The pair sits after the user message and before the assistant's.
	var lastUserEnd = -1
	for i, l := range lines {
		if l["type"] == "message_end" {
			if msg, _ := l["message"].(map[string]any); msg != nil && msg["role"] == "user" {
				lastUserEnd = i
			}
		}
	}
	if lastUserEnd < 0 || start < lastUserEnd {
		t.Errorf("compaction_start at %d, want after the user message_end at %d", start, lastUserEnd)
	}
	endLine := lines[end]
	if endLine["aborted"] != false || endLine["willRetry"] != false {
		t.Errorf("compaction_end flags = %v/%v, want false/false", endLine["aborted"], endLine["willRetry"])
	}
	if _, has := endLine["errorMessage"]; has {
		t.Error("a successful compaction_end must not carry errorMessage")
	}
	result, _ := endLine["result"].(map[string]any)
	if result == nil {
		t.Fatalf("compaction_end carries no result: %v", endLine)
	}
	if summary, _ := result["summary"].(string); !strings.Contains(summary, "CHECKPOINT-KEEP-DEPLOY-NOTES") {
		t.Errorf("result.summary = %q, want the summarization call's text", summary)
	}
	if after, _ := result["estimatedTokensAfter"].(float64); after <= 0 || after >= 111616 {
		t.Errorf("estimatedTokensAfter = %v, want a positive number under the trigger line", after)
	}

	// ① The checkpoint landed in the session file, pointing at the entry
	// the compressed context keeps.
	files := env.sessionFiles()
	if len(files) != 1 {
		t.Fatalf("session files = %d, want 1 (continue appends)", len(files))
	}
	mgr, err := session.OpenSession(files[0])
	if err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	entry := compactionEntry(t, mgr)
	if got := entry.ExtraString("summary"); !strings.Contains(got, "CHECKPOINT-KEEP-DEPLOY-NOTES") {
		t.Errorf("entry summary = %q, want the checkpoint text", got)
	}
	// The cut keeps the newest big user message (the walk crosses
	// keepRecentTokens on it) and summarizes the older exchange away.
	if want := entryIDWithText(t, mgr, bigNeedle); entry.ExtraString("firstKeptEntryId") != want {
		t.Errorf("firstKeptEntryId = %q, want the newest big user entry %q",
			entry.ExtraString("firstKeptEntryId"), want)
	}
	var tokensBefore int
	if err := json.Unmarshal(entry.Extra["tokensBefore"], &tokensBefore); err != nil {
		t.Fatalf("tokensBefore is not a number: %s", entry.Extra["tokensBefore"])
	}
	if tokensBefore < 100000 {
		t.Errorf("tokensBefore = %d, want the pre-compression estimate (>= 100000)", tokensBefore)
	}

	// ② The request that went out after the compaction is the compressed
	// projection: the checkpoint framing, the kept tail, and none of the
	// summarized history.
	transcript := env.provider.transcript(2)
	var sawSummary bool
	for _, m := range transcript {
		text := ai.MessageText(m)
		if strings.Contains(text, "The conversation history before this point was compacted") &&
			strings.Contains(text, "CHECKPOINT-KEEP-DEPLOY-NOTES") {
			sawSummary = true
		}
		if strings.Contains(text, bigNeedle) && m.Role != ai.RoleUser {
			t.Error("the summarized assistant history rode along in the compressed request")
		}
	}
	if !sawSummary {
		t.Errorf("the compressed request carries no checkpoint summary: %d messages", len(transcript))
	}
	// The token watermark dropped below the line it crossed.
	if got := ai.EstimateContextTokens(transcript); got >= 111616 {
		t.Errorf("compressed request estimates %d tokens, want under the 111616 trigger line", got)
	}
	// And run 1's request was under the line — which is why the hook
	// correctly waited instead of compacting the first prompt.
	if got := ai.EstimateContextTokens(env.provider.transcript(0)); got >= 111616 {
		t.Errorf("run 1's request estimated %d tokens — it should have been under the line", got)
	}

	// ④ The agent state was swapped too: the NEXT prompt must not replay
	// the summarized history (the D2/R1 regression).
	env.flags.setPrompt("third")
	env.provider.scripts = append(env.provider.scripts, textOnlyScript("third answer"))
	if code, _, stderr := env.run(); code != 0 {
		t.Fatalf("run 3 exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	third := env.provider.transcript(3)
	for _, m := range third {
		if m.Role == ai.RoleAssistant && strings.Contains(ai.MessageText(m), bigNeedle) {
			t.Error("the next prompt replayed the summarized-away assistant history")
		}
	}
	var sawCheckpoint bool
	for _, m := range third {
		if strings.Contains(ai.MessageText(m), "CHECKPOINT-KEEP-DEPLOY-NOTES") {
			sawCheckpoint = true
		}
	}
	if !sawCheckpoint {
		t.Error("the next prompt does not carry the checkpoint — the agent state was not swapped")
	}
	// And the session now holds exactly one checkpoint: the third prompt
	// fits comfortably, so nothing fired again.
	mgr2, err := session.OpenSession(files[0])
	if err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	compactionEntry(t, mgr2)
}

// ---------------------------------------------------------------------------
// E-C5a: the manual compact command aborts a streaming run first
// ---------------------------------------------------------------------------

func TestManualCompactAbortsActiveRun(t *testing.T) {
	env := newPrintEnv(t)

	// The gated fake: call 0 answers run 1; call 1 (run 2's request)
	// blocks until the run is aborted; call 2 is the summarization the
	// manual compaction makes.
	var calls int32
	gate := make(chan struct{})
	streamFn := func(ctx context.Context, model ai.Model, tc ai.TranscriptContext, _ ai.StreamOptions) ai.Stream {
		idx := int(atomic.AddInt32(&calls, 1)) - 1
		switch idx {
		case 0:
			return ai.StreamOf(textOnlyScript("first answer")...)
		case 1:
			s := ai.NewEventStream()
			go func() {
				select {
				case <-gate:
					for _, ev := range textOnlyScript("never") {
						s.Push(ev)
					}
				case <-ctx.Done():
					for _, ev := range abortedScript() {
						s.Push(ev)
					}
				}
				s.End()
			}()
			return s
		default:
			return ai.StreamOf(textOnlyScript("MANUAL-CHECKPOINT")...)
		}
	}

	h, code := buildHarness(env.flags, env.cwd, io.Discard, streamFn)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}

	// The real server over a pipe: the command must arrive mid-stream,
	// exactly as a desktop client would send it.
	rec := &wireRecorder{respCh: make(chan map[string]any, 64)}
	writer := transport.NewPiJSONWriter(rec)
	h.emitEvent = writer.WriteEvent // compaction events ride the same wire
	host := newRPCHost(h, context.Background())
	server := rpc.NewServer(host, writer)
	defer server.Close()
	pr, pw := io.Pipe()
	go func() { _ = server.Serve(context.Background(), pr) }()
	t.Cleanup(func() { _ = pw.Close() })

	send := func(line string) {
		if _, err := fmt.Fprintln(pw, line); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	next := func() map[string]any {
		t.Helper()
		select {
		case m := <-rec.respCh:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("no response within 5s")
			return nil
		}
	}

	// Run 1 completes.
	send(cmdJSON("prompt", map[string]any{"id": "p1", "message": "first"}))
	waitFor(t, 5*time.Second, func() bool { return atomic.LoadInt32(&calls) >= 1 })
	_ = host.Agent().WaitForIdle(context.Background())
	if r := next(); r["command"] != "prompt" || r["success"] != true {
		t.Fatalf("prompt 1 response = %v", r)
	}

	// Run 2 carries a big user message and is held open at the provider.
	send(cmdJSON("prompt", map[string]any{"id": "p2", "message": bigText(80)}))
	waitFor(t, 5*time.Second, func() bool { return atomic.LoadInt32(&calls) >= 2 })
	if r := next(); r["command"] != "prompt" {
		t.Fatalf("prompt 2 response = %v", r)
	}
	if !host.Agent().State().IsStreaming {
		t.Fatal("precondition: the gated run should still be streaming")
	}

	// The compact command arrives mid-stream.
	send(cmdJSON("compact", map[string]any{"id": "c1"}))
	r := next()
	if r["command"] != "compact" || r["success"] != true {
		t.Fatalf("compact response = %v", r)
	}
	d, _ := r["data"].(map[string]any)
	if d == nil {
		t.Fatalf("compact response carries no data: %v", r)
	}
	if summary, _ := d["summary"].(string); !strings.Contains(summary, "MANUAL-CHECKPOINT") {
		t.Errorf("summary = %q, want the summarization call's text", summary)
	}
	if got, _ := d["firstKeptEntryId"].(string); got == "" {
		t.Error("firstKeptEntryId must name the kept entry")
	}

	// The aborted run settled: the wire shows the pair, and the checkpoint
	// landed pointing at the big user message (the newest cut point).
	waitFor(t, 5*time.Second, func() bool {
		return eventIndex(rec.snapshot(), "agent_settled", func(m map[string]any) bool {
			return m["aborted"] == true
		}) >= 0
	})
	_ = host.Agent().WaitForIdle(context.Background())

	mgr := host.Session()
	entry := compactionEntry(t, mgr)
	if want := entryIDWithText(t, mgr, bigText(80)); entry.ExtraString("firstKeptEntryId") != want {
		t.Errorf("firstKeptEntryId = %q, want the big user entry %q",
			entry.ExtraString("firstKeptEntryId"), want)
	}
	if got := entry.ExtraString("summary"); !strings.Contains(got, "MANUAL-CHECKPOINT") {
		t.Errorf("entry summary = %q, want the checkpoint text", got)
	}
	// The compaction events reached the wire too — the executor's sink is
	// the same writer the pump uses.
	lines := rec.snapshot()
	if eventIndex(lines, "compaction_start", func(m map[string]any) bool {
		return m["reason"] == "manual"
	}) < 0 {
		t.Error("compaction_start{manual} never reached the wire")
	}
	if eventIndex(lines, "compaction_end", func(m map[string]any) bool {
		return m["reason"] == "manual"
	}) < 0 {
		t.Error("compaction_end{manual} never reached the wire")
	}
}

// ---------------------------------------------------------------------------
// E-C5b: set_auto_compaction persists, re-reads, and gates the trigger
// ---------------------------------------------------------------------------

func TestSetAutoCompactionPersistsAndGates(t *testing.T) {
	env := newPrintEnv(t)
	host := newRPCHost(mustHarness(t, env), context.Background())

	// Flip the gate off through the host (what the RPC command calls).
	if err := host.SetAutoCompaction(false); err != nil {
		t.Fatalf("SetAutoCompaction: %v", err)
	}
	if host.AutoCompactionEnabled() {
		t.Error("the live gate did not flip")
	}

	// The write landed in the user settings layer — and did not clobber
	// the env block it shares the file with.
	data, err := os.ReadFile(filepath.Join(env.home, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	var section struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(doc["compaction"], &section); err != nil {
		t.Fatalf("settings.json has no compaction object: %s", data)
	}
	if section.Enabled == nil || *section.Enabled {
		t.Errorf("persisted enabled = %v, want false", section.Enabled)
	}
	if _, ok := doc["env"]; !ok {
		t.Errorf("the env block was lost on write: %s", data)
	}

	// A fresh resolver re-reads the gate: the persistence is real, not an
	// in-memory flip.
	r := config.NewFileResolver()
	s, err := r.Get(context.Background(), env.cwd)
	if err != nil {
		t.Fatal(err)
	}
	if s.Compaction == nil || s.Compaction.Enabled == nil || *s.Compaction.Enabled {
		t.Errorf("re-read compaction = %+v, want enabled:false", s.Compaction)
	}

	// With the gate off, a threshold-crossing pair of prompts does NOT
	// compact: one provider call per prompt, no checkpoint.
	env.provider.scripts = [][]ai.AssistantEvent{
		textOnlyScript(bigText(140)),
		textOnlyScript("off answer"),
	}
	drivePrompt(t, host, bigText(140))
	drivePrompt(t, host, bigText(140))
	if n := len(env.provider.contexts); n != 2 {
		t.Fatalf("provider calls = %d, want 2 (no summarization call)", n)
	}
	for _, e := range host.Session().Entries() {
		if e.Type == session.TypeCompaction {
			t.Fatal("a compaction entry landed with auto-compaction off")
		}
	}

	// Flip it back on and start a fresh session: the gate survives the
	// swap (the harness's resolved copy is updated, not just the live
	// executor), and the same content now compacts.
	if err := host.SetAutoCompaction(true); err != nil {
		t.Fatalf("SetAutoCompaction(true): %v", err)
	}
	host.NewSession("")
	env.provider.scripts = append(env.provider.scripts,
		textOnlyScript(bigText(140)),
		textOnlyScript("CHECKPOINT-ON"),
		textOnlyScript("on answer"),
	)
	drivePrompt(t, host, bigText(140))
	drivePrompt(t, host, bigText(140))

	if n := len(env.provider.contexts); n != 5 {
		t.Fatalf("provider calls = %d, want 5 (2 off + 3 on, the extra being the summarization)", n)
	}
	entry := compactionEntry(t, host.Session())
	if got := entry.ExtraString("summary"); !strings.Contains(got, "CHECKPOINT-ON") {
		t.Errorf("entry summary = %q, want the checkpoint text", got)
	}
}

// ---------------------------------------------------------------------------
// Settings resolution
// ---------------------------------------------------------------------------

func TestResolveCompactionSettings(t *testing.T) {
	// No section at all: pi's defaults, byte for byte.
	got := resolveCompactionSettings(nil, io.Discard)
	if want := compact.DefaultSettings(); got != want {
		t.Errorf("nil section resolved to %+v, want the defaults %+v", got, want)
	}

	// A partial section keeps the defaults for the keys it does not name.
	keep := 1000
	got = resolveCompactionSettings(&config.CompactionSettings{KeepRecentTokens: &keep}, io.Discard)
	if got.KeepRecentTokens != 1000 {
		t.Errorf("keepRecentTokens = %d, want 1000", got.KeepRecentTokens)
	}
	if got.ReserveTokens != 16384 || !got.Enabled {
		t.Errorf("unnamed keys did not keep their defaults: %+v", got)
	}

	// An explicit false survives — that is the whole point of the pointer.
	off := false
	got = resolveCompactionSettings(&config.CompactionSettings{Enabled: &off}, io.Discard)
	if got.Enabled {
		t.Error("enabled:false resolved to true")
	}

	// A negative budget is refused with a warning and falls back to the
	// default: the config layer's validate() normally rejects it first,
	// but buildHarness resolves from raw layers, so the runtime keeps the
	// last word.
	var stderr strings.Builder
	neg := -5
	got = resolveCompactionSettings(&config.CompactionSettings{ReserveTokens: &neg}, &stderr)
	if got.ReserveTokens != 16384 {
		t.Errorf("reserveTokens = %d, want the 16384 default after the refusal", got.ReserveTokens)
	}
	if !strings.Contains(stderr.String(), "reserveTokens") {
		t.Errorf("no warning was printed for the negative budget: %q", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mustHarness assembles the sandbox harness the way print mode does.
func mustHarness(t *testing.T, env *printEnv) *harness {
	t.Helper()
	h, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	return h
}

// drivePrompt runs one prompt through the host the way print mode does:
// persist completed messages to the session, then wait for the run to
// settle. It is the direct-drive equivalent of a prompt command.
func drivePrompt(t *testing.T, host *rpcHost, text string) {
	t.Helper()
	a := host.Agent()
	mgr := host.Session()
	unsubscribe := a.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		// The RPC pump's persist rule, applied by hand: every completed
		// message of a persistable role lands in the session, so the
		// compaction hook sees a branch as complete as the wire mode's.
		if e, ok := ev.(agent.EvMessageEnd); ok && persistable(e.Message.Role) {
			if _, err := mgr.AppendMessage(e.Message); err != nil {
				t.Errorf("persist: %v", err)
			}
		}
		return nil
	})
	defer unsubscribe()
	in, err := host.BuildPrompt(text, nil)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	msgs := []ai.Message{in.User}
	if in.Patch != nil {
		msgs = append([]ai.Message{*in.Patch}, msgs...)
	}
	if err := a.Prompt(host.RunContext(), msgs...); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := a.WaitForIdle(host.RunContext()); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
}

// cmdJSON builds one RPC command line.
func cmdJSON(typ string, fields map[string]any) string {
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
	t.Fatal("condition not met within timeout")
}

// wireRecorder captures every line the server writes, in arrival order,
// and routes responses to a channel for the driver.
type wireRecorder struct {
	mu     sync.Mutex
	lines  []map[string]any
	respCh chan map[string]any
}

func (w *wireRecorder) Write(p []byte) (int, error) {
	line := strings.TrimSuffix(string(p), "\n")
	if line == "" {
		return len(p), nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return len(p), nil
	}
	w.mu.Lock()
	w.lines = append(w.lines, m)
	w.mu.Unlock()
	if m["type"] == "response" {
		select {
		case w.respCh <- m:
		default:
		}
	}
	return len(p), nil
}

func (w *wireRecorder) snapshot() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.lines...)
}

// eventTypes lists the discriminants of a parsed stream, for failure
// messages.
func eventTypes(lines []map[string]any) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, fmt.Sprint(l["type"]))
	}
	return out
}
