package compact

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// Branch summarization tests (plan P-C3): the common-ancestor collection,
// the newest-first budget walk, and the one-shot summary request. The write
// side (BranchWithSummary) is covered in the session package; the end-to-end
// test at the bottom wires the two together the way pi's navigateTree does.

func appendUsers(t *testing.T, m *session.SessionManager, texts ...string) []string {
	t.Helper()
	ids := make([]string, len(texts))
	for i, text := range texts {
		id, err := m.AppendMessage(ai.NewTextMessage(ai.RoleUser, text))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

// branchedTree builds a→b→c (old leaf c), then branches at b and appends d:
// the tree holds two children of b, the sibling a navigation targets.
// Returns the manager, the four entry ids [a b c d] and the whole tree.
func branchedTree(t *testing.T) (*session.SessionManager, [4]string, []*session.SessionEntry) {
	t.Helper()
	m := session.InMemorySession(`D:\repo`)
	abc := appendUsers(t, m, "a", "b", "c")
	if err := m.Branch(abc[1]); err != nil {
		t.Fatal(err)
	}
	d := appendUsers(t, m, "d")
	var ids [4]string
	copy(ids[:], abc)
	ids[3] = d[0]
	return m, ids, m.TreeEntries()
}

func entryTexts(entries []*session.SessionEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if msg, ok := session.EntryMessage(e); ok {
			out = append(out, ai.MessageText(msg))
		}
	}
	return out
}

func TestCollectEntriesForBranchSummarySiblingBranch(t *testing.T) {
	_, ids, entries := branchedTree(t)
	// Navigating from the old leaf c to the sibling d: the common ancestor
	// is b, and the abandoned path is c alone.
	res := CollectEntriesForBranchSummary(entries, ids[2], ids[3])
	if res.CommonAncestorID != ids[1] {
		t.Errorf("commonAncestor = %s, want %s (b)", res.CommonAncestorID, ids[1])
	}
	if got := entryTexts(res.Entries); strings.Join(got, ",") != "c" {
		t.Errorf("entries = %v, want [c]", got)
	}
}

func TestCollectEntriesForBranchSummaryDeepAncestor(t *testing.T) {
	// a→b→c→e (old leaf e), branching at b for d: the ancestor is b and
	// the whole c→e stretch is abandoned, in chronological order.
	m := session.InMemorySession(`D:\repo`)
	abce := appendUsers(t, m, "a", "b", "c", "e")
	if err := m.Branch(abce[1]); err != nil {
		t.Fatal(err)
	}
	d := appendUsers(t, m, "d")[0]

	res := CollectEntriesForBranchSummary(m.TreeEntries(), abce[3], d)
	if res.CommonAncestorID != abce[1] {
		t.Errorf("commonAncestor = %s, want %s (b)", res.CommonAncestorID, abce[1])
	}
	if got := entryTexts(res.Entries); strings.Join(got, ",") != "c,e" {
		t.Errorf("entries = %v, want [c e]", got)
	}
}

func TestCollectEntriesForBranchSummaryNoOldLeaf(t *testing.T) {
	_, ids, entries := branchedTree(t)
	// No old position (pi's null leaf): nothing was explored.
	res := CollectEntriesForBranchSummary(entries, "", ids[3])
	if len(res.Entries) != 0 || res.CommonAncestorID != "" {
		t.Errorf("result = %+v, want empty", res)
	}
}

func TestCollectEntriesForBranchSummarySamePosition(t *testing.T) {
	_, ids, entries := branchedTree(t)
	// Navigating to where the leaf already is: the ancestor is the entry
	// itself and nothing is abandoned (pi's no-op navigate).
	res := CollectEntriesForBranchSummary(entries, ids[2], ids[2])
	if res.CommonAncestorID != ids[2] {
		t.Errorf("commonAncestor = %s, want %s (the position itself)", res.CommonAncestorID, ids[2])
	}
	if len(res.Entries) != 0 {
		t.Errorf("entries = %v, want none", entryTexts(res.Entries))
	}
}

func TestCollectEntriesForBranchSummaryUnknownTarget(t *testing.T) {
	_, ids, entries := branchedTree(t)
	// A target that is not in the tree shares no ancestor with the old
	// path, so the walk runs all the way to the root — pi's behavior
	// against a null commonAncestorId.
	res := CollectEntriesForBranchSummary(entries, ids[2], "deadbeef")
	if res.CommonAncestorID != "" {
		t.Errorf("commonAncestor = %q, want empty", res.CommonAncestorID)
	}
	if got := entryTexts(res.Entries); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("entries = %v, want the whole old path [a b c]", got)
	}
}

func TestPrepareBranchEntriesKeepsNewestWithinBudget(t *testing.T) {
	// Five 100-token messages (350 chars at 3.5 chars/token); a 250-token
	// budget keeps the two newest — the walk stops at the first message
	// that does not fit, exactly like pi.
	m := session.InMemorySession(`D:\repo`)
	for i := 0; i < 5; i++ {
		if _, err := m.AppendMessage(ai.NewTextMessage(ai.RoleUser, strings.Repeat("x", 350))); err != nil {
			t.Fatal(err)
		}
	}
	messages, _, total := prepareBranchEntries(m.TreeEntries(), 250)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the 2 newest", len(messages))
	}
	if total != 200 {
		t.Errorf("totalTokens = %d, want 200", total)
	}
	// Chronological order is preserved despite the newest-first walk.
	for _, msg := range messages {
		if len(ai.MessageText(msg)) != 350 {
			t.Errorf("unexpected message %q in the kept set", ai.MessageText(msg))
		}
	}
}

func TestPrepareBranchEntriesSummaryGrace(t *testing.T) {
	// The tree is seed → branch_summary → u1 → u2 (two 100-token users).
	// The checkpoint is older than both users, so the budget walk reaches
	// it only after the users are in: whether it rides along depends on
	// pi's grace rule (a summary fits past the line while the budget is
	// less than 90% spent).
	m := session.InMemorySession(`D:\repo`)
	seed := appendUsers(t, m, "seed")[0]
	if _, err := m.BranchWithSummary(&seed, "checkpoint text",
		&session.BranchSummaryDetails{ReadFiles: []string{"a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	appendUsers(t, m, strings.Repeat("u", 350), strings.Repeat("v", 350))
	entries := m.TreeEntries()

	var summaryTokens int
	for _, e := range entries {
		if e.Type == session.TypeBranchSummary {
			for _, msg := range branchSummaryMessages(e) {
				summaryTokens += ai.EstimateMessageTokens(msg)
			}
		}
	}
	users := 200 // the two 100-token messages

	// A budget the checkpoint cannot fit and the grace declines (the
	// budget is already too spent): only the users survive.
	tight := users + summaryTokens/2
	messages, fileOps, _ := prepareBranchEntries(entries, tight)
	if len(messages) != 2 {
		t.Errorf("budget %d: messages = %d (%v), want the 2 users only", tight, len(messages), messageTexts(messages))
	}
	// The first pass harvests the checkpoint's file inventory even though
	// the budget walk dropped the checkpoint itself.
	if !fileOps.Read["a.go"] {
		t.Errorf("fileOps.read = %v, want a.go from the dropped checkpoint's details", fileOps.Read)
	}

	// A budget that is spent past the grace fraction: the checkpoint is
	// included anyway, ahead of the users (chronological order).
	loose := users + summaryTokens - 4
	messages, _, _ = prepareBranchEntries(entries, loose)
	texts := messageTexts(messages)
	if len(texts) != 3 || !strings.Contains(texts[0], "checkpoint text") {
		t.Errorf("budget %d: messages = %v, want [checkpoint, u, v]", loose, texts)
	}
}

func TestPrepareBranchEntriesSkipsCompactionSystemSnapshot(t *testing.T) {
	// A compaction on the abandoned path contributes its checkpoint (the
	// framed user message) but not its captured system prompt: pi's
	// getMessageFromEntry yields the checkpoint alone.
	m := session.InMemorySession(`D:\repo`)
	seed := appendUsers(t, m, "seed")[0]
	system := ai.Message{Role: ai.RoleSystem, Content: []ai.ContentBlock{ai.TextBlock("prompt at the boundary")}}
	if _, err := m.AppendCompaction("## Goal\nearlier work", seed, 100, nil, &system, nil); err != nil {
		t.Fatal(err)
	}
	appendUsers(t, m, "later")

	messages, _, _ := prepareBranchEntries(m.TreeEntries(), 0) // 0 = no budget
	if len(messages) != 3 {
		t.Fatalf("messages = %d (%v), want [seed, checkpoint, later]", len(messages), messageTexts(messages))
	}
	for _, msg := range messages {
		if msg.Role == ai.RoleSystem {
			t.Errorf("system snapshot leaked into the branch summary: %q", ai.MessageText(msg))
		}
	}
	if !strings.Contains(ai.MessageText(messages[1]), "earlier work") {
		t.Errorf("checkpoint missing from the summary messages: %q", ai.MessageText(messages[1]))
	}
}

func TestPrepareBranchEntriesToleratesMalformedDetails(t *testing.T) {
	// A branch_summary whose details are not an object (hand-edited file,
	// future format drift) contributes its message but no file inventory —
	// pi reaches the same place through Array.isArray checks.
	entries := []*session.SessionEntry{
		{
			Type: session.TypeBranchSummary,
			ID:   "bs1",
			Extra: map[string]json.RawMessage{
				"summary": json.RawMessage(`"checkpoint"`),
				"details": json.RawMessage(`"not an object"`),
			},
		},
	}
	messages, fileOps, _ := prepareBranchEntries(entries, 0)
	if len(messages) != 1 || !strings.Contains(ai.MessageText(messages[0]), "checkpoint") {
		t.Errorf("messages = %v, want the checkpoint", messageTexts(messages))
	}
	if len(fileOps.Read) != 0 || len(fileOps.Edited) != 0 {
		t.Errorf("fileOps = %v/%v, want nothing harvested", fileOps.Read, fileOps.Edited)
	}
}

func TestSummarizeBranchRequestShape(t *testing.T) {
	m := session.InMemorySession(`D:\repo`)
	appendUsers(t, m, "explored one", "explored two")
	entries := m.TreeEntries()

	fake := &capturingStreamFn{events: doneEvents("## Goal\nbranch work", ai.Usage{Input: 7, Output: 8, TotalTokens: 15})}
	got, err := SummarizeBranch(context.Background(), entries, BranchSummaryOptions{
		SummaryOptions: testOptions(fake.fn),
		ReserveTokens:  16384,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The preamble frames the checkpoint (pi: BRANCH_SUMMARY_PREAMBLE).
	if want := branchSummaryPreamble + "## Goal\nbranch work"; got.Summary != want {
		t.Errorf("summary = %q, want %q", got.Summary, want)
	}
	if got.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", got.Usage)
	}
	// The request shape: the shared summarization system prompt, the
	// serialized conversation inside <conversation> tags, the branch
	// prompt last.
	if fake.gotSystem != SummarizationSystemPrompt {
		t.Errorf("system prompt drifted from the verbatim constant")
	}
	if !strings.HasPrefix(fake.gotPrompt, "<conversation>\n[User]: explored one\n\n[User]: explored two\n</conversation>\n\n") {
		t.Errorf("prompt = %q", fake.gotPrompt)
	}
	if !strings.HasSuffix(fake.gotPrompt, BranchSummaryPrompt) {
		t.Errorf("prompt does not end with BRANCH_SUMMARY_PROMPT: %q", fake.gotPrompt)
	}
	// The flat 4096 cap, not compaction's reserve-scaled budget.
	if fake.gotOpts.MaxTokens != 4096 {
		t.Errorf("maxTokens = %d, want 4096", fake.gotOpts.MaxTokens)
	}
	// No cache write for a one-off summary.
	if fake.gotOpts.CacheRetention != ai.CacheRetentionNone {
		t.Errorf("cacheRetention = %q, want none", fake.gotOpts.CacheRetention)
	}
	// The branch path carries no thinking level: pi's generateBranchSummary
	// destructures none, even though its caller computes one.
	if fake.gotOpts.Reasoning != "" {
		t.Errorf("reasoning = %q, want empty", fake.gotOpts.Reasoning)
	}
}

func TestSummarizeBranchMaxTokensFollowsModelCap(t *testing.T) {
	m := session.InMemorySession(`D:\repo`)
	appendUsers(t, m, "explored")
	fake := &capturingStreamFn{events: doneEvents("s", ai.Usage{})}
	opts := testOptions(fake.fn)
	opts.Model.MaxTokens = 1000
	if _, err := SummarizeBranch(context.Background(), m.TreeEntries(), BranchSummaryOptions{SummaryOptions: opts}); err != nil {
		t.Fatal(err)
	}
	if fake.gotOpts.MaxTokens != 1000 {
		t.Errorf("maxTokens = %d, want the model cap 1000", fake.gotOpts.MaxTokens)
	}
}

func TestSummarizeBranchInstructions(t *testing.T) {
	m := session.InMemorySession(`D:\repo`)
	appendUsers(t, m, "explored")
	entries := m.TreeEntries()

	// Appended focus.
	appended := &capturingStreamFn{events: doneEvents("s", ai.Usage{})}
	if _, err := SummarizeBranch(context.Background(), entries, BranchSummaryOptions{
		SummaryOptions:     testOptions(appended.fn),
		CustomInstructions: "focus on the API",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(appended.gotPrompt, BranchSummaryPrompt+"\n\nAdditional focus: focus on the API") {
		t.Errorf("appended prompt = %q", appended.gotPrompt)
	}

	// Replacing focus.
	replaced := &capturingStreamFn{events: doneEvents("s", ai.Usage{})}
	if _, err := SummarizeBranch(context.Background(), entries, BranchSummaryOptions{
		SummaryOptions:      testOptions(replaced.fn),
		CustomInstructions:  "only this",
		ReplaceInstructions: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(replaced.gotPrompt, "only this") {
		t.Errorf("replaced prompt = %q", replaced.gotPrompt)
	}
	if strings.Contains(replaced.gotPrompt, "structured summary of this conversation branch") {
		t.Error("replaced prompt still contains the default instructions")
	}
}

func TestSummarizeBranchNoContent(t *testing.T) {
	// Entries that contribute no messages (setup state only): pi returns
	// "No content to summarize" without calling the model at all.
	m := session.InMemorySession(`D:\repo`)
	if _, err := m.AppendModelChange("anthropic", "claude-test"); err != nil {
		t.Fatal(err)
	}
	var called bool
	fn := ai.StreamFn(func(context.Context, ai.Model, ai.TranscriptContext, ai.StreamOptions) ai.Stream {
		called = true
		return ai.StreamOf()
	})
	opts := testOptions(fn)
	got, err := SummarizeBranch(context.Background(), m.TreeEntries(), BranchSummaryOptions{SummaryOptions: opts})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != noBranchContent {
		t.Errorf("summary = %q, want %q", got.Summary, noBranchContent)
	}
	if called {
		t.Error("summarize was called for an empty branch")
	}
}

func TestSummarizeBranchFailures(t *testing.T) {
	m := session.InMemorySession(`D:\repo`)
	appendUsers(t, m, "explored")
	entries := m.TreeEntries()

	errorMessage := ai.NewTextMessage(ai.RoleAssistant, "")
	errorMessage.StopReason = ai.StopError
	errorMessage.ErrorMessage = "provider exploded"

	lengthMessage := ai.NewTextMessage(ai.RoleAssistant, "partial")
	lengthMessage.StopReason = ai.StopLength

	abortedMessage := ai.NewTextMessage(ai.RoleAssistant, "")
	abortedMessage.StopReason = ai.StopAborted

	toolCallMessage := ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.ContentBlock{ai.ToolCallBlock("c1", "read", map[string]any{"path": "x"})},
		StopReason: ai.StopStop,
	}

	cases := []struct {
		name string
		msg  ai.Message
		want string
	}{
		{"error", errorMessage, "Branch summarization failed: provider exploded"},
		{"length", lengthMessage, "Branch summarization failed: generation hit the token cap and the summary is incomplete"},
		{"aborted", abortedMessage, "Branch summarization aborted"},
		{"tool call", toolCallMessage, "Branch summarization attempted to call a tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason := ai.StopError
			if c.msg.StopReason == ai.StopLength || c.msg.StopReason == ai.StopStop {
				reason = ai.StopStop
			}
			events := []ai.AssistantEvent{ai.EvDone{Reason: reason, Message: c.msg}}
			if c.msg.StopReason == ai.StopError || c.msg.StopReason == ai.StopAborted {
				events = []ai.AssistantEvent{ai.EvError{Reason: c.msg.StopReason, Error: c.msg}}
			}
			opts := testOptions((&capturingStreamFn{events: events}).fn)
			_, err := SummarizeBranch(context.Background(), entries, BranchSummaryOptions{SummaryOptions: opts})
			if err == nil {
				t.Fatalf("no error for %s", c.name)
			}
			if err.Error() != c.want {
				t.Errorf("error = %q, want %q", err.Error(), c.want)
			}
		})
	}
}

func TestSummarizeBranchAppendsFileOperations(t *testing.T) {
	// The checkpoint carries the file inventory of everything it
	// summarizes, as the XML sections pi appends (utils.ts:77-87).
	m := session.InMemorySession(`D:\repo`)
	if _, err := m.AppendMessage(bigAssistant(t, "working",
		readCall("go.mod"),
		ai.ToolCallBlock("c2", "edit", map[string]any{"path": "main.go"}),
	)); err != nil {
		t.Fatal(err)
	}
	fake := &capturingStreamFn{events: doneEvents("## Goal\nx", ai.Usage{})}
	got, err := SummarizeBranch(context.Background(), m.TreeEntries(), BranchSummaryOptions{
		SummaryOptions: testOptions(fake.fn),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := "<read-files>\ngo.mod\n</read-files>\n\n<modified-files>\nmain.go\n</modified-files>"
	if !strings.HasSuffix(got.Summary, wantSuffix) {
		t.Errorf("summary = %q, want the file-operation sections appended", got.Summary)
	}
	if len(got.ReadFiles) != 1 || got.ReadFiles[0] != "go.mod" {
		t.Errorf("readFiles = %v, want [go.mod]", got.ReadFiles)
	}
	if len(got.ModifiedFiles) != 1 || got.ModifiedFiles[0] != "main.go" {
		t.Errorf("modifiedFiles = %v, want [main.go]", got.ModifiedFiles)
	}
}

func TestBranchSummaryEndToEnd(t *testing.T) {
	// pi's navigateTree({summarize: true}) minus the navigation decision:
	// collect the abandoned path, summarize it, write the checkpoint at the
	// branch point, and the projection must carry it ahead of the new work.
	m, ids, entries := branchedTree(t)
	collected := CollectEntriesForBranchSummary(entries, ids[2], ids[3])
	if got := entryTexts(collected.Entries); strings.Join(got, ",") != "c" {
		t.Fatalf("collected = %v, want [c]", got)
	}

	fake := &capturingStreamFn{events: doneEvents("## Goal\nthe exploration", ai.Usage{TotalTokens: 9})}
	res, err := SummarizeBranch(context.Background(), collected.Entries, BranchSummaryOptions{
		SummaryOptions: testOptions(fake.fn),
	})
	if err != nil {
		t.Fatal(err)
	}

	details := &session.BranchSummaryDetails{ReadFiles: res.ReadFiles, ModifiedFiles: res.ModifiedFiles}
	usage := res.Usage
	if _, err := m.BranchWithSummary(&ids[1], res.Summary, details, &usage); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(ai.NewTextMessage(ai.RoleUser, "back on the main line")); err != nil {
		t.Fatal(err)
	}

	msgs := m.BuildSessionContext().Messages
	if len(msgs) != 4 {
		t.Fatalf("messages = %d (%v), want 4", len(msgs), messageTexts(msgs))
	}
	if ai.MessageText(msgs[0]) != "a" || ai.MessageText(msgs[1]) != "b" {
		t.Errorf("prefix = %v, want [a b]", messageTexts(msgs[:2]))
	}
	// The checkpoint reaches context as a user message (the exact framing
	// is pinned in the session package's projection tests).
	framed := ai.MessageText(msgs[2])
	if msgs[2].Role != ai.RoleUser || !strings.Contains(framed, res.Summary) {
		t.Errorf("messages[2] = %q (%s), want the framed checkpoint", framed, msgs[2].Role)
	}
	if ai.MessageText(msgs[3]) != "back on the main line" {
		t.Errorf("messages[3] = %q, want the new work", ai.MessageText(msgs[3]))
	}
}
