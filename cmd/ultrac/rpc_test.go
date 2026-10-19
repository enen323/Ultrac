package main

// rpc_test.go — the rpcHost half of P-D0: resuming a real transcript
// restores what the file carries, and a refused resume leaves no
// half-state behind.
//
// The protocol half (command shapes, error responses, the pump rebind) is
// tested in internal/rpc against a scripted host; what is under test here
// is the harness wiring — the model fallback, the thinking-level restore,
// the abort→swap ordering — over real session files. The fake provider
// from print_test.go is wired in, so nothing reaches the network.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// writeResumableSession writes what a real run leaves behind: session
// state (model_change, thinking_level_change) followed by one exchange,
// with the assistant message carrying the model it was answered with —
// the context projection reads the model off the last assistant message
// when no model_change follows it, so a transcript without one would
// project an empty model ref.
func writeResumableSession(t *testing.T, cwd, provider, modelID, level, prompt, answer string) string {
	t.Helper()
	m := session.CreateSession(cwd)
	if _, err := m.AppendModelChange(provider, modelID); err != nil {
		t.Fatalf("AppendModelChange: %v", err)
	}
	if _, err := m.AppendThinkingLevelChange(level); err != nil {
		t.Fatalf("AppendThinkingLevelChange: %v", err)
	}
	for _, msg := range []ai.Message{
		{
			Role:        ai.RoleUser,
			Content:     []ai.ContentBlock{ai.TextBlock(prompt)},
			TimestampMS: time.Now().UnixMilli(),
		},
		{
			Role:        ai.RoleAssistant,
			Content:     []ai.ContentBlock{ai.TextBlock(answer)},
			API:         "anthropic-messages",
			Provider:    provider,
			Model:       modelID,
			StopReason:  ai.StopStop,
			TimestampMS: time.Now().UnixMilli(),
		},
	} {
		if _, err := m.AppendMessage(msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	return m.SessionFile()
}

// resumeHost assembles a harness and its RPC host over the print sandbox.
func resumeHost(t *testing.T) (*rpcHost, *printEnv) {
	t.Helper()
	env := newPrintEnv(t)
	h, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	return newRPCHost(h, context.Background()), env
}

func TestRPCHostResumeSessionRestoresFileState(t *testing.T) {
	host, env := resumeHost(t)
	path := writeResumableSession(t, env.cwd, "deepseek", "deepseek-reasoner", "high",
		"earlier question", "earlier answer")
	before := host.Session().SessionID()

	if err := host.ResumeSession(path); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	if host.Session().SessionID() == before {
		t.Error("resume must load another session identity")
	}
	if host.Session().SessionFile() != path {
		t.Errorf("sessionFile = %q, want %q", host.Session().SessionFile(), path)
	}

	// The transcript is replayed into the live agent. The seeded
	// tool-loadout system message leads it: this synthetic file has no
	// system entry of its own, and NewAgent declares the tools exactly as
	// pi's createMutableAgentState does when the transcript does not
	// start with one (agent.ts:82-111) — without it the next request
	// would go out with no tool declarations.
	msgs := host.Agent().State().Messages
	if len(msgs) != 3 {
		t.Fatalf("replayed %d messages, want 3 (system + the exchange)", len(msgs))
	}
	if msgs[0].Role != ai.RoleSystem {
		t.Errorf("first message role = %q, want system (the tool loadout)", msgs[0].Role)
	}
	if ai.MessageText(msgs[1]) != "earlier question" || ai.MessageText(msgs[2]) != "earlier answer" {
		t.Errorf("replayed = %q / %q", ai.MessageText(msgs[1]), ai.MessageText(msgs[2]))
	}

	// The file's model wins over the harness's — the conversation was held
	// with it — and the thinking level rides along (deepseek-reasoner is a
	// reasoning model, so the clamp keeps high).
	if got := host.Agent().State().Model.ID; got != "deepseek-reasoner" {
		t.Errorf("model = %q, want the session's deepseek-reasoner", got)
	}
	if got := host.Agent().State().ThinkingLevel; got != agent.ThinkingLevel("high") {
		t.Errorf("thinkingLevel = %q, want high (restored from the file)", got)
	}
}

func TestRPCHostResumeSessionKeepsHarnessModelWhenUnselectable(t *testing.T) {
	host, env := resumeHost(t)
	// A transcript naming a model this process cannot select. resolveModel
	// does not fail on an unknown provider/id, so the gate has to be the
	// available-models check; switching to a phantom would only surface as
	// a failed request on the next turn.
	path := writeResumableSession(t, env.cwd, "ghost", "ghost-1", "off",
		"old question", "old answer")

	if err := host.ResumeSession(path); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if got := host.Agent().State().Model.ID; got != "deepseek-chat" {
		t.Errorf("model = %q, want the harness's deepseek-chat", got)
	}
	// The replay still happened: the resume is useful even when the model
	// could not be restored. (Three messages: the seeded tool loadout
	// plus the exchange, same as the restore test.)
	if len(host.Agent().State().Messages) != 3 {
		t.Errorf("replayed %d messages, want 3", len(host.Agent().State().Messages))
	}
}

func TestRPCHostResumeSessionRejectsInvalidFile(t *testing.T) {
	host, _ := resumeHost(t)
	// Entries without a header: the shape OpenSession refuses.
	headerless := filepath.Join(t.TempDir(), "headerless.jsonl")
	if err := os.WriteFile(headerless,
		[]byte(`{"type":"message","id":"x1","parentId":null,"timestamp":"2026-10-19T08:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"orphan"}]}}`+"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	liveBefore := host.Session().SessionID()
	err := host.ResumeSession(headerless)
	if err == nil {
		t.Fatal("ResumeSession on a headerless file must fail")
	}
	if !strings.Contains(err.Error(), "not a valid session file") {
		t.Errorf("error = %v, want the open failure", err)
	}
	// A refused resume is a no-op: the live pair stays exactly as it was.
	if host.Session().SessionID() != liveBefore {
		t.Error("a failed resume must not swap the live session")
	}
}

func TestRPCHostListSessions(t *testing.T) {
	host, env := resumeHost(t)
	older := writeResumableSession(t, env.cwd, "deepseek", "deepseek-chat", "off",
		"the older question", "the older answer")
	newer := writeResumableSession(t, env.cwd, "deepseek", "deepseek-chat", "off",
		"the newer question", "the newer answer")

	// mtime is the recency the listing sorts by; the two files are written
	// within the same millisecond often enough that intent has to be set.
	now := time.Now()
	if err := os.Chtimes(older, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now, now); err != nil {
		t.Fatal(err)
	}

	list, err := host.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(list))
	}
	if list[0].Path != newer {
		t.Errorf("newest first: got %q, want %q", list[0].Path, newer)
	}
	if list[0].Title != "the newer question" {
		t.Errorf("title = %q, want the first user message", list[0].Title)
	}
	if list[0].MessageCount != 2 {
		t.Errorf("messageCount = %d, want 2 (user + assistant)", list[0].MessageCount)
	}
	if list[1].Path != older {
		t.Errorf("second = %q, want %q", list[1].Path, older)
	}
}
