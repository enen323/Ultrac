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
	"encoding/json"
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

// ---------------------------------------------------------------------------
// P-C4: the branch quartet over a real resumed session
// ---------------------------------------------------------------------------

// writePlainExchanges writes a session whose entries are only messages —
// the clean shape the branch commands navigate, with no model/thinking
// state entries for the projection to route around.
func writePlainExchanges(t *testing.T, cwd string, pairs ...[2]string) string {
	t.Helper()
	m := session.CreateSession(cwd)
	for _, p := range pairs {
		now := time.Now().UnixMilli()
		for _, msg := range []ai.Message{
			{
				Role:        ai.RoleUser,
				Content:     []ai.ContentBlock{ai.TextBlock(p[0])},
				TimestampMS: now,
			},
			{
				Role:        ai.RoleAssistant,
				Content:     []ai.ContentBlock{ai.TextBlock(p[1])},
				TimestampMS: now,
			},
		} {
			if _, err := m.AppendMessage(msg); err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}
		}
	}
	return m.SessionFile()
}

// entryIDsOf lists a manager's entry ids in file order.
func entryIDsOf(mgr *session.SessionManager) []string {
	entries := mgr.Entries()
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// treeShape describes a chain u1→a1→u2→a2 from a get_tree forest, failing
// with the shape it found when the forest is anything else.
func treeShape(t *testing.T, forest []*session.TreeNode) (string, string) {
	t.Helper()
	if len(forest) != 1 {
		t.Fatalf("forest roots = %d, want 1", len(forest))
	}
	node := forest[0]
	ids := make([]string, 0, 4)
	for i := 0; i < 4 && node != nil; i++ {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(node.Entry, &probe); err != nil {
			t.Fatalf("node entry: %v", err)
		}
		ids = append(ids, probe.ID)
		if len(node.Children) > 1 {
			t.Fatalf("node %s has %d children, want a chain", probe.ID, len(node.Children))
		}
		if len(node.Children) == 1 {
			node = node.Children[0]
			continue
		}
		node = nil
	}
	if len(ids) != 4 {
		t.Fatalf("chain = %v, want 4 entries", ids)
	}
	return ids[0], ids[3]
}

func TestRPCHostBranchQuartetOverRealSession(t *testing.T) {
	host, env := resumeHost(t)
	path := writePlainExchanges(t, env.cwd,
		[2]string{"first question", "first answer"},
		[2]string{"second question", "second answer"})
	if err := host.ResumeSession(path); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	mgr := host.Session()
	ids := entryIDsOf(mgr)
	if len(ids) != 4 {
		t.Fatalf("entries = %d, want 4", len(ids))
	}
	u1, a1, _, a2 := ids[0], ids[1], ids[2], ids[3]

	// The selector lists both user messages, in file order.
	msgs, err := host.GetForkMessages()
	if err != nil {
		t.Fatalf("GetForkMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("fork messages = %d, want 2", len(msgs))
	}
	if msgs[0].Text != "first question" || msgs[0].EntryID != u1 {
		t.Errorf("first candidate = %+v, want the first user entry", msgs[0])
	}
	if msgs[1].Text != "second question" {
		t.Errorf("second candidate text = %q", msgs[1].Text)
	}

	// The tree is the chain, leaf on the last assistant message.
	tree, leaf, err := host.GetTree()
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	first, last := treeShape(t, tree)
	if first != u1 || last != a2 {
		t.Errorf("chain = %s…%s, want %s…%s", first, last, u1, a2)
	}
	if leaf != a2 {
		t.Errorf("leaf = %s, want %s", leaf, a2)
	}

	// Fork to the first user message: it is the root, so the leaf resets
	// and the editor gets the prompt back. D2's re-seed is observable
	// here — the agent's transcript becomes the fork point's projection,
	// which for a reset leaf is empty, not the abandoned history.
	leafID, editorText, err := host.Fork(u1)
	if err != nil {
		t.Fatalf("Fork(u1): %v", err)
	}
	if leafID != "" {
		t.Errorf("leaf = %q, want the reset position", leafID)
	}
	if editorText != "first question" {
		t.Errorf("editorText = %q, want the prompt", editorText)
	}
	if got := host.Agent().State().Messages; len(got) != 0 {
		t.Errorf("agent transcript = %d messages, want the empty fork-point projection", len(got))
	}

	// Fork to the first assistant message: the leaf lands on the target,
	// no editor text, and the agent is re-seeded with the prefix.
	leafID, editorText, err = host.Fork(a1)
	if err != nil {
		t.Fatalf("Fork(a1): %v", err)
	}
	if leafID != a1 {
		t.Errorf("leaf = %q, want %q", leafID, a1)
	}
	if editorText != "" {
		t.Errorf("editorText = %q, want empty for a non-user target", editorText)
	}
	got := host.Agent().State().Messages
	if len(got) != 2 ||
		ai.MessageText(got[0]) != "first question" ||
		ai.MessageText(got[1]) != "first answer" {
		t.Errorf("agent transcript = %q/%q, want the fork point's projection",
			ai.MessageText(got[0]), ai.MessageText(got[1]))
	}

	// An unknown id is pi's wire literal, and the leaf does not move.
	if _, _, err := host.Fork("deadbeef"); err == nil ||
		err.Error() != "Invalid entry ID for forking" {
		t.Errorf("Fork(unknown) err = %v, want pi's literal", err)
	}
	if host.Session().GetLeafID() != a1 {
		t.Error("a refused fork must leave the leaf alone")
	}

	// Clone extracts the current root→leaf path (u1→a1) into a new file;
	// the live session keeps its leaf, and the extraction is resumable —
	// the client's switch, this port's twin of pi's switch_session.
	clonePath, err := host.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if clonePath == path {
		t.Fatal("clone answered the source's path")
	}
	if host.Session().GetLeafID() != a1 {
		t.Error("clone must not move the live leaf")
	}
	clone, err := session.OpenSession(clonePath)
	if err != nil {
		t.Fatalf("OpenSession(clone): %v", err)
	}
	if got := entryIDsOf(clone); len(got) != 2 || got[0] != u1 || got[1] != a1 {
		t.Errorf("clone entries = %v, want [%s %s]", got, u1, a1)
	}
	if got := clone.Header().ParentSession; got != path {
		t.Errorf("clone parentSession = %q, want %q", got, path)
	}
	if err := host.ResumeSession(clonePath); err != nil {
		t.Fatalf("ResumeSession(clone): %v", err)
	}
	if got := host.Agent().State().Messages; len(got) < 2 ||
		ai.MessageText(got[len(got)-2]) != "first question" {
		t.Errorf("resumed clone replay = %d messages, want the extracted path", len(got))
	}
}

func TestRPCHostCloneFreshSessionExtractsMetadata(t *testing.T) {
	host, _ := resumeHost(t)
	// A fresh harness session has no conversation, but it does carry the
	// model/thinking metadata a resume restores from (newRPCHost records
	// it before the first message). A clone of it is a legitimate if
	// boring extraction: two state entries, no messages.
	path, err := host.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	clone, err := session.OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession(clone): %v", err)
	}
	entries := clone.Entries()
	if len(entries) != 2 {
		t.Fatalf("clone entries = %d, want 2 (model_change + thinking_level_change)", len(entries))
	}
	if entries[0].Type != "model_change" || entries[1].Type != "thinking_level_change" {
		t.Errorf("clone entry types = %s/%s", entries[0].Type, entries[1].Type)
	}
	if got := clone.Header().ParentSession; got != host.Session().SessionFile() {
		t.Errorf("clone parentSession = %q, want %q", got, host.Session().SessionFile())
	}
}
