package compact

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// Branch builders over a real (in-memory) SessionManager: the entries carry
// the ids and parent links the projection walks.

func buildBranch(t *testing.T, messages ...ai.Message) (*session.SessionManager, []*session.SessionEntry) {
	t.Helper()
	m := session.InMemorySession(`D:\repo`)
	for _, msg := range messages {
		if _, err := m.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	return m, m.GetBranch("")
}

func bigAssistant(t *testing.T, text string, calls ...ai.ContentBlock) ai.Message {
	t.Helper()
	content := []ai.ContentBlock{ai.TextBlock(text)}
	content = append(content, calls...)
	m := ai.Message{Role: ai.RoleAssistant, Content: content, StopReason: ai.StopStop}
	return m
}

func readCall(path string) ai.ContentBlock {
	return ai.ToolCallBlock("call-"+path, "read", map[string]any{"path": path})
}

func messageTexts(messages []ai.Message) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		out[i] = ai.MessageText(m)
	}
	return out
}

// summarizeSession is the scenario-A fixture: two turns, the second ending in
// a large assistant message, so a 100-token keep-recent window cuts inside
// the second turn.
func summarizeSession(t *testing.T) (*session.SessionManager, []*session.SessionEntry) {
	t.Helper()
	return buildBranch(t,
		ai.NewTextMessage(ai.RoleUser, "first question"),
		bigAssistant(t, "first answer", readCall("go.mod")),
		ai.NewTextMessage(ai.RoleUser, "second question"),
		bigAssistant(t, strings.Repeat("x", 3000), readCall("main.go")),
	)
}

func testSettings() Settings {
	return Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 100}
}

func TestPrepareSplitTurn(t *testing.T) {
	_, branch := summarizeSession(t)
	prep := Prepare(branch, testSettings())
	if prep == nil {
		t.Fatal("Prepare = nil, want a split-turn plan")
	}
	if got, want := messageTexts(prep.MessagesToSummarize), []string{"first question", "first answer"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("messagesToSummarize = %v, want %v", got, want)
	}
	if got, want := messageTexts(prep.TurnPrefixMessages), []string{"second question"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("turnPrefixMessages = %v, want %v", got, want)
	}
	if !prep.IsSplitTurn {
		t.Error("IsSplitTurn = false, want true")
	}
	if prep.FirstKeptEntryID != branch[3].ID {
		t.Errorf("firstKept = %s, want %s (the large assistant)", prep.FirstKeptEntryID, branch[3].ID)
	}
	if prep.PreviousSummary != "" {
		t.Errorf("previousSummary = %q, want empty", prep.PreviousSummary)
	}
	wantTokens := ai.EstimateContextTokens([]ai.Message{
		ai.NewTextMessage(ai.RoleUser, "first question"),
		bigAssistant(t, "first answer", readCall("go.mod")),
		ai.NewTextMessage(ai.RoleUser, "second question"),
		bigAssistant(t, strings.Repeat("x", 3000), readCall("main.go")),
	})
	if prep.TokensBefore != wantTokens {
		t.Errorf("tokensBefore = %d, want %d", prep.TokensBefore, wantTokens)
	}
	// File ops of the summarized window only (the kept side's "main.go"
	// read is not part of this compaction).
	if !prep.FileOps.Read["go.mod"] || prep.FileOps.Read["main.go"] {
		t.Errorf("fileOps.read = %v, want only go.mod", prep.FileOps.Read)
	}
}

func TestPrepareSkipsSystemMessagesInHistory(t *testing.T) {
	// System messages are prompt state, not conversation history: they must
	// not be summarized (pi: getMessagesFromProjectedEntryForCompaction,
	// compaction.ts:98-102). The compaction entry itself is skipped for the
	// same reason — its checkpoint is the previous summary.
	m := session.InMemorySession(`D:\repo`)
	for _, msg := range []ai.Message{
		ai.NewTextMessage(ai.RoleSystem, "you are a coding assistant"),
		ai.NewTextMessage(ai.RoleUser, "q1"),
		bigAssistant(t, strings.Repeat("x", 3000)),
		ai.NewTextMessage(ai.RoleUser, "q2"),
		bigAssistant(t, "s"),
	} {
		if _, err := m.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	// keepRecent = 2 lands the cut on the q2 user entry (not a split), so
	// the whole window before it — system, q1, a1 — is the history.
	prep := Prepare(m.GetBranch(""), Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 2})
	if prep == nil {
		t.Fatal("Prepare = nil")
	}
	if prep.IsSplitTurn {
		t.Fatalf("IsSplitTurn = true, want a cut on the user entry")
	}
	for _, msg := range prep.MessagesToSummarize {
		if msg.Role == ai.RoleSystem {
			t.Fatalf("system message in history: %q", ai.MessageText(msg))
		}
	}
	big := strings.Repeat("x", 3000)
	if got, want := messageTexts(prep.MessagesToSummarize), []string{"q1", big}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("messagesToSummarize = %d messages, want [q1, the assistant] without the system prompt", len(got))
	}
}

func TestPrepareNothingToCompact(t *testing.T) {
	// Empty branch.
	if prep := Prepare(nil, testSettings()); prep != nil {
		t.Errorf("Prepare(nil) = %+v, want nil", prep)
	}
	// Setup entries only: no context-visible messages anywhere.
	m := session.InMemorySession(`D:\repo`)
	if _, err := m.AppendModelChange("anthropic", "claude-test"); err != nil {
		t.Fatal(err)
	}
	if prep := Prepare(m.GetBranch(""), testSettings()); prep != nil {
		t.Errorf("Prepare(metadata only) = %+v, want nil", prep)
	}
	// Everything fits the keep-recent window.
	_, branch := buildBranch(t,
		ai.NewTextMessage(ai.RoleUser, "hi"),
		bigAssistant(t, "hello"),
	)
	if prep := Prepare(branch, Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 100000}); prep != nil {
		t.Errorf("Prepare(small session) = %+v, want nil", prep)
	}
}

func TestPrepareSkipsWhenNewestEntryIsCompaction(t *testing.T) {
	m, branch := summarizeSession(t)
	prep := Prepare(branch, testSettings())
	if prep == nil {
		t.Fatal("first Prepare = nil")
	}
	result := runWithTexts(t, prep, "S1")
	if _, err := m.AppendCompaction(result.Summary, result.FirstKeptEntryID, result.TokensBefore, &result.Details, nil, &result.Usage); err != nil {
		t.Fatal(err)
	}
	if again := Prepare(m.GetBranch(""), testSettings()); again != nil {
		t.Errorf("Prepare after compaction = %+v, want nil (already compacted)", again)
	}
}

func TestPrepareSecondCompactionResumesAtBoundary(t *testing.T) {
	// The twice-compacted session (plan R7): the second plan starts after
	// the first compaction's checkpoint, carries its summary forward for
	// the update prompt, and re-summarizes only what the moving window left
	// behind.
	m, branch := summarizeSession(t)
	prep := Prepare(branch, testSettings())
	result := runWithTexts(t, prep, "S1")
	if _, err := m.AppendCompaction(result.Summary, result.FirstKeptEntryID, result.TokensBefore, &result.Details, nil, &result.Usage); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []ai.Message{
		ai.NewTextMessage(ai.RoleUser, "third question"),
		bigAssistant(t, strings.Repeat("y", 3000), readCall("new.go")),
	} {
		if _, err := m.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}

	branch2 := m.GetBranch("")
	prep2 := Prepare(branch2, testSettings())
	if prep2 == nil {
		t.Fatal("second Prepare = nil")
	}
	if prep2.PreviousSummary != result.Summary {
		t.Errorf("previousSummary = %q, want the first result %q", prep2.PreviousSummary, result.Summary)
	}
	if got := messageTexts(prep2.MessagesToSummarize); len(got) != 1 || !strings.HasPrefix(got[0], strings.Repeat("x", 100)) {
		t.Errorf("messagesToSummarize = %d messages (%q...), want only the previously-kept assistant", len(got), got)
	}
	if got, want := messageTexts(prep2.TurnPrefixMessages), []string{"third question"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("turnPrefixMessages = %v, want %v", got, want)
	}
	if prep2.FirstKeptEntryID != branch2[len(branch2)-1].ID {
		t.Errorf("firstKept = %s, want the newest entry", prep2.FirstKeptEntryID)
	}
	// The first checkpoint's file inventory carries forward (go.mod), and
	// the re-summarized assistant's read (main.go) joins it. new.go is read
	// by the KEPT assistant, so it is not part of this compaction.
	if !prep2.FileOps.Read["go.mod"] || !prep2.FileOps.Read["main.go"] || prep2.FileOps.Read["new.go"] {
		t.Errorf("fileOps.read = %v, want go.mod and main.go only", prep2.FileOps.Read)
	}
}

func TestPrepareCarriesFileOpsOnlyFromNonHookCompactions(t *testing.T) {
	// pi skips details of fromHook compactions (compaction.ts:69-71): a
	// hook-originated summary is not pi's inventory to extend.
	hooked := &session.SessionEntry{
		Type:  session.TypeCompaction,
		ID:    "c1",
		Extra: map[string]json.RawMessage(nil),
	}
	hooked.Extra = map[string]json.RawMessage{
		"summary":  mustRaw(t, "hooked summary"),
		"fromHook": mustRaw(t, true),
		"details":  mustRaw(t, map[string]any{"readFiles": []string{"hooked.go"}, "modifiedFiles": []string{}}),
	}
	branch := []*session.SessionEntry{
		hooked,
		userEntry(t, "u1", "question"),
		assistantEntry(t, "a1", strings.Repeat("x", 3000)),
	}
	prep := Prepare(branch, testSettings())
	if prep == nil {
		t.Fatal("Prepare = nil")
	}
	if prep.FileOps.Read["hooked.go"] {
		t.Error("fromHook details carried forward")
	}
	if prep.PreviousSummary != "hooked summary" {
		t.Errorf("previousSummary = %q", prep.PreviousSummary)
	}

	// The same entry without fromHook carries its inventory.
	plain := &session.SessionEntry{
		Type: session.TypeCompaction,
		ID:   "c2",
		Extra: map[string]json.RawMessage{
			"summary": mustRaw(t, "plain summary"),
			"details": mustRaw(t, map[string]any{"readFiles": []string{"plain.go"}, "modifiedFiles": []string{"m.go"}}),
		},
	}
	branch[0] = plain
	prep = Prepare(branch, testSettings())
	if prep == nil {
		t.Fatal("Prepare = nil")
	}
	if !prep.FileOps.Read["plain.go"] || !prep.FileOps.Edited["m.go"] {
		t.Errorf("fileOps = %v / %v, want plain.go read and m.go edited", prep.FileOps.Read, prep.FileOps.Edited)
	}
}

func TestTokensBeforeDistrustsUsagePredatingCompaction(t *testing.T) {
	withUsage := func() ai.Message {
		m := bigAssistant(t, "answer with usage")
		usage := ai.Usage{Input: 10, Output: 5, TotalTokens: 50000}
		m.Usage = &usage
		return m
	}

	// No compaction: the usage is the newest anchor and is trusted.
	_, branch := buildBranch(t,
		ai.NewTextMessage(ai.RoleUser, "q1"),
		withUsage(),
		ai.NewTextMessage(ai.RoleUser, "q2"),
		bigAssistant(t, strings.Repeat("x", 3000)),
	)
	prep := Prepare(branch, testSettings())
	if prep == nil {
		t.Fatal("Prepare = nil")
	}
	wantTrusted := ai.EstimateContextTokens([]ai.Message{
		ai.NewTextMessage(ai.RoleUser, "q1"),
		withUsage(),
		ai.NewTextMessage(ai.RoleUser, "q2"),
		bigAssistant(t, strings.Repeat("x", 3000)),
	})
	if prep.TokensBefore != wantTrusted {
		t.Errorf("tokensBefore = %d, want the usage-anchored %d", prep.TokensBefore, wantTrusted)
	}

	// With a compaction after the usage: that usage describes a context
	// the compaction replaced, so the projection is estimated from scratch
	// (pi: estimateProjectedContextTokens, compaction.ts:226-262). The
	// compaction keeps everything from q1, so the usage-carrying assistant
	// is still projected — just invalidated.
	m, branch2 := buildBranch(t,
		ai.NewTextMessage(ai.RoleUser, "q1"),
		withUsage(),
		ai.NewTextMessage(ai.RoleUser, "q2"),
	)
	if _, err := m.AppendCompaction("prev", branch2[0].ID, 1, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []ai.Message{
		ai.NewTextMessage(ai.RoleUser, "q3"),
		bigAssistant(t, strings.Repeat("x", 3000)),
	} {
		if _, err := m.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	prep = Prepare(m.GetBranch(""), testSettings())
	if prep == nil {
		t.Fatal("Prepare = nil")
	}
	final := m.GetBranch("")
	fromScratch := 0
	for _, msg := range session.ProjectedMessages(session.BuildProjection(final)) {
		if msg.Role != ai.RoleSystem {
			fromScratch += ai.EstimateMessageTokens(msg)
		}
	}
	if prep.TokensBefore != fromScratch {
		t.Errorf("tokensBefore = %d, want the from-scratch %d", prep.TokensBefore, fromScratch)
	}
}

// runWithTexts runs a preparation against a stub that answers every call with
// the given text, and fails the test on error.
func runWithTexts(t *testing.T, prep *Preparation, texts ...string) *Result {
	t.Helper()
	events := make([][]ai.AssistantEvent, len(texts))
	for i, text := range texts {
		events[i] = doneEvents(text, ai.Usage{Input: 1, Output: 1, TotalTokens: 2})
	}
	queue := &queueStreamFn{events: events}
	opts := RunOptions{SummaryOptions: testOptions(queue.fn)}
	result, err := Run(context.Background(), prep, opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRunNonSplitTurn(t *testing.T) {
	// A cut that lands on a user message is not a split turn: one summary
	// call, file ops appended as XML.
	_, branch := buildBranch(t,
		ai.NewTextMessage(ai.RoleUser, "q1"),
		bigAssistant(t, strings.Repeat("x", 3000), readCall("go.mod")),
		ai.NewTextMessage(ai.RoleUser, "q2"),
		bigAssistant(t, "small"),
	)
	// keepRecent = 3: the two small trailing entries together reach it, so
	// the cut snaps to the user entry between them.
	prep := Prepare(branch, Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 3})
	if prep == nil {
		t.Fatal("Prepare = nil")
	}
	if prep.IsSplitTurn {
		t.Fatalf("IsSplitTurn = true, want a cut on the user entry (%+v)", prep)
	}
	result := runWithTexts(t, prep, "SUMMARY")
	want := "SUMMARY\n\n<read-files>\ngo.mod\n</read-files>"
	if result.Summary != want {
		t.Errorf("summary = %q, want %q", result.Summary, want)
	}
	if strings.Join(result.Details.ReadFiles, ",") != "go.mod" {
		t.Errorf("details.readFiles = %v", result.Details.ReadFiles)
	}
	if result.FirstKeptEntryID != prep.FirstKeptEntryID {
		t.Errorf("firstKept = %s, want %s", result.FirstKeptEntryID, prep.FirstKeptEntryID)
	}
}

func TestRunSplitTurnMergesTwoSummaries(t *testing.T) {
	_, branch := summarizeSession(t)
	prep := Prepare(branch, testSettings())
	queue := &queueStreamFn{events: [][]ai.AssistantEvent{
		doneEvents("HISTORY", ai.Usage{Input: 10, Output: 2, TotalTokens: 12}),
		doneEvents("PREFIX", ai.Usage{Input: 20, Output: 3, TotalTokens: 23}),
	}}
	opts := RunOptions{SummaryOptions: testOptions(queue.fn)}
	result, err := Run(context.Background(), prep, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "HISTORY" + splitTurnSeparator + "PREFIX" + "\n\n<read-files>\ngo.mod\n</read-files>"
	if result.Summary != want {
		t.Errorf("summary = %q, want %q", result.Summary, want)
	}
	if result.Usage.Input != 30 || result.Usage.Output != 5 || result.Usage.TotalTokens != 35 {
		t.Errorf("usage = %+v, want the combined totals", result.Usage)
	}
	if len(queue.calls) != 2 {
		t.Fatalf("%d summarization calls, want 2", len(queue.calls))
	}
	if !strings.HasPrefix(queue.calls[1].prompt, "# Conversation\n") {
		t.Errorf("second call is not the turn-prefix prompt: %q", queue.calls[1].prompt)
	}
}

func TestRunSplitTurnWithoutHistoryMessages(t *testing.T) {
	// A split turn whose prefix is the whole window (nothing before it):
	// no history call, the placeholder stands in for the missing history.
	_, branch := buildBranch(t,
		ai.NewTextMessage(ai.RoleUser, "the only question"),
		bigAssistant(t, strings.Repeat("x", 3000)),
	)
	prep := Prepare(branch, testSettings())
	if prep == nil || !prep.IsSplitTurn {
		t.Fatalf("prep = %+v, want a split turn", prep)
	}
	queue := &queueStreamFn{events: [][]ai.AssistantEvent{
		doneEvents("PREFIX", ai.Usage{TotalTokens: 9}),
	}}
	opts := RunOptions{SummaryOptions: testOptions(queue.fn)}
	result, err := Run(context.Background(), prep, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := noPriorHistory + splitTurnSeparator + "PREFIX"
	if result.Summary != want {
		t.Errorf("summary = %q, want %q", result.Summary, want)
	}
	if result.Usage.TotalTokens != 9 {
		t.Errorf("usage = %+v, want the prefix usage alone", result.Usage)
	}
	if len(queue.calls) != 1 {
		t.Errorf("%d calls, want 1 (no history to summarize)", len(queue.calls))
	}
}

func TestRunSplitTurnUsesUpdatePromptWithPreviousSummary(t *testing.T) {
	m, branch := summarizeSession(t)
	prep := Prepare(branch, testSettings())
	first := runWithTexts(t, prep, "S1")
	if _, err := m.AppendCompaction(first.Summary, first.FirstKeptEntryID, first.TokensBefore, &first.Details, nil, &first.Usage); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(ai.NewTextMessage(ai.RoleUser, "third question")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(bigAssistant(t, strings.Repeat("y", 3000))); err != nil {
		t.Fatal(err)
	}
	prep2 := Prepare(m.GetBranch(""), testSettings())
	if prep2 == nil {
		t.Fatal("second Prepare = nil")
	}
	queue := &queueStreamFn{events: [][]ai.AssistantEvent{
		doneEvents("HISTORY", ai.Usage{}),
		doneEvents("PREFIX", ai.Usage{}),
	}}
	opts := RunOptions{SummaryOptions: testOptions(queue.fn)}
	if _, err := Run(context.Background(), prep2, opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(queue.calls[0].prompt, "<previous-summary>\n"+first.Summary+"\n</previous-summary>") {
		t.Errorf("history call did not carry the previous summary: %q", queue.calls[0].prompt)
	}
}

func TestRunFailureWritesNothing(t *testing.T) {
	_, branch := summarizeSession(t)
	prep := Prepare(branch, testSettings())
	errorMessage := ai.NewTextMessage(ai.RoleAssistant, "")
	errorMessage.StopReason = ai.StopError
	errorMessage.ErrorMessage = "provider down"
	queue := &queueStreamFn{events: [][]ai.AssistantEvent{
		{ai.EvError{Reason: ai.StopError, Error: errorMessage}},
	}}
	opts := RunOptions{SummaryOptions: testOptions(queue.fn)}
	result, err := Run(context.Background(), prep, opts)
	if err == nil {
		t.Fatal("Run succeeded on a failed summarization")
	}
	if result != nil {
		t.Errorf("result = %+v, want nil", result)
	}
	if !strings.Contains(err.Error(), "provider down") {
		t.Errorf("error = %v, want the provider message", err)
	}
}
