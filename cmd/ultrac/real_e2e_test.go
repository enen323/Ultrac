//go:build reale2e

package main

// The P-C2 real-endpoint acceptance (plan: "真实端点手测").
//
// Behind the reale2e build tag on purpose: it spends real tokens against
// DeepSeek, so it must never run as part of `go test ./...`. Run it by hand:
//
//	go test -tags reale2e ./cmd/ultrac -run TestRealEndpoint -v -count=1
//
// It needs the repo-root .env (the project convention) and network; the
// first run's evidence is recorded in docs/compaction-branch-plan.md under
// "P-C2 落地记录".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/session"
	"ultrac/internal/transport"
)

// realEnv is a sandbox whose settings point at the real DeepSeek endpoint
// with the key from the repo's ./.env (the project convention).
type realEnv struct {
	t     *testing.T
	cwd   string
	home  string
	flags *agentFlags
}

func newRealEnv(t *testing.T) *realEnv {
	t.Helper()
	key := readRepoAPIKey(t)
	home := t.TempDir()
	cwd := t.TempDir()
	settings := `{
      "env": {
        "ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
        "ANTHROPIC_AUTH_TOKEN": "` + key + `",
        "ANTHROPIC_MODEL": "deepseek-chat"
      }
    }`
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	return &realEnv{
		t:    t,
		cwd:  cwd,
		home: home,
		// Continue resumes the seeded session: without it the harness
		// opens a fresh empty one and there is nothing to compact.
		flags: &agentFlags{Mode: "text", Model: "deepseek/deepseek-chat", Continue: true},
	}
}

func readRepoAPIKey(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../.env")
	if err != nil {
		t.Skipf("no .env at the repo root: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "apiKey:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "apiKey:"))
		}
	}
	t.Skip(".env carries no apiKey: line")
	return ""
}

// seedBigSession writes a session whose projection is necessarily over the
// compaction line: ~420KB of varied text across six messages (~20,000
// estimated tokens each) ≈ 120,000 tokens against deepseek-chat's 128,000
// window and the 16,384 reserve (line at 111,616). The text is varied on
// purpose — a wall of one repeated token makes the summarizer loop into its
// output cap, which is a素材 problem, not an engine one.
func seedBigSession(t *testing.T, cwd string) *session.SessionManager {
	t.Helper()
	m := session.CreateSession(cwd)
	if _, err := m.AppendModelChange("deepseek", "deepseek-chat"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		for _, msg := range []ai.Message{
			{
				Role:        ai.RoleUser,
				Content:     []ai.ContentBlock{ai.TextBlock(variedText(70, i*2))},
				TimestampMS: time.Now().UnixMilli() + int64(i)*1000,
			},
			{
				Role:        ai.RoleAssistant,
				Content:     []ai.ContentBlock{ai.TextBlock(variedText(70, i*2+1))},
				API:         "anthropic-messages",
				Provider:    "deepseek",
				Model:       "deepseek-chat",
				StopReason:  ai.StopStop,
				TimestampMS: time.Now().UnixMilli() + int64(i)*1000 + 500,
			},
		} {
			if _, err := m.AppendMessage(msg); err != nil {
				t.Fatal(err)
			}
		}
	}
	return m
}

// variedText builds ~kb kilobytes of natural-looking text: numbered
// paragraphs with distinct content, so a summarizer has structure to
// compress instead of a loop to fall into.
func variedText(kb, seed int) string {
	var b strings.Builder
	topics := []string{
		"the Fujian BSS rollout", "the kite provider registry", "the spool order pipeline",
		"the session projection rules", "the compaction cut-point walk", "the desktop shell lease",
		"the RPC command surface", "the settings layer merge", "the token estimate heuristic",
	}
	for i := 0; b.Len() < kb*1024; i++ {
		fmt.Fprintf(&b, "Section %d.%d concerns %s. The team recorded that step %d depends on the previous checkpoint, and the review noted the tradeoff between a strict merge and a per-key override. Remaining risk: the estimate drifts by a few percent on long transcripts.\n\n",
			seed, i, topics[(seed+i)%len(topics)], i)
	}
	return b.String()
}

func TestRealEndpointAutoCompaction(t *testing.T) {
	env := newRealEnv(t)
	seedBigSession(t, env.cwd)

	h, code := buildHarness(env.flags, env.cwd, io.Discard, nil) // nil = the real registry
	if code != 0 {
		t.Fatalf("buildHarness exit = %d", code)
	}
	// Compaction events are emitted above the agent, so they ride the
	// harness's wire sink, not the agent's subscribers — that is
	// production's arrangement (print-json / rpc set h.emitEvent before the
	// first agent exists). Wiring a real PiJSONWriter here also checks the
	// wire shape from plan §5.4 instead of the Go structs.
	var wire bytes.Buffer
	writer := transport.NewPiJSONWriter(&wire)
	h.emitEvent = writer.WriteEvent
	host := newRPCHost(h, context.Background())

	var events []agent.AgentEvent
	a := host.Agent()
	unsub := a.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		events = append(events, ev)
		return nil
	})
	defer unsub()

	in, err := host.BuildPrompt("这个会话是关于什么的？用一句中文回答。", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = a.Prompt(context.Background(), in.User)
	_ = a.WaitForIdle(context.Background())
	t.Logf("run took %s", time.Since(start))
	t.Logf("compaction wire:\n%s", wire.String())

	// The threshold hook fired: a real summarization produced a real
	// checkpoint, and the pair is on the wire in pi's shape.
	type wireEvent struct {
		Type         string          `json:"type"`
		Reason       string          `json:"reason"`
		Aborted      *bool           `json:"aborted"`
		WillRetry    *bool           `json:"willRetry"`
		ErrorMessage *string         `json:"errorMessage"`
		Result       json.RawMessage `json:"result"`
	}
	var starts, ends int
	var end wireEvent
	for _, line := range strings.Split(wire.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev wireEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("compaction wire line is not JSON: %q: %v", line, err)
		}
		switch ev.Type {
		case "compaction_start":
			starts++
			if ev.Reason != "threshold" {
				t.Errorf("reason = %q, want threshold", ev.Reason)
			}
		case "compaction_end":
			ends++
			end = ev
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("compaction events: %d starts, %d ends, want 1/1", starts, ends)
	}
	if end.Reason != "threshold" {
		t.Errorf("end reason = %q, want threshold", end.Reason)
	}
	if end.Aborted == nil || *end.Aborted {
		t.Errorf("end aborted = %v, want false", end.Aborted)
	}
	if end.WillRetry == nil || *end.WillRetry {
		t.Errorf("end willRetry = %v, want false", end.WillRetry)
	}
	if end.ErrorMessage != nil {
		t.Errorf("end errorMessage = %q, want absent", *end.ErrorMessage)
	}
	if len(end.Result) == 0 {
		t.Fatal("compaction_end carried no result")
	}
	var result struct {
		Summary              string `json:"summary"`
		FirstKeptEntryID     string `json:"firstKeptEntryId"`
		TokensBefore         int    `json:"tokensBefore"`
		EstimatedTokensAfter int    `json:"estimatedTokensAfter"`
	}
	if err := json.Unmarshal(end.Result, &result); err != nil {
		t.Fatalf("result is not the compaction result: %s: %v", end.Result, err)
	}
	if result.TokensBefore < 111616 || result.EstimatedTokensAfter >= result.TokensBefore {
		t.Errorf("tokens %d → %d, want a drop below the 111616 line",
			result.TokensBefore, result.EstimatedTokensAfter)
	}
	if len(result.Summary) < 50 || result.FirstKeptEntryID == "" {
		t.Errorf("result summary=%d chars firstKeptEntryId=%q, want a real checkpoint",
			len(result.Summary), result.FirstKeptEntryID)
	}

	// The checkpoint is on disk with a real summary. The assertion reads
	// the harness's own manager (seedBigSession's instance is a different
	// handle on the same file and would never see an append made through
	// this one), then re-opens the file to prove the entry is durable.
	path := host.Session().SessionFile()
	reopened, err := session.OpenSession(path)
	if err != nil {
		t.Fatalf("reopen %s: %v", path, err)
	}
	var compaction *session.SessionEntry
	for _, e := range reopened.Entries() {
		if e.Type == session.TypeCompaction {
			e := e
			compaction = &e
		}
	}
	if compaction == nil {
		t.Fatal("no compaction entry in the session file")
	}
	summary := compaction.ExtraString("summary")
	t.Logf("summary (%d chars): %s", len(summary), realTruncate(summary, 300))
	if len(summary) < 50 {
		t.Errorf("summary = %q, want a real checkpoint", summary)
	}
	if !strings.Contains(summary, "<summary>") && strings.Contains(summary, "Goal") {
		t.Logf("(summary shape noted)")
	}

	// The answer came back — the compressed request was accepted by the
	// real endpoint.
	answer := ""
	for _, ev := range events {
		if e, ok := ev.(agent.EvMessageEnd); ok && e.Message.Role == ai.RoleAssistant {
			answer = ai.MessageText(e.Message)
		}
	}
	t.Logf("answer: %s", realTruncate(answer, 200))
	if answer == "" {
		t.Error("no assistant answer after the compaction")
	}

	// The agent state carries the checkpoint, not the old history.
	state := host.Agent().State().Messages
	var sawCheckpoint bool
	for _, m := range state {
		if strings.Contains(ai.MessageText(m), "The conversation history before this point was compacted") {
			sawCheckpoint = true
		}
	}
	if !sawCheckpoint {
		t.Error("the live transcript does not carry the checkpoint framing")
	}
}

func TestRealEndpointManualCompactAndGate(t *testing.T) {
	env := newRealEnv(t)
	seedBigSession(t, env.cwd)

	h, code := buildHarness(env.flags, env.cwd, io.Discard, nil)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d", code)
	}
	host := newRPCHost(h, context.Background())

	// The gate reports on by default.
	if !host.AutoCompactionEnabled() {
		t.Error("autoCompactionEnabled should default to true")
	}

	// Manual compaction: a real summarization over the seeded branch.
	result, err := host.Compact("")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	t.Logf("manual compact: tokensBefore=%d after=%d summary=%d chars",
		result.TokensBefore, result.EstimatedTokensAfter, len(result.Summary))
	if result.TokensBefore < 111616 {
		t.Errorf("tokensBefore = %d, want over the 111616 line", result.TokensBefore)
	}
	if len(result.Summary) < 50 {
		t.Errorf("summary = %q, want a real checkpoint", result.Summary)
	}
	if result.FirstKeptEntryID == "" {
		t.Error("firstKeptEntryId is empty")
	}

	// A second manual compaction is the idempotence error (the branch now
	// ends in a checkpoint... well, the agent state was swapped, but the
	// branch's newest entry is the compaction itself).
	if _, err := host.Compact(""); err == nil {
		t.Error("a second compact should fail")
	} else {
		t.Logf("second compact error: %v", err)
	}

	// The gate persists and re-reads.
	if err := host.SetAutoCompaction(false); err != nil {
		t.Fatalf("SetAutoCompaction: %v", err)
	}
	if host.AutoCompactionEnabled() {
		t.Error("live gate did not flip")
	}
	data, err := os.ReadFile(filepath.Join(env.home, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"compaction"`) {
		t.Errorf("settings.json has no compaction section: %s", redactKey(string(data)))
	}
	t.Logf("settings.json: %s", redactKey(string(data)))

	// And the session now holds exactly one checkpoint.
	n := 0
	for _, e := range host.Session().Entries() {
		if e.Type == session.TypeCompaction {
			n++
		}
	}
	if n != 1 {
		t.Errorf("session carries %d compaction entries, want 1", n)
	}
}

func realTruncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// redactKey masks token-looking secrets before a settings dump reaches the
// test log — this file reads the real key from ./.env, and a logged key is
// a leaked key.
func redactKey(s string) string {
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "_TOKEN") || strings.Contains(line, "_KEY") {
			if i := strings.Index(line, `"`); i >= 0 {
				if j := strings.LastIndex(line, `"`); j > i {
					line = line[:i+1] + "***redacted***" + line[j:]
				}
			}
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}
