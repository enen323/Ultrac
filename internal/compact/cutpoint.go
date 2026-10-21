package compact

import (
	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// The cut-point walk: where in the entry list does the summarized part end
// and the kept part begin.
//
// Ported from pi's compaction.ts:351-501. Two rules carry the design:
//
//   - A cut point is a user-like or assistant entry, never a tool result —
//     a tool result must stay with the call that produced it.
//   - Cutting at an assistant message splits its turn: the turn's opening
//     user message is reported back so the caller can summarize the prefix
//     separately (TURN_PREFIX_SUMMARIZATION_PROMPT).

// CutPoint is where a compaction would split the entry list.
//
// pi: CutPointResult (compaction.ts:421-428).
type CutPoint struct {
	// FirstKeptEntryIndex is the index of the first entry to keep.
	FirstKeptEntryIndex int
	// TurnStartIndex is the index of the user-like entry that starts the
	// turn being split, or -1 when the cut does not split a turn.
	TurnStartIndex int
	// IsSplitTurn reports whether the cut falls inside a turn (i.e. the
	// kept part starts mid-turn and its prefix needs its own summary).
	IsSplitTurn bool
}

// FindCutPoint finds the entry index that keeps approximately
// keepRecentTokens of the newest context.
//
// The walk goes from newest to oldest accumulating per-message estimates and
// stops once the budget is met; the cut then snaps to the closest valid cut
// point at or after that entry (so trailing tool results that alone exceed
// the budget still keep their preceding tool call). Metadata entries that
// contribute nothing to context (model_change, thinking_level_change, ...)
// are absorbed into the kept side.
//
// Only entries in [startIndex, endIndex) are considered. The returned index
// is absolute in entries, not relative to the window.
//
// pi: findCutPoint (compaction.ts:446-501).
func FindCutPoint(entries []*session.SessionEntry, startIndex, endIndex, keepRecentTokens int) CutPoint {
	// Clamp the window so a caller passing stale bounds cannot walk off the
	// slice; pi trusts its callers' arithmetic, Go prefers not to.
	if startIndex < 0 {
		startIndex = 0
	}
	if endIndex > len(entries) {
		endIndex = len(entries)
	}
	cutPoints := validCutPoints(entries, startIndex, endIndex)
	if len(cutPoints) == 0 {
		// Nothing in the window can be cut at: keep everything from the
		// window start (pi's cutPoints.length === 0 branch).
		return CutPoint{FirstKeptEntryIndex: startIndex, TurnStartIndex: -1}
	}

	accumulated := 0
	cutIndex := cutPoints[0] // default: keep from the window's first cut point
	for i := endIndex - 1; i >= startIndex; i-- {
		tokens := entryTokens(entries[i])
		if tokens == 0 {
			continue
		}
		accumulated += tokens
		if accumulated >= keepRecentTokens {
			// Prefer the closest valid cut point at or after this entry; if
			// the trailing tool results exceed the budget by themselves,
			// keep their preceding assistant call instead of falling all
			// the way back to the window's first message.
			cutIndex = firstAtOrAfter(cutPoints, i)
			break
		}
	}

	// Absorb adjacent context-invisible metadata into the kept side.
	for cutIndex > startIndex {
		prev := entries[cutIndex-1]
		if prev.Type == session.TypeCompaction || len(session.EntryContextMessages(prev)) > 0 {
			break
		}
		cutIndex--
	}

	cutEntry := entries[cutIndex]
	startsTurn := isTurnStartEntry(cutEntry)
	turnStart := -1
	if !startsTurn {
		turnStart = findTurnStartIndex(entries, cutIndex, startIndex)
	}
	return CutPoint{
		FirstKeptEntryIndex: cutIndex,
		TurnStartIndex:      turnStart,
		IsSplitTurn:         !startsTurn && turnStart != -1,
	}
}

// validCutPoints lists the indices of entries the cut may land on.
//
// pi: findValidCutPoints (compaction.ts:394-406). Compaction entries are
// skipped outright: the cut must never land on a checkpoint.
func validCutPoints(entries []*session.SessionEntry, startIndex, endIndex int) []int {
	var cutPoints []int
	for i := startIndex; i < endIndex; i++ {
		if entries[i].Type == session.TypeCompaction {
			continue
		}
		if isCutPointEntry(entries[i]) {
			cutPoints = append(cutPoints, i)
		}
	}
	return cutPoints
}

// isCutPointEntry reports whether the cut may land on this entry.
//
// pi: isCutPointMessage (compaction.ts:351-364) applied per entry. Ultrac's
// projection has already folded every custom role (custom_message,
// branch_summary) into a user message, so the role check is just
// user-or-assistant; toolResult is the one that must never be cut at.
func isCutPointEntry(e *session.SessionEntry) bool {
	for _, m := range session.EntryContextMessages(e) {
		switch m.Role {
		case ai.RoleUser, ai.RoleAssistant:
			return true
		}
	}
	return false
}

// isTurnStartEntry reports whether this entry opens a turn.
//
// pi: isTurnStartMessage + isTurnStartEntry (compaction.ts:366-386). A
// compaction entry never starts a turn — its checkpoint belongs to whatever
// came before it.
func isTurnStartEntry(e *session.SessionEntry) bool {
	if e.Type == session.TypeCompaction {
		return false
	}
	for _, m := range session.EntryContextMessages(e) {
		if m.Role == ai.RoleUser {
			return true
		}
	}
	return false
}

// findTurnStartIndex scans backwards from entryIndex for the turn opener.
//
// pi: findTurnStartIndex (compaction.ts:412-419). Returns -1 when the window
// holds no turn start at or before the index.
func findTurnStartIndex(entries []*session.SessionEntry, entryIndex, startIndex int) int {
	for i := entryIndex; i >= startIndex; i-- {
		if isTurnStartEntry(entries[i]) {
			return i
		}
	}
	return -1
}

// entryTokens sums the projected messages' estimates for one entry.
func entryTokens(e *session.SessionEntry) int {
	tokens := 0
	for _, m := range session.EntryContextMessages(e) {
		tokens += ai.EstimateMessageTokens(m)
	}
	return tokens
}

// firstAtOrAfter returns the first cut point at or after i, or the last one
// when every cut point precedes i (pi's `?? cutPoints[cutPoints.length - 1]`
// fallback, compaction.ts:476).
func firstAtOrAfter(cutPoints []int, i int) int {
	for _, c := range cutPoints {
		if c >= i {
			return c
		}
	}
	return cutPoints[len(cutPoints)-1]
}
