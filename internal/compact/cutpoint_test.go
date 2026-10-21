package compact

import (
	"encoding/json"
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// Test-case shapes ported from pi's compaction.test.ts "findCutPoint" block
// (lines 297-411). The budgets below are chosen to be unambiguous under
// Ultrac's 3.5 chars/token estimator (ai.EstimateMessageTokens), which is
// deliberately shared with the request-side clamp — see cutpoint.go.

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func msgEntry(t *testing.T, id string, msg ai.Message) *session.SessionEntry {
	t.Helper()
	return &session.SessionEntry{Type: session.TypeMessage, ID: id, Message: mustRaw(t, msg)}
}

func userEntry(t *testing.T, id, text string) *session.SessionEntry {
	t.Helper()
	return msgEntry(t, id, ai.NewTextMessage(ai.RoleUser, text))
}

func assistantEntry(t *testing.T, id, text string) *session.SessionEntry {
	t.Helper()
	m := ai.NewTextMessage(ai.RoleAssistant, text)
	m.StopReason = ai.StopStop
	return msgEntry(t, id, m)
}

func toolResultEntry(t *testing.T, id, text string) *session.SessionEntry {
	t.Helper()
	return msgEntry(t, id, ai.NewToolResultMessage("call-1", "read", text, false))
}

// customEntry is a custom_message entry: context-visible, and it projects to
// a user message (session/pi_context.go contextMessages).
func customEntry(t *testing.T, id, content string) *session.SessionEntry {
	t.Helper()
	return &session.SessionEntry{
		Type:  session.TypeCustomMessage,
		ID:    id,
		Extra: map[string]json.RawMessage{"content": mustRaw(t, content)},
	}
}

func TestFindCutPointKeepsEverythingWhenItFits(t *testing.T) {
	// pi: "should keep everything if all messages fit within budget".
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "1"),
		assistantEntry(t, "e1", "a"),
		userEntry(t, "e2", "2"),
		assistantEntry(t, "e3", "b"),
	}
	got := FindCutPoint(entries, 0, len(entries), 50000)
	want := CutPoint{FirstKeptEntryIndex: 0, TurnStartIndex: -1}
	if got != want {
		t.Fatalf("FindCutPoint = %+v, want %+v", got, want)
	}
}

func TestFindCutPointNoValidCutPointsInWindow(t *testing.T) {
	// A window holding only a tool result has nowhere to cut: the result
	// must stay with its call. pi's equivalent case asserts the window
	// start is returned (compaction.test.ts:318-322).
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "before the window"),
		assistantEntry(t, "e1", "call"),
		toolResultEntry(t, "e2", "result"),
	}
	got := FindCutPoint(entries, 2, 3, 1000)
	want := CutPoint{FirstKeptEntryIndex: 2, TurnStartIndex: -1}
	if got != want {
		t.Fatalf("FindCutPoint = %+v, want %+v", got, want)
	}
}

func TestFindCutPointEmptyWindow(t *testing.T) {
	entries := []*session.SessionEntry{userEntry(t, "e0", "hi")}
	got := FindCutPoint(entries, 0, 0, 1000)
	want := CutPoint{FirstKeptEntryIndex: 0, TurnStartIndex: -1}
	if got != want {
		t.Fatalf("FindCutPoint = %+v, want %+v", got, want)
	}
}

func TestFindCutPointSplitTurnAtAssistant(t *testing.T) {
	// pi: "should indicate split turn when cutting at assistant message"
	// (compaction.test.ts:336-356), with a budget that actually bites: the
	// newest assistant alone exceeds it.
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "Turn 1"),
		assistantEntry(t, "e1", "A1"),
		userEntry(t, "e2", "Turn 2"),
		assistantEntry(t, "e3", strings.Repeat("x", 3000)),
	}
	got := FindCutPoint(entries, 0, len(entries), 100)
	want := CutPoint{FirstKeptEntryIndex: 3, TurnStartIndex: 2, IsSplitTurn: true}
	if got != want {
		t.Fatalf("FindCutPoint = %+v, want %+v", got, want)
	}
	if entries[got.FirstKeptEntryIndex].ID != "e3" {
		t.Errorf("cut entry = %s, want e3 (the assistant)", entries[got.FirstKeptEntryIndex].ID)
	}
}

func TestFindCutPointBudgetsCustomMessageEntries(t *testing.T) {
	// pi: "should budget context-visible custom message entries"
	// (compaction.test.ts:358-376). A custom_message entry is both a cut
	// point and a turn start — it projects to a user message.
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "hi"),
		assistantEntry(t, "e1", "hello"),
		customEntry(t, "e2", strings.Repeat("x", 4000)),
		assistantEntry(t, "e3", "ok"),
	}

	tiny := FindCutPoint(entries, 0, len(entries), 1)
	wantTiny := CutPoint{FirstKeptEntryIndex: 3, TurnStartIndex: 2, IsSplitTurn: true}
	if tiny != wantTiny {
		t.Errorf("keepRecent=1: %+v, want %+v", tiny, wantTiny)
	}

	fits := FindCutPoint(entries, 0, len(entries), 2)
	wantFits := CutPoint{FirstKeptEntryIndex: 2, TurnStartIndex: -1}
	if fits != wantFits {
		t.Errorf("keepRecent=2: %+v, want %+v", fits, wantFits)
	}
}

func TestFindCutPointKeepsToolCallBeforeOversizedToolResult(t *testing.T) {
	// Regression port: pi #9740 (compaction.test.ts:378-411). Trailing tool
	// results that alone exceed the budget must keep their preceding
	// assistant call, not fall back to the window's first message — and the
	// resulting cut is a split turn whose prefix is the current user turn.
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "old history"),
		assistantEntry(t, "e1", "old answer"),
		userEntry(t, "e2", "read the large file"),
		msgEntry(t, "e3", ai.Message{
			Role:       ai.RoleAssistant,
			Content:    []ai.ContentBlock{ai.ToolCallBlock("call-1", "read", map[string]any{"path": "big.txt"})},
			StopReason: ai.StopToolUse,
		}),
		toolResultEntry(t, "e4", strings.Repeat("x", 8000)),
	}
	got := FindCutPoint(entries, 0, len(entries), 1000)
	want := CutPoint{FirstKeptEntryIndex: 3, TurnStartIndex: 2, IsSplitTurn: true}
	if got != want {
		t.Fatalf("FindCutPoint = %+v, want %+v", got, want)
	}
	if entries[got.FirstKeptEntryIndex].ID != "e3" {
		t.Errorf("cut entry = %s, want e3 (the tool call, not its result)", entries[got.FirstKeptEntryIndex].ID)
	}
}

func TestFindCutPointRespectsWindowStart(t *testing.T) {
	// The walk may never cut before the window start, even when the whole
	// history would fit the budget: entries before startIndex are already
	// summarized away by an earlier compaction.
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "already summarized"),
		assistantEntry(t, "e1", "already summarized"),
		userEntry(t, "e2", "kept"),
		assistantEntry(t, "e3", "kept"),
	}
	got := FindCutPoint(entries, 2, len(entries), 1)
	want := CutPoint{FirstKeptEntryIndex: 3, TurnStartIndex: 2, IsSplitTurn: true}
	if got != want {
		t.Fatalf("FindCutPoint = %+v, want %+v", got, want)
	}
}

func TestFindCutPointAbsorbsLeadingMetadata(t *testing.T) {
	// A metadata entry (model_change) between the cut and the last visible
	// entry is absorbed into the kept side and becomes the first kept entry
	// (pi's backwards scan, compaction.ts:481-489). It contributes nothing
	// to context, so the reader's window simply starts there.
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "summarized"),
		assistantEntry(t, "e1", "summarized"),
		{Type: session.TypeModelChange, ID: "e2", Provider: "anthropic", ModelID: "claude-test"},
		userEntry(t, "e3", "kept"),
	}
	got := FindCutPoint(entries, 0, len(entries), 1)
	want := CutPoint{FirstKeptEntryIndex: 2, TurnStartIndex: 0, IsSplitTurn: true}
	if got != want {
		t.Fatalf("FindCutPoint = %+v, want %+v", got, want)
	}
	if entries[got.FirstKeptEntryIndex].ID != "e2" {
		t.Errorf("cut entry = %s, want e2 (the absorbed metadata)", entries[got.FirstKeptEntryIndex].ID)
	}
}

func TestFindCutPointStopsAtCompactionBoundary(t *testing.T) {
	// The backwards scan must not walk past a compaction entry: the
	// checkpoint is a hard boundary (pi: compaction.ts:484).
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "kept"),
		{Type: session.TypeCompaction, ID: "e1", Extra: map[string]json.RawMessage{"summary": mustRaw(t, "old")}},
		userEntry(t, "e2", "kept"),
	}
	got := FindCutPoint(entries, 0, len(entries), 1)
	// cutPoints = [0, 2] (the compaction is skipped). The newest entry
	// alone exceeds the budget, so the cut snaps to 2; the scan back stops
	// at the compaction.
	want := CutPoint{FirstKeptEntryIndex: 2, TurnStartIndex: -1}
	if got != want {
		t.Fatalf("FindCutPoint = %+v, want %+v", got, want)
	}
}

func TestFindCutPointClampsOutOfRangeWindow(t *testing.T) {
	entries := []*session.SessionEntry{
		userEntry(t, "e0", "hi"),
		assistantEntry(t, "e1", "hello"),
	}
	// A window past the end is clamped rather than panicking; a negative
	// start is clamped to 0.
	got := FindCutPoint(entries, 0, 99, 50000)
	if got.FirstKeptEntryIndex != 0 {
		t.Errorf("FirstKeptEntryIndex = %d, want 0", got.FirstKeptEntryIndex)
	}
	got = FindCutPoint(entries, -5, len(entries), 50000)
	if got.FirstKeptEntryIndex != 0 {
		t.Errorf("FirstKeptEntryIndex = %d, want 0", got.FirstKeptEntryIndex)
	}
}
