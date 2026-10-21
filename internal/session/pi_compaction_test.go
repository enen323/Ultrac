package session

import (
	"encoding/json"
	"os"

	"strings"
	"testing"
	"time"

	"ultrac/internal/ai"
)

// The write side of compaction: AppendCompaction must produce the line pi
// would have produced (docs/compaction-branch-plan.md E-C1). The read side is
// already covered by piCompactionFixture / TestReadsPiCompactedSessionFile;
// these tests pin the writer against the same shapes.

// compactionJSON reads the session file and returns its lines as parsed maps.
func sessionLines(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not a JSON object: %v", err)
		}
		lines = append(lines, m)
	}
	return lines
}

// compactionLine returns the session file's compaction entry as a parsed
// map. It is not necessarily the last line: appends after a compaction are
// normal.
func compactionLine(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	for _, line := range sessionLines(t, path) {
		if string(line["type"]) == `"compaction"` {
			return line
		}
	}
	t.Fatalf("no compaction entry in %s", path)
	return nil
}

func TestAppendCompactionRoundTrip(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendMessage(userMsg("dropped by compaction")); err != nil {
		t.Fatal(err)
	}
	kept, err := m.AppendMessage(userMsg("kept prefix"))
	if err != nil {
		t.Fatal(err)
	}

	system := ai.Message{
		Role:     ai.RoleSystem,
		Content:  []ai.ContentBlock{ai.TextBlock("")},
		Sections: map[string]*string{"preamble": strPtr("prompt at compaction")},
	}
	usage := ai.Usage{Input: 12345, Output: 678, TotalTokens: 13023}
	details := &CompactionDetails{ReadFiles: []string{"go.mod"}, ModifiedFiles: []string{"main.go"}}

	compID, err := m.AppendCompaction("## Goal\nship it", kept, 182340, details, &system, &usage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("after compaction")); err != nil {
		t.Fatal(err)
	}

	// ---- the line on disk: pi's CompactionEntry key set, verbatim names.
	last := compactionLine(t, m.SessionFile())
	if string(last["type"]) != `"compaction"` {
		t.Fatalf("type = %s", last["type"])
	}
	if string(last["id"]) != `"`+compID+`"` {
		t.Errorf("id = %s, want %s", last["id"], compID)
	}
	if string(last["summary"]) != `"## Goal\nship it"` {
		t.Errorf("summary = %s", last["summary"])
	}
	if string(last["firstKeptEntryId"]) != `"`+kept+`"` {
		t.Errorf("firstKeptEntryId = %s, want %s", last["firstKeptEntryId"], kept)
	}
	if string(last["tokensBefore"]) != "182340" {
		t.Errorf("tokensBefore = %s", last["tokensBefore"])
	}
	if string(last["details"]) != `{"readFiles":["go.mod"],"modifiedFiles":["main.go"]}` {
		t.Errorf("details = %s", last["details"])
	}
	if string(last["usage"]) == "" || !strings.Contains(string(last["usage"]), `"totalTokens":13023`) {
		t.Errorf("usage = %s", last["usage"])
	}
	// fromHook is never written by this version (no compaction hooks); the
	// key must stay absent, exactly like pi's undefined.
	if _, present := last["fromHook"]; present {
		t.Error("fromHook written; this version has no compaction hooks")
	}

	// The systemMessage snapshot carries the compaction's own timestamp
	// (pi spreads it, session-manager.ts:1277), not the prompt's birth.
	var snapshot struct {
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.Unmarshal(last["systemMessage"], &snapshot); err != nil {
		t.Fatalf("systemMessage: %v", err)
	}
	var ts string
	if err := json.Unmarshal(last["timestamp"], &ts); err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", ts)
	if err != nil {
		t.Fatalf("timestamp %q: %v", ts, err)
	}
	if snapshot.Timestamp != parsed.UnixMilli() {
		t.Errorf("systemMessage timestamp = %d, want the entry's %d", snapshot.Timestamp, parsed.UnixMilli())
	}

	// ---- the projection on reload: the same four messages the pi fixture
	// produces — captured system, framed summary, kept prefix, tail.
	reopened, err := OpenSession(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	ctx := reopened.BuildSessionContext()
	if len(ctx.Messages) != 4 {
		t.Fatalf("messages = %d, want [system, summary, kept, after]", len(ctx.Messages))
	}
	if ctx.Messages[0].Role != ai.RoleSystem {
		t.Errorf("messages[0] = %s, want the captured system message", ctx.Messages[0].Role)
	}
	if v := ctx.Messages[0].Sections["preamble"]; v == nil || *v != "prompt at compaction" {
		t.Errorf("captured sections = %v", ctx.Messages[0].Sections)
	}
	if !strings.HasPrefix(ai.MessageText(ctx.Messages[1]), compactionSummaryPrefix) ||
		!strings.HasSuffix(ai.MessageText(ctx.Messages[1]), compactionSummarySuffix) {
		t.Errorf("summary framing wrong: %q", ai.MessageText(ctx.Messages[1]))
	}
	if !strings.Contains(ai.MessageText(ctx.Messages[1]), "## Goal\nship it") {
		t.Errorf("summary text missing: %q", ai.MessageText(ctx.Messages[1]))
	}
	if ai.MessageText(ctx.Messages[2]) != "kept prefix" {
		t.Errorf("messages[2] = %q", ai.MessageText(ctx.Messages[2]))
	}
	if ai.MessageText(ctx.Messages[3]) != "after compaction" {
		t.Errorf("messages[3] = %q", ai.MessageText(ctx.Messages[3]))
	}
}

func TestAppendCompactionOptionalFieldsStayAbsent(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendMessage(userMsg("hi")); err != nil {
		t.Fatal(err)
	}
	compID, err := m.AppendCompaction("s", "", 5, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := compactionLine(t, m.SessionFile())
	for _, key := range []string{"summary", "firstKeptEntryId", "tokensBefore"} {
		if _, present := last[key]; !present {
			t.Errorf("%s absent, want present", key)
		}
	}
	for _, key := range []string{"details", "usage", "systemMessage", "fromHook"} {
		if _, present := last[key]; present {
			t.Errorf("%s = %s, want absent (nil parameter)", key, last[key])
		}
	}
	// A null firstKeptEntryId substitutes the entry's own id (pi
	// session-manager.ts:1275): the whole prefix is summarized away and the
	// projection holds just the summary.
	if string(last["firstKeptEntryId"]) != `"`+compID+`"` {
		t.Errorf("firstKeptEntryId = %s, want the entry's own id %s", last["firstKeptEntryId"], compID)
	}
	reopened, err := OpenSession(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	msgs := reopened.BuildSessionContext().Messages
	if len(msgs) != 1 || msgs[0].Role != ai.RoleUser {
		t.Fatalf("messages = %d (%+v), want just the summary user message", len(msgs), msgs)
	}
}

func TestAppendCompactionMatchesPiEntryKeySet(t *testing.T) {
	// The keys this writer emits are exactly pi's CompactionEntry interface
	// (session-manager.ts:91-103) minus fromHook, which stays undefined
	// until this version grows compaction hooks. Key ORDER is not compared:
	// Go marshals maps in sorted order, and no reader depends on order.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendMessage(userMsg("hi")); err != nil {
		t.Fatal(err)
	}
	system := ai.Message{Role: ai.RoleSystem, Content: []ai.ContentBlock{ai.TextBlock("p")}}
	usage := ai.Usage{TotalTokens: 1}
	if _, err := m.AppendCompaction("s", "", 1, &CompactionDetails{}, &system, &usage); err != nil {
		t.Fatal(err)
	}
	last := compactionLine(t, m.SessionFile())
	want := map[string]bool{
		"type": true, "id": true, "parentId": true, "timestamp": true,
		"summary": true, "firstKeptEntryId": true, "tokensBefore": true,
		"details": true, "usage": true, "systemMessage": true,
	}
	if len(last) != len(want) {
		t.Fatalf("entry has %d keys (%v), want %d", len(last), keysOf(last), len(want))
	}
	for key := range want {
		if _, present := last[key]; !present {
			t.Errorf("key %q missing (have %v)", key, keysOf(last))
		}
	}
	// parentId chains to the previous entry, and the leaf advanced.
	entries := m.Entries()
	if len(entries) != 2 || entries[1].ParentID == nil || *entries[1].ParentID != entries[0].ID {
		t.Fatalf("parent chain broken: %+v", entries)
	}
	if m.GetLeafID() != entries[1].ID {
		t.Errorf("leaf = %s, want the compaction entry", m.GetLeafID())
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAppendCompactionAfterSetupEntriesOnly exercises the lazy rule: a
// compaction appended to a session that never held a conversation stays in
// memory (nothing to flush), matching how setup entries behave.
func TestAppendCompactionAfterSetupEntriesOnly(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendModelChange("anthropic", "claude-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendCompaction("s", "", 1, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.SessionFile()); !os.IsNotExist(err) {
		t.Fatalf("file exists without a conversation message: %v", err)
	}
	ctx := m.BuildSessionContext()
	if len(ctx.Messages) != 1 {
		t.Fatalf("messages = %d, want the in-memory summary", len(ctx.Messages))
	}
}
