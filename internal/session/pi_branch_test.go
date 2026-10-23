package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ultrac/internal/ai"
)

// The write side of the branch tree: Branch / ResetLeaf / BranchWithSummary
// (docs/compaction-branch-plan.md P-C3, E-C6). The read side (framing,
// fromId/details through Extra) is pinned by the pi fixture at the bottom.

// entryLine returns the session file's first entry of the given type as a
// parsed map. A branch_summary is not necessarily the last line: appends
// after a branch point are the normal case.
func entryLine(t *testing.T, path, typ string) map[string]json.RawMessage {
	t.Helper()
	for _, line := range sessionLines(t, path) {
		if string(line["type"]) == `"`+typ+`"` {
			return line
		}
	}
	t.Fatalf("no %s entry in %s", typ, path)
	return nil
}

func pathIDs(path []*SessionEntry) []string {
	out := make([]string, len(path))
	for i, e := range path {
		out[i] = e.ID
	}
	return out
}

func TestBranchMovesLeafWithoutRewritingHistory(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	a, err := m.AppendMessage(userMsg("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.AppendMessage(userMsg("b"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.AppendMessage(userMsg("c"))
	if err != nil {
		t.Fatal(err)
	}

	fileBefore, err := os.ReadFile(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Branch(b); err != nil {
		t.Fatal(err)
	}
	if m.GetLeafID() != b {
		t.Fatalf("leaf = %s, want the branch point %s", m.GetLeafID(), b)
	}

	d, err := m.AppendMessage(userMsg("d"))
	if err != nil {
		t.Fatal(err)
	}
	// The new entry parents at the branch point, and the path from the leaf
	// is the branch — not the abandoned tail (pi's branch, :1579-1593).
	entries := m.Entries()
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4 (append-only: nothing is deleted)", len(entries))
	}
	if entries[3].ParentID == nil || *entries[3].ParentID != b {
		t.Fatalf("d's parent = %v, want %s", entries[3].ParentID, b)
	}
	if got := pathIDs(m.GetBranch("")); strings.Join(got, ",") != strings.Join([]string{a, b, d}, ",") {
		t.Errorf("path = %v, want [%s %s %s]", got, a, b, d)
	}
	// History is untouched: the old lines are still on disk, byte for byte,
	// and the abandoned entry survives in the file (off-path).
	fileAfter, err := os.ReadFile(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(fileAfter), string(fileBefore)) {
		t.Error("branching rewrote existing file content")
	}
	if !strings.Contains(string(fileAfter), `"id":"`+c+`"`) {
		t.Errorf("abandoned entry %s disappeared from the file", c)
	}
	// The leaf advanced to the new entry (appendLocked semantics).
	if m.GetLeafID() != d {
		t.Errorf("leaf = %s, want the appended %s", m.GetLeafID(), d)
	}
}

func TestBranchUnknownEntryErrors(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendMessage(userMsg("a")); err != nil {
		t.Fatal(err)
	}
	// pi throws `Entry ${branchFromId} not found`; the leaf must not move.
	if err := m.Branch("deadbeef"); err == nil {
		t.Fatal("Branch(unknown) = nil error, want a refusal")
	}
	if m.GetLeafID() == "deadbeef" {
		t.Error("leaf moved to an unknown entry")
	}
	if _, err := m.BranchWithSummary(strPtr("deadbeef"), "s", nil, nil); err == nil {
		t.Fatal("BranchWithSummary(unknown) = nil error, want a refusal")
	}
}

func TestResetLeafMakesNextAppendRoot(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	a, err := m.AppendMessage(userMsg("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("b")); err != nil {
		t.Fatal(err)
	}
	m.ResetLeaf()
	if m.GetLeafID() != "" {
		t.Fatalf("leaf = %q, want the empty (before any entry) position", m.GetLeafID())
	}
	c, err := m.AppendMessage(userMsg("c"))
	if err != nil {
		t.Fatal(err)
	}
	// The append is a new root (parentId null) — pi's resetLeaf (:1591-1594).
	entries := m.Entries()
	if entries[2].ParentID != nil {
		t.Fatalf("c's parent = %v, want null", *entries[2].ParentID)
	}
	if got := pathIDs(m.GetBranch("")); strings.Join(got, ",") != c {
		t.Errorf("path = %v, want just [%s]", got, c)
	}
	_ = a
}

func TestBranchWithSummaryWritesPiEntryShape(t *testing.T) {
	// The keys this writer emits are exactly pi's BranchSummaryEntry
	// (session-manager.ts:106-114) minus fromHook, which stays undefined
	// until this version grows summary hooks. Key ORDER is not compared:
	// Go marshals maps in sorted order, and no reader depends on order.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	b, err := m.AppendMessage(userMsg("fork point"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.AppendMessage(userMsg("abandoned tail"))
	if err != nil {
		t.Fatal(err)
	}

	usage := ai.Usage{Input: 11, Output: 22, TotalTokens: 33}
	details := &BranchSummaryDetails{ReadFiles: []string{"go.mod"}, ModifiedFiles: []string{"main.go"}}
	bsID, err := m.BranchWithSummary(&b, "## Goal\nexplored", details, &usage)
	if err != nil {
		t.Fatal(err)
	}

	line := entryLine(t, m.SessionFile(), "branch_summary")
	want := map[string]bool{
		"type": true, "id": true, "parentId": true, "timestamp": true,
		"fromId": true, "summary": true, "details": true, "usage": true,
	}
	if len(line) != len(want) {
		t.Fatalf("entry has %d keys (%v), want %d", len(line), keysOf(line), len(want))
	}
	for key := range want {
		if _, present := line[key]; !present {
			t.Errorf("key %q missing (have %v)", key, keysOf(line))
		}
	}
	if string(line["id"]) != `"`+bsID+`"` {
		t.Errorf("id = %s, want %s", line["id"], bsID)
	}
	// fromId is the leaf as it was BEFORE the move — where the abandoned
	// branch was left from, not where it ends (pi: this.leafId ?? "root").
	if string(line["fromId"]) != `"`+c+`"` {
		t.Errorf("fromId = %s, want the old leaf %s", line["fromId"], c)
	}
	if string(line["parentId"]) != `"`+b+`"` {
		t.Errorf("parentId = %s, want the branch point %s", line["parentId"], b)
	}
	if string(line["summary"]) != `"## Goal\nexplored"` {
		t.Errorf("summary = %s", line["summary"])
	}
	if string(line["details"]) != `{"readFiles":["go.mod"],"modifiedFiles":["main.go"]}` {
		t.Errorf("details = %s", line["details"])
	}
	if string(line["usage"]) == "" || !strings.Contains(string(line["usage"]), `"totalTokens":33`) {
		t.Errorf("usage = %s", line["usage"])
	}
	if _, present := line["fromHook"]; present {
		t.Error("fromHook written; this version has no summary hooks")
	}

	// The summary is the new leaf; the next append continues from it, so the
	// checkpoint leads the new branch's context.
	if m.GetLeafID() != bsID {
		t.Errorf("leaf = %s, want the branch_summary entry", m.GetLeafID())
	}
	next, err := m.AppendMessage(userMsg("after branch"))
	if err != nil {
		t.Fatal(err)
	}
	entries := m.Entries()
	last := entries[len(entries)-1]
	if last.ParentID == nil || *last.ParentID != bsID {
		t.Fatalf("next entry's parent = %v, want the summary %s", last.ParentID, bsID)
	}
	if m.GetLeafID() != next {
		t.Errorf("leaf = %s, want %s", m.GetLeafID(), next)
	}
}

func TestBranchWithSummaryNilFromIDStartsNewRoot(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendMessage(userMsg("a")); err != nil {
		t.Fatal(err)
	}
	b, err := m.AppendMessage(userMsg("b"))
	if err != nil {
		t.Fatal(err)
	}
	// nil fromID is pi's branchFromId: null — the resetLeaf position.
	bsID, err := m.BranchWithSummary(nil, "s", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	line := entryLine(t, m.SessionFile(), "branch_summary")
	if string(line["parentId"]) != "null" {
		t.Errorf("parentId = %s, want null (a new root)", line["parentId"])
	}
	if string(line["fromId"]) != `"`+b+`"` {
		t.Errorf("fromId = %s, want the old leaf %s", line["fromId"], b)
	}
	path := m.GetBranch("")
	if len(path) != 1 || path[0].ID != bsID {
		t.Fatalf("path = %v, want just the summary %s", pathIDs(path), bsID)
	}
}

func TestBranchWithSummaryOptionalFieldsStayAbsent(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	b, err := m.AppendMessage(userMsg("b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.BranchWithSummary(&b, "s", nil, nil); err != nil {
		t.Fatal(err)
	}
	line := entryLine(t, m.SessionFile(), "branch_summary")
	for _, key := range []string{"fromId", "summary"} {
		if _, present := line[key]; !present {
			t.Errorf("%s absent, want present", key)
		}
	}
	// A nil details/usage must stay absent on disk rather than become a
	// JSON null (same rule as AppendCompaction, plan P-C1 note 2).
	for _, key := range []string{"details", "usage", "fromHook"} {
		if _, present := line[key]; present {
			t.Errorf("%s = %s, want absent (nil parameter)", key, line[key])
		}
	}
}

func TestBranchWithSummaryStaysInMemoryWithoutConversation(t *testing.T) {
	// The lazy rule covers branch writes too: a session that never held a
	// conversation keeps its branch_summary in memory, and the whole tree
	// (summary included) lands once the first user message flushes.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendModelChange("anthropic", "claude-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BranchWithSummary(nil, "s", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.SessionFile()); !os.IsNotExist(err) {
		t.Fatalf("file exists without a conversation message: %v", err)
	}
	if _, err := m.AppendMessage(userMsg("hi")); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSession(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	path := reopened.GetBranch("")
	if len(path) != 2 || path[0].Type != TypeBranchSummary || path[1].Type != TypeMessage {
		t.Fatalf("path = %v, want [branch_summary, message]", pathIDs(path))
	}
}

func TestBranchFileOrderInvariantAfterReopen(t *testing.T) {
	// The plan's reindex invariant: branch moves only the in-memory leaf,
	// the next append still lands at the end of the file, so reopening
	// (leaf = last line) yields the new entry and the branch path.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	a, err := m.AppendMessage(userMsg("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.AppendMessage(userMsg("b"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.AppendMessage(userMsg("c"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := m.AppendMessage(userMsg("d"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Branch(b); err != nil {
		t.Fatal(err)
	}
	e, err := m.AppendMessage(userMsg("e"))
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.GetLeafID() != e {
		t.Fatalf("reopened leaf = %s, want the appended %s", reopened.GetLeafID(), e)
	}
	if got := pathIDs(reopened.GetBranch("")); strings.Join(got, ",") != strings.Join([]string{a, b, e}, ",") {
		t.Errorf("reopened path = %v, want [%s %s %s]", got, a, b, e)
	}
	// The abandoned tail is still in the file — append-only means it.
	lines := sessionLines(t, m.SessionFile())
	if len(lines) != 6 {
		t.Fatalf("file lines = %d, want header + 5 entries", len(lines))
	}
	for _, id := range []string{a, b, c, d, e} {
		found := false
		for _, line := range lines {
			if string(line["id"]) == `"`+id+`"` {
				found = true
			}
		}
		if !found {
			t.Errorf("entry %s missing from the reopened file", id)
		}
	}
}

func TestBranchWithSummaryProjectionFramesSummary(t *testing.T) {
	// Read-side alignment (pi_context.go:167-172): the branch_summary enters
	// context as a user message with pi's framing, between the kept prefix
	// and whatever follows the branch point.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendMessage(userMsg("before branch")); err != nil {
		t.Fatal(err)
	}
	b, err := m.AppendMessage(userMsg("fork point"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("abandoned tail")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BranchWithSummary(&b, "the branch summary", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("after branch")); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	msgs := reopened.BuildSessionContext().Messages
	want := []string{
		"before branch",
		"fork point",
		branchSummaryPrefix + "the branch summary" + branchSummarySuffix,
		"after branch",
	}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %d (%v), want %d", len(msgs), messageTextsOf(msgs), len(want))
	}
	for i, w := range want {
		if got := ai.MessageText(msgs[i]); got != w {
			t.Errorf("messages[%d] = %q, want %q", i, got, w)
		}
	}
}

func messageTextsOf(msgs []ai.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = ai.MessageText(m)
	}
	return out
}

// piBranchSummaryFixture is the shape pi writes for branchWithSummary
// (session-manager.ts:1600-1627): fromId is the leaf the branch was left
// from, parentId is the branch point, and the reader folds the summary back
// into context with the framing.
const piBranchSummaryFixture = `{"type":"session","version":3,"id":"0197bbbb-0000-7000-8000-000000000003","timestamp":"2026-10-14T12:00:00.000Z","cwd":"/repo"}
{"type":"message","id":"00112233","parentId":null,"timestamp":"2026-10-14T12:00:00.100Z","message":{"role":"user","content":"before branch","timestamp":1}}
{"type":"message","id":"44556677","parentId":"00112233","timestamp":"2026-10-14T12:00:01.000Z","message":{"role":"user","content":"abandoned tail","timestamp":2}}
{"type":"branch_summary","id":"8899aabb","parentId":"00112233","timestamp":"2026-10-14T12:00:02.000Z","fromId":"44556677","summary":"what the branch did","details":{"readFiles":["go.mod"],"modifiedFiles":[]},"usage":{"input":5,"output":6,"totalTokens":11}}
{"type":"message","id":"ccddeeff","parentId":"8899aabb","timestamp":"2026-10-14T12:00:03.000Z","message":{"role":"user","content":"after branch","timestamp":3}}
`

func TestReadsPiBranchSummarySessionFile(t *testing.T) {
	setupAgentDir(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(piBranchSummaryFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	// fromId and details round-trip through Extra under pi's exact names —
	// the same keys BranchWithSummary writes, so the two implementations
	// read each other's files.
	entries := m.Entries()
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(entries))
	}
	bs := entries[2]
	if bs.Type != TypeBranchSummary {
		t.Fatalf("entries[2].Type = %s, want branch_summary", bs.Type)
	}
	if got := bs.ExtraString("fromId"); got != "44556677" {
		t.Errorf("fromId = %q, want 44556677", got)
	}
	if got := bs.ExtraString("summary"); got != "what the branch did" {
		t.Errorf("summary = %q", got)
	}
	var details BranchSummaryDetails
	if err := json.Unmarshal(bs.Extra["details"], &details); err != nil {
		t.Fatalf("details: %v", err)
	}
	if len(details.ReadFiles) != 1 || details.ReadFiles[0] != "go.mod" {
		t.Errorf("readFiles = %v, want [go.mod]", details.ReadFiles)
	}
	// The leaf is the last line, and the projection frames the summary.
	if m.GetLeafID() != "ccddeeff" {
		t.Errorf("leaf = %s, want ccddeeff", m.GetLeafID())
	}
	msgs := m.BuildSessionContext().Messages
	want := []string{
		"before branch",
		branchSummaryPrefix + "what the branch did" + branchSummarySuffix,
		"after branch",
	}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %d (%v), want %d", len(msgs), messageTextsOf(msgs), len(want))
	}
	for i, w := range want {
		if got := ai.MessageText(msgs[i]); got != w {
			t.Errorf("messages[%d] = %q, want %q", i, got, w)
		}
	}
}

func TestTreeEntriesSeesWholeTree(t *testing.T) {
	// The pointer view whole-tree walks need: after a branch, both children
	// of the fork point are visible — one path is not the tree.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	a, err := m.AppendMessage(userMsg("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("b")); err != nil {
		t.Fatal(err)
	}
	if err := m.Branch(a); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("c")); err != nil {
		t.Fatal(err)
	}
	tree := m.TreeEntries()
	if len(tree) != 3 {
		t.Fatalf("tree entries = %d, want 3", len(tree))
	}
	children := 0
	for _, e := range tree {
		if e.ParentID != nil && *e.ParentID == a {
			children++
		}
	}
	if children != 2 {
		t.Errorf("children of the fork point = %d, want 2 (b and c)", children)
	}
}
