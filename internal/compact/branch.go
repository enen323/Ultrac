package compact

import (
	"context"
	"encoding/json"

	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// Branch summarization for tree navigation — pi's
// core/compaction/branch-summarization.ts, ported for Ultrac (plan P-C3).
//
// When the leaf moves to an earlier entry (session.Branch / BranchWithSummary),
// the path being abandoned can be checkpointed so its context is not lost.
// This file is the pure half: collect the abandoned entries, budget them,
// and turn them into a summary. The caller writes the summary through
// session.BranchWithSummary, which is where the leaf actually moves.
//
// Two deviations from pi, both forced by Go:
//
//   - pi's collectEntriesForBranchSummary takes the session manager; here it
//     takes the full entry slice (SessionManager.TreeEntries). The walk needs
//     both the old and the new path, and a single branch path is not enough
//     — the target may sit on a sibling branch.
//   - pi's generateBranchSummary reports aborts and failures inside its
//     result object; this package returns errors (Summarize does the same),
//     and the aborted case is an error, matching P-C1's decision that a
//     cancelled summarization must not become a checkpoint.

// defaultBranchReserveTokens is pi's reserveTokens default in
// generateBranchSummary (branch-summarization.ts:293-309). Ultrac has no
// branch-summary settings section (plan D6 keeps compaction's three keys),
// so the caller passes the reserve explicitly and 0 falls back to this.
const defaultBranchReserveTokens = 16384

// defaultBranchContextWindow is pi's `model.contextWindow || 128000`
// (branch-summarization.ts:311-312): a model with no recorded window still
// gets a budget rather than an unlimited one.
const defaultBranchContextWindow = 128000

// branchSummaryMaxTokens is pi's flat cap on a branch summary
// (branch-summarization.ts:345) — min(4096, model.maxTokens). Unlike a
// compaction, whose budget scales with the reserve, a branch checkpoint is
// always small.
const branchSummaryMaxTokens = 4096

// branchSummaryGraceFraction is the share of the budget a summary entry may
// overshoot when it does not fit (branch-summarization.ts:232-236): an
// existing checkpoint is important enough to keep past the line, as long as
// the budget is not already 90% spent.
const branchSummaryGraceFraction = 0.9

// BranchSummaryPrompt builds the checkpoint for an abandoned branch.
//
// pi: BRANCH_SUMMARY_PROMPT (branch-summarization.ts:258-285). Verbatim for
// the same reason as the compaction prompts: the text survives in the
// session file and is re-read by both implementations.
const BranchSummaryPrompt = `Create a structured summary of this conversation branch for context when returning later.

Use this EXACT format:

## Goal
[What was the user trying to accomplish in this branch?]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Work that was started but not finished]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [What should happen next to continue this work]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// branchSummaryPreamble frames the summary so a later reader knows it
// describes an exploration the conversation came back from.
//
// pi: BRANCH_SUMMARY_PREAMBLE (branch-summarization.ts:253-256).
const branchSummaryPreamble = `The user explored a different conversation branch before returning here.
Summary of that exploration:

`

// noBranchContent is what a branch with nothing to say reports.
//
// pi: branch-summarization.ts:318-319 (`{ summary: "No content to summarize" }`).
const noBranchContent = "No content to summarize"

// noBranchSummary is the fallback when the call returned no text at all.
//
// pi: branch-summarization.ts:377 (`summary || "No summary generated"`).
const noBranchSummary = "No summary generated"

// CollectEntriesResult is what a navigation would summarize.
//
// pi: CollectEntriesResult (branch-summarization.ts:60-65).
type CollectEntriesResult struct {
	// Entries are the entries to summarize, in chronological order.
	Entries []*session.SessionEntry
	// CommonAncestorID is the deepest entry on both the old and the new
	// path — where the abandoned branch diverged. Empty when the two
	// positions share no ancestor, or the old position is unknown.
	CommonAncestorID string
}

// CollectEntriesForBranchSummary collects the entries a leaf move from
// fromID to toID would abandon: from the old leaf back to (excluding) the
// deepest common ancestor of the two paths.
//
// pi: collectEntriesForBranchSummary (branch-summarization.ts:108-146).
// Compaction boundaries are not a stop condition — their summaries are
// included and become context for the branch checkpoint, same as pi.
//
// entries is the session's full entry set in file order
// (SessionManager.TreeEntries): both paths are walked within it, so one
// branch's path is not enough.
func CollectEntriesForBranchSummary(entries []*session.SessionEntry, fromID, toID string) CollectEntriesResult {
	// No old position means nothing was explored (pi's `!oldLeafId`).
	if fromID == "" {
		return CollectEntriesResult{}
	}
	byID := make(map[string]*session.SessionEntry, len(entries))
	for _, e := range entries {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}

	// The old path, as an id set: every entry from the old leaf to the root.
	oldPath := make(map[string]bool, len(entries))
	for current, ok := byID[fromID]; ok; {
		if current.ID != "" {
			if oldPath[current.ID] {
				break // corrupt parent chain: stop rather than loop
			}
			oldPath[current.ID] = true
		}
		if current.ParentID == nil {
			break
		}
		current, ok = byID[*current.ParentID]
	}

	// The deepest common ancestor: walking up from the target visits
	// ancestors deepest-first, so the first hit on the old path is it (pi
	// iterates the root-first target path backwards for the same entry).
	commonAncestorID := ""
	for current, ok := byID[toID]; ok; {
		if oldPath[current.ID] {
			commonAncestorID = current.ID
			break
		}
		if current.ParentID == nil {
			break
		}
		current, ok = byID[*current.ParentID]
	}

	// Collect from the old leaf up to (excluding) the ancestor. With no
	// common ancestor the walk runs to the root, exactly like pi's while
	// loop against a null commonAncestorId.
	var collected []*session.SessionEntry
	for current, ok := byID[fromID]; ok && current.ID != commonAncestorID; {
		collected = append(collected, current)
		if current.ParentID == nil {
			break
		}
		current, ok = byID[*current.ParentID]
	}
	// The walk collected leaf-first; reverse for chronological order.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return CollectEntriesResult{Entries: collected, CommonAncestorID: commonAncestorID}
}

// BranchSummaryOptions is everything SummarizeBranch needs besides the
// entries.
type BranchSummaryOptions struct {
	SummaryOptions
	// CustomInstructions focuses the summary. Appended to the default
	// prompt as "Additional focus", unless ReplaceInstructions is set.
	CustomInstructions string
	// ReplaceInstructions makes CustomInstructions replace the default
	// prompt instead of being appended to it (pi's replaceInstructions).
	ReplaceInstructions bool
	// ReserveTokens sizes the token budget: contextWindow - ReserveTokens.
	// 0 means defaultBranchReserveTokens.
	ReserveTokens int
}

// BranchSummaryResult is one generated branch checkpoint.
//
// pi: BranchSummaryResult (branch-summarization.ts:34-41), minus the
// aborted/error fields — this package reports failures as errors.
type BranchSummaryResult struct {
	Summary       string
	Usage         ai.Usage
	ReadFiles     []string
	ModifiedFiles []string
}

// SummarizeBranch generates the checkpoint for a set of abandoned entries
// (CollectEntriesForBranchSummary's output).
//
// pi: generateBranchSummary (branch-summarization.ts:293-382). The entries
// are budgeted newest-first, serialized inside <conversation> tags, and
// summarized with BRANCH_SUMMARY_PROMPT under the shared summarization
// system prompt; the result carries the preamble and the file inventory.
func SummarizeBranch(ctx context.Context, entries []*session.SessionEntry, opts BranchSummaryOptions) (BranchSummaryResult, error) {
	reserveTokens := opts.ReserveTokens
	if reserveTokens <= 0 {
		reserveTokens = defaultBranchReserveTokens
	}
	contextWindow := opts.Model.ContextWindow
	if contextWindow <= 0 {
		contextWindow = defaultBranchContextWindow
	}
	tokenBudget := contextWindow - reserveTokens

	messages, fileOps, _ := prepareBranchEntries(entries, tokenBudget)
	if len(messages) == 0 {
		return BranchSummaryResult{Summary: noBranchContent}, nil
	}

	instructions := BranchSummaryPrompt
	switch {
	case opts.ReplaceInstructions && opts.CustomInstructions != "":
		instructions = opts.CustomInstructions
	case opts.CustomInstructions != "":
		instructions = BranchSummaryPrompt + "\n\nAdditional focus: " + opts.CustomInstructions
	}
	promptText := "<conversation>\n" + SerializeConversation(messages) + "\n</conversation>\n\n" + instructions

	// pi's branch path destructures no thinkingLevel from its options
	// (branch-summarization.ts:309-312) even though the caller computes one:
	// the request runs without reasoning. The shared summarize() would
	// otherwise apply the caller's level, so it is cleared here.
	opts.Reasoning = ""

	result, err := summarize(ctx, promptText, branchMaxTokens(opts.Model), "Branch summarization", opts.SummaryOptions)
	if err != nil {
		return BranchSummaryResult{}, err
	}

	summary := branchSummaryPreamble + result.Text
	readFiles, modifiedFiles := ComputeFileLists(fileOps)
	summary += FormatFileOperations(readFiles, modifiedFiles)
	if summary == "" {
		summary = noBranchSummary
	}
	return BranchSummaryResult{
		Summary:       summary,
		Usage:         result.Usage,
		ReadFiles:     readFiles,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// branchMaxTokens is pi's min(4096, model.maxTokens)
// (branch-summarization.ts:345).
func branchMaxTokens(model ai.Model) int {
	if model.MaxTokens > 0 && model.MaxTokens < branchSummaryMaxTokens {
		return model.MaxTokens
	}
	return branchSummaryMaxTokens
}

// prepareBranchEntries budgets a set of entries into the messages a branch
// summary is built from.
//
// pi: prepareBranchEntries (branch-summarization.ts:195-247). Two passes:
// the first harvests file operations from every branch_summary's details —
// nested checkpoints carry the cumulative inventory even when the budget
// drops them — and the second walks newest to oldest until the budget is
// spent, so the most recent context survives.
func prepareBranchEntries(entries []*session.SessionEntry, tokenBudget int) ([]ai.Message, FileOperations, int) {
	messages := []ai.Message{}
	fileOps := CreateFileOps()
	totalTokens := 0

	for _, entry := range entries {
		if entry.Type != session.TypeBranchSummary || entryFromHook(entry) {
			continue
		}
		if details := decodeBranchSummaryDetails(entry); details != nil {
			for _, f := range details.ReadFiles {
				fileOps.Read[f] = true
			}
			for _, f := range details.ModifiedFiles {
				// Modified files go into edited as well, so a file both
				// read and written here resolves as modified (pi's
				// comment, branch-summarization.ts:210-213).
				fileOps.Edited[f] = true
			}
		}
	}

	// Second pass: newest to oldest, until the budget is spent. The break
	// exits the whole walk (pi's `break` leaves the entry loop, not just
	// the message): once an entry does not fit, nothing older is reached.
walk:
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		isSummaryEntry := entry.Type == session.TypeCompaction || entry.Type == session.TypeBranchSummary
		for _, m := range branchSummaryMessages(entry) {
			ExtractFileOpsFromMessage(m, fileOps)
			tokens := ai.EstimateMessageTokens(m)
			if tokenBudget > 0 && totalTokens+tokens > tokenBudget {
				// A checkpoint overshoots the line only while the budget
				// is mostly unspent (pi's grace, :231-238); either way the
				// walk stops here.
				if isSummaryEntry && totalTokens < int(float64(tokenBudget)*branchSummaryGraceFraction) {
					messages = append([]ai.Message{m}, messages...)
					totalTokens += tokens
				}
				break walk
			}
			messages = append([]ai.Message{m}, messages...)
			totalTokens += tokens
		}
	}
	return messages, fileOps, totalTokens
}

// branchSummaryMessages returns the messages one entry contributes to a
// branch summary — pi's getMessageFromEntry
// (branch-summarization.ts:156-180) over Ultrac's projection.
//
// A toolResult entry is skipped outright (its context rides in the
// assistant's tool call), and a compaction entry's system snapshot stays
// out: pi's conversion of a compaction yields the checkpoint alone.
func branchSummaryMessages(e *session.SessionEntry) []ai.Message {
	var out []ai.Message
	for _, m := range session.EntryContextMessages(e) {
		switch {
		case m.Role == ai.RoleToolResult:
			continue
		case m.Role == ai.RoleSystem && e.Type == session.TypeCompaction:
			continue
		}
		out = append(out, m)
	}
	return out
}

// decodeBranchSummaryDetails reads a branch_summary entry's details, nil
// when absent or unreadable.
func decodeBranchSummaryDetails(e *session.SessionEntry) *session.BranchSummaryDetails {
	raw, ok := e.Extra["details"]
	if !ok {
		return nil
	}
	var details session.BranchSummaryDetails
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil
	}
	return &details
}
