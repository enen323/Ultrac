package session

// pi_tree_test.go — the read side of the branch tree (docs/compaction-
// branch-plan.md P-C4): BuildTree's forest shape, the fork selector's
// candidate list, PlanFork's decision table, and CreateBranchedSession's
// extraction. The write side (Branch / ResetLeaf) is pi_branch_test.go;
// the wire contract these serve is internal/rpc's.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ultrac/internal/ai"
)

// handEntry builds a bare entry for tree-shape tests: only the fields
// BuildTree reads (type, id, parentId, timestamp) matter, and a message
// body is what the wire carries, not what the walk follows.
func handEntry(id string, parent *string, stamp string) *SessionEntry {
	return &SessionEntry{Type: TypeMessage, ID: id, ParentID: parent, Timestamp: stamp}
}

// ---------------------------------------------------------------------------
// BuildTree
// ---------------------------------------------------------------------------

func TestBuildTreeLinearChain(t *testing.T) {
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

	roots, err := BuildTree(m.TreeEntries())
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("roots = %d, want 1", len(roots))
	}
	// The chain nests: root → b → c, and the leaf is the deepest node.
	root := roots[0]
	if id := entryIDOf(t, root); id != a {
		t.Fatalf("root = %s, want %s", id, a)
	}
	if len(root.Children) != 1 || entryIDOf(t, root.Children[0]) != b {
		t.Fatalf("root children = %v, want [%s]", childIDs(root), b)
	}
	mid := root.Children[0]
	if len(mid.Children) != 1 || entryIDOf(t, mid.Children[0]) != c {
		t.Fatalf("second-level children = %v, want [%s]", childIDs(mid), c)
	}
	if len(mid.Children[0].Children) != 0 {
		t.Error("the leaf must have no children")
	}
}

func TestBuildTreeBranchForestKeepsFileOrder(t *testing.T) {
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
	// Branch back to a and append c: a now has two children, and they can
	// share a millisecond, so the order must come from the file, not the
	// clock (pi sorts children by timestamp; stable sort keeps file order
	// for equal stamps, which is the only order that reproduces pi).
	if err := m.Branch(a); err != nil {
		t.Fatal(err)
	}
	c, err := m.AppendMessage(userMsg("c"))
	if err != nil {
		t.Fatal(err)
	}

	roots, err := BuildTree(m.TreeEntries())
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("roots = %d, want 1 (one fork point, one forest)", len(roots))
	}
	got := childIDs(roots[0])
	if strings.Join(got, ",") != strings.Join([]string{b, c}, ",") {
		t.Errorf("children = %v, want [%s %s] in file order", got, b, c)
	}
}

func TestBuildTreeOrphanAndSelfLoopAreRoots(t *testing.T) {
	// Hand-built: a broken parent link and a self-loop are pi's two
	// documented root cases (session-manager.ts:1543-1552) — the walk
	// must show the entries, never swallow or wedge on them.
	entries := []*SessionEntry{
		handEntry("root1", nil, "2026-01-01T00:00:00.000Z"),
		handEntry("child", strPtr("root1"), "2026-01-01T00:00:01.000Z"),
		handEntry("orphan", strPtr("missing"), "2026-01-01T00:00:02.000Z"),
		handEntry("selfloop", strPtr("selfloop"), "2026-01-01T00:00:03.000Z"),
	}
	roots, err := BuildTree(entries)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	var ids []string
	for _, r := range roots {
		ids = append(ids, entryIDOf(t, r))
	}
	if strings.Join(ids, ",") != "root1,orphan,selfloop" {
		t.Errorf("roots = %v, want [root1 orphan selfloop]", ids)
	}
}

func TestBuildTreeDeepChainDoesNotOverflow(t *testing.T) {
	// The walk is iterative on purpose (pi's own note): a deep chain must
	// not overflow the goroutine stack. 100k entries is far past the
	// default stack growth budget for a recursive walk of this shape.
	const deep = 100000
	entries := make([]*SessionEntry, deep)
	for i := 0; i < deep; i++ {
		e := handEntry(fmt.Sprintf("e%06d", i), nil, fmt.Sprintf("2026-01-01T00:%02d:%02d.%03dZ", i/60000%60, i/1000%60, i%1000))
		if i > 0 {
			p := entries[i-1].ID
			e.ParentID = &p
		}
		entries[i] = e
	}
	roots, err := BuildTree(entries)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("roots = %d, want 1", len(roots))
	}
}

// ---------------------------------------------------------------------------
// UserForkMessages
// ---------------------------------------------------------------------------

func TestUserForkMessagesSkipsNonTextfulUsers(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	for _, msg := range []ai.Message{
		userMsg("first"),
		assistantMsg("answer"),
		userMsg("second"),
		ai.NewTextMessage(ai.RoleUser, ""), // image-only in spirit: no text
		userMsg("third"),
	} {
		if _, err := m.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	got := UserForkMessages(m.TreeEntries())
	if len(got) != 3 {
		t.Fatalf("fork messages = %d, want 3 (assistant and textless user skipped)", len(got))
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if got[i].Text != w {
			t.Errorf("message %d text = %q, want %q", i, got[i].Text, w)
		}
		if got[i].EntryID == "" {
			t.Errorf("message %d has no entryId", i)
		}
	}
}

// ---------------------------------------------------------------------------
// PlanFork
// ---------------------------------------------------------------------------

func TestPlanForkDecisionTable(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	u1, err := m.AppendMessage(userMsg("one"))
	if err != nil {
		t.Fatal(err)
	}
	a1, err := m.AppendMessage(assistantMsg("a1"))
	if err != nil {
		t.Fatal(err)
	}
	u2, err := m.AppendMessage(userMsg("two"))
	if err != nil {
		t.Fatal(err)
	}
	entries := m.TreeEntries()

	cases := []struct {
		name       string
		target     string
		leaf       string
		wantLeaf   string
		wantText   string
		wantNoOp   bool
		wantErrLit string
	}{
		{
			name:     "user target with a parent moves the leaf to the parent",
			target:   u2,
			leaf:     a1,
			wantLeaf: a1,
			wantText: "two",
		},
		{
			name:     "root user target resets the leaf",
			target:   u1,
			leaf:     u2,
			wantLeaf: "",
			wantText: "one",
		},
		{
			name:     "non-user target moves the leaf to the target",
			target:   a1,
			leaf:     u2,
			wantLeaf: a1,
		},
		{
			name:     "the current leaf is a no-op",
			target:   u2,
			leaf:     u2,
			wantLeaf: u2,
			wantNoOp: true,
		},
		{
			name:       "unknown entry is pi's wire literal",
			target:     "deadbeef",
			leaf:       u2,
			wantErrLit: "Invalid entry ID for forking",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			move, err := PlanFork(entries, c.target, c.leaf)
			if c.wantErrLit != "" {
				if err == nil || err.Error() != c.wantErrLit {
					t.Fatalf("err = %v, want %q", err, c.wantErrLit)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanFork: %v", err)
			}
			if move.NoOp != c.wantNoOp {
				t.Errorf("NoOp = %v, want %v", move.NoOp, c.wantNoOp)
			}
			if move.LeafID != c.wantLeaf {
				t.Errorf("leaf = %q, want %q", move.LeafID, c.wantLeaf)
			}
			if move.EditorText != c.wantText {
				t.Errorf("editorText = %q, want %q", move.EditorText, c.wantText)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CreateBranchedSession
// ---------------------------------------------------------------------------

func TestCreateBranchedSessionExtractsRootToLeafPath(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	u1, err := m.AppendMessage(userMsg("kept one"))
	if err != nil {
		t.Fatal(err)
	}
	a1, err := m.AppendMessage(assistantMsg("kept answer"))
	if err != nil {
		t.Fatal(err)
	}
	u2, err := m.AppendMessage(userMsg("kept two"))
	if err != nil {
		t.Fatal(err)
	}
	// An abandoned sibling off u1: the extraction must take the root→leaf
	// path only, leaving the branch behind in the source file.
	if err := m.Branch(u1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("abandoned tail")); err != nil {
		t.Fatal(err)
	}
	if err := m.Branch(u2); err != nil {
		t.Fatal(err)
	}

	src := m.SessionFile()
	srcBefore, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	path, err := m.CreateBranchedSession(u2)
	if err != nil {
		t.Fatalf("CreateBranchedSession: %v", err)
	}
	if path == src {
		t.Fatal("the extraction must be a new file, not the source")
	}
	if filepath.Dir(path) != filepath.Dir(src) {
		t.Errorf("clone dir = %s, want the source's %s", filepath.Dir(path), filepath.Dir(src))
	}

	// The source is untouched: the extraction reads, never mutates.
	srcAfter, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(srcBefore) != string(srcAfter) {
		t.Error("CreateBranchedSession rewrote the source file")
	}

	// The clone is the path, verbatim ids, header linked to the source
	// (pi's parentSession), same cwd.
	clone, err := OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession(clone): %v", err)
	}
	if got := clone.Header().ParentSession; got != src {
		t.Errorf("parentSession = %q, want %q", got, src)
	}
	if clone.CWD() != m.CWD() {
		t.Errorf("cwd = %q, want %q", clone.CWD(), m.CWD())
	}
	entries := clone.Entries()
	gotIDs := make([]string, len(entries))
	for i, e := range entries {
		gotIDs[i] = e.ID
	}
	if strings.Join(gotIDs, ",") != strings.Join([]string{u1, a1, u2}, ",") {
		t.Errorf("clone entries = %v, want [%s %s %s]", gotIDs, u1, a1, u2)
	}
	if clone.GetLeafID() != u2 {
		t.Errorf("clone leaf = %s, want %s", clone.GetLeafID(), u2)
	}

	// Reopening projects the same branch the source did at the fork point.
	if !sameMessages(clone.BuildSessionContext().Messages, m.BuildSessionContext().Messages) {
		t.Error("the clone's projection differs from the source's at the same leaf")
	}
}

func TestCreateBranchedSessionKeepsCompactionFirstKeptEntryId(t *testing.T) {
	// A compaction entry's firstKeptEntryId names an entry on the path.
	// Ids are preserved verbatim, so the reference stays valid without a
	// remap — the case the plan calls out for label-style re-linking.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	u1, err := m.AppendMessage(userMsg("dropped by compaction"))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := m.AppendMessage(userMsg("kept prefix"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendCompaction("summary", kept, 1000, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	u3, err := m.AppendMessage(userMsg("after compaction"))
	if err != nil {
		t.Fatal(err)
	}

	path, err := m.CreateBranchedSession(u3)
	if err != nil {
		t.Fatalf("CreateBranchedSession: %v", err)
	}
	clone, err := OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession(clone): %v", err)
	}
	var comp SessionEntry
	found := false
	for _, e := range clone.Entries() {
		if e.Type == TypeCompaction {
			comp = e
			found = true
		}
	}
	if !found {
		t.Fatal("the compaction entry did not survive the extraction")
	}
	if got := comp.ExtraString("firstKeptEntryId"); got != kept {
		t.Errorf("firstKeptEntryId = %q, want %q (ids are preserved, not remapped)", got, kept)
	}
	// The whole path rode along: the dropped entry, the kept prefix, the
	// compaction itself, and the tail.
	var types []string
	for _, e := range clone.Entries() {
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "message,message,compaction,message" {
		t.Errorf("clone entry types = %v, want the full path", types)
	}
	if clone.Entries()[0].ID != u1 {
		t.Errorf("first clone entry = %s, want the dropped-by-compaction %s", clone.Entries()[0].ID, u1)
	}
}

func TestCreateBranchedSessionKeepsLabelEntry(t *testing.T) {
	// Labels are read-only in this version (nothing writes them), but pi's
	// files carry them. The extraction copies the path verbatim — a label
	// on the path stays on the path with its parent intact, because the
	// path is a chain and needs no re-linking.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	u1, err := m.AppendMessage(userMsg("one"))
	if err != nil {
		t.Fatal(err)
	}
	a1, err := m.AppendMessage(assistantMsg("a1"))
	if err != nil {
		t.Fatal(err)
	}
	u2, err := m.AppendMessage(userMsg("two"))
	if err != nil {
		t.Fatal(err)
	}

	// Splice a label entry into the file the way pi would have written it:
	// the label sits between a1 and u2 on the chain (u2 re-parents onto
	// it), then reopen — the reader keeps unknown-shaped entries whole.
	data, err := os.ReadFile(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	parsed := make([]map[string]json.RawMessage, len(lines))
	for i, l := range lines {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal([]byte(l), &entry); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", i, err)
		}
		parsed[i] = entry
	}
	label := map[string]any{
		"type": TypeLabel, "id": "lbl00001", "parentId": a1,
		"timestamp": NowISO(), "label": "milestone one",
	}
	labelLine, err := json.Marshal(label)
	if err != nil {
		t.Fatal(err)
	}
	spliced := make([]string, 0, len(lines)+1)
	for i, entry := range parsed {
		if string(entry["id"]) == `"`+u2+`"` {
			// Re-parent u2 onto the label so the label is on the path.
			entry["parentId"] = json.RawMessage(`"lbl00001"`)
			rewritten, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			spliced = append(spliced, string(labelLine), string(rewritten))
			continue
		}
		spliced = append(spliced, lines[i])
	}
	if err := os.WriteFile(m.SessionFile(), []byte(strings.Join(spliced, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := OpenSession(m.SessionFile())
	if err != nil {
		t.Fatalf("OpenSession(spliced): %v", err)
	}

	path, err := m2.CreateBranchedSession(u2)
	if err != nil {
		t.Fatalf("CreateBranchedSession: %v", err)
	}
	clone, err := OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession(clone): %v", err)
	}
	var found bool
	var ids []string
	for _, e := range clone.Entries() {
		ids = append(ids, e.ID)
		if e.Type == TypeLabel {
			found = true
			if e.ID != "lbl00001" {
				t.Errorf("label id = %s, want lbl00001", e.ID)
			}
			if e.ParentID == nil || *e.ParentID != a1 {
				t.Errorf("label parent = %v, want %s (the path is a chain)", e.ParentID, a1)
			}
			if got := e.ExtraString("label"); got != "milestone one" {
				t.Errorf("label body = %q, want milestone one", got)
			}
		}
	}
	if !found {
		t.Error("the label entry disappeared from the extraction")
	}
	// The label sits on the path, between its parent and the leaf.
	if strings.Join(ids, ",") != strings.Join([]string{u1, a1, "lbl00001", u2}, ",") {
		t.Errorf("clone entries = %v, want [%s %s lbl00001 %s]", ids, u1, a1, u2)
	}
}

func TestCreateBranchedSessionErrors(t *testing.T) {
	// In-memory: no file behind the session, so there is nothing to write
	// an extraction into.
	mem := InMemorySession(`D:\repo`)
	if _, err := mem.AppendMessage(userMsg("only in ram")); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.CreateBranchedSession(mem.GetLeafID()); err == nil ||
		err.Error() != "session: cannot branch a session with no file behind it" {
		t.Errorf("in-memory err = %v, want the no-file refusal", err)
	}

	// A leaf the session does not contain.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendMessage(userMsg("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateBranchedSession("deadbeef"); err == nil ||
		err.Error() != "session: entry deadbeef not found" {
		t.Errorf("unknown-leaf err = %v, want the not-found refusal", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func entryIDOf(t *testing.T, n *TreeNode) string {
	t.Helper()
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(n.Entry, &probe); err != nil {
		t.Fatalf("node entry is not an entry line: %v", err)
	}
	return probe.ID
}

func childIDs(n *TreeNode) []string {
	out := make([]string, len(n.Children))
	for i, c := range n.Children {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(c.Entry, &probe); err != nil {
			return []string{fmt.Sprintf("<bad: %v>", err)}
		}
		out[i] = probe.ID
	}
	return out
}

// sameMessages compares two projections by their wire form: the transcript
// is what the next prompt would send, so equality there is the contract.
func sameMessages(a, b []ai.Message) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ja) == string(jb)
}
