package compact

import (
	"context"
	"encoding/json"
	"errors"

	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// The compaction engine's orchestration: Prepare plans a compaction against a
// branch without touching the network, Run executes it with one or two LLM
// calls. The caller persists the Result through session.AppendCompaction.
//
// Ported from pi's prepareCompaction + compact (compaction.ts:872-1071),
// with the projected cut-point walk this repo's entry-level FindCutPoint runs
// over (cutpoint.go).

// Preparation is everything a compaction needs before the first LLM call.
//
// pi: CompactionPreparation (compaction.ts:772-788). A nil Preparation means
// "nothing to compact" — the session is empty, or its newest entry is already
// a compaction.
type Preparation struct {
	// FirstKeptEntryID is the entry the kept context starts at.
	FirstKeptEntryID string
	// MessagesToSummarize are the messages the checkpoint replaces.
	MessagesToSummarize []ai.Message
	// TurnPrefixMessages are the messages of a split turn's prefix, when
	// the cut fell inside a turn.
	TurnPrefixMessages []ai.Message
	// IsSplitTurn reports whether TurnPrefixMessages is in play.
	IsSplitTurn bool
	// TokensBefore is the estimated context size at planning time.
	TokensBefore int
	// PreviousSummary is the last checkpoint on the branch, when there is
	// one: the new summary updates it instead of starting over.
	PreviousSummary string
	// FileOps accumulates the file operations of everything being
	// summarized (plus the previous checkpoint's).
	FileOps FileOperations
	// Settings is the configuration the plan was computed under.
	Settings Settings
}

// Result is a finished compaction, ready to append.
//
// pi: CompactionResult (compaction.ts:104-114). estimatedTokensAfter is
// absent because pi's compact() never sets it either — the caller can
// re-estimate the compressed context if a UI wants the number.
type Result struct {
	Summary          string
	FirstKeptEntryID string
	TokensBefore     int
	Usage            ai.Usage
	Details          session.CompactionDetails
}

// RunOptions carries the model and credentials for the summarization calls,
// plus the caller's optional focus for the summary.
type RunOptions struct {
	SummaryOptions
	// CustomInstructions is appended to the prompt as "Additional focus"
	// (pi's customInstructions, compaction.ts:719-721).
	CustomInstructions string
}

// Prepare plans a compaction over a root-first branch (GetBranch's output).
//
// It returns nil when there is nothing to do: an empty branch, a branch whose
// newest entry is already a compaction (pi's "Already compacted"), or a
// branch whose entire content fits in the keep-recent window.
//
// pi: prepareCompaction (compaction.ts:872-936).
func Prepare(branch []*session.SessionEntry, s Settings) *Preparation {
	if len(branch) > 0 && branch[len(branch)-1].Type == session.TypeCompaction {
		return nil
	}

	projected := session.BuildProjection(branch)

	// The newest compaction projects first (it leads the projection); older
	// compactions retained inside its window contribute no messages, so the
	// first compaction with messages is the boundary to start after.
	prevCompactionIndex := -1
	for i, pe := range projected {
		if pe.Source.Type == session.TypeCompaction && len(pe.Messages) > 0 {
			prevCompactionIndex = i
			break
		}
	}
	var previousSummary string
	boundaryStart := 0
	if prevCompactionIndex >= 0 {
		previousSummary = projected[prevCompactionIndex].Source.ExtraString("summary")
		// The canonical projection already selected the previous
		// compaction's retained tail; everything before it is summarized
		// away and must not be summarized again.
		boundaryStart = prevCompactionIndex + 1
	}

	tokensBefore := estimateProjectedContextTokens(projected, branch)

	// The cut point runs over the projected window's source entries, in
	// projection index space, so boundaryStart maps directly.
	sources := make([]*session.SessionEntry, len(projected))
	for i := range projected {
		sources[i] = projected[i].Source
	}
	cut := FindCutPoint(sources, boundaryStart, len(sources), s.KeepRecentTokens)
	if cut.FirstKeptEntryIndex >= len(sources) {
		return nil // no entries at all in the window
	}
	firstKept := sources[cut.FirstKeptEntryIndex]
	if firstKept.ID == "" {
		return nil
	}

	historyEnd := cut.FirstKeptEntryIndex
	if cut.IsSplitTurn {
		historyEnd = cut.TurnStartIndex
	}
	messagesToSummarize := collectMessages(projected[boundaryStart:historyEnd])
	var turnPrefixMessages []ai.Message
	if cut.IsSplitTurn {
		turnPrefixMessages = collectMessages(projected[cut.TurnStartIndex:cut.FirstKeptEntryIndex])
	}
	if len(messagesToSummarize) == 0 && len(turnPrefixMessages) == 0 {
		return nil
	}

	fileOps := CreateFileOps()
	// The previous checkpoint's file inventory carries forward, unless it
	// came from a hook (fromHook summaries have no details to carry).
	if prevCompactionIndex >= 0 {
		prev := projected[prevCompactionIndex].Source
		if !entryFromHook(prev) {
			if details := decodeCompactionDetails(prev); details != nil {
				for _, f := range details.ReadFiles {
					fileOps.Read[f] = true
				}
				for _, f := range details.ModifiedFiles {
					fileOps.Edited[f] = true
				}
			}
		}
	}
	for _, m := range messagesToSummarize {
		ExtractFileOpsFromMessage(m, fileOps)
	}
	for _, m := range turnPrefixMessages {
		ExtractFileOpsFromMessage(m, fileOps)
	}

	return &Preparation{
		FirstKeptEntryID:    firstKept.ID,
		MessagesToSummarize: messagesToSummarize,
		TurnPrefixMessages:  turnPrefixMessages,
		IsSplitTurn:         cut.IsSplitTurn,
		TokensBefore:        tokensBefore,
		PreviousSummary:     previousSummary,
		FileOps:             fileOps,
		Settings:            s,
	}
}

// Run executes a prepared compaction and returns the result to append.
//
// A split turn produces two summaries: the history (updated from the
// previous checkpoint when there is one) and the turn prefix, merged with
// pi's separator. Any failure aborts the whole compaction — the caller must
// not write a partial checkpoint.
//
// pi: compact (compaction.ts:965-1071).
func Run(ctx context.Context, prep *Preparation, opts RunOptions) (*Result, error) {
	var (
		summary string
		usage   ai.Usage
	)

	if prep.IsSplitTurn && len(prep.TurnPrefixMessages) > 0 {
		historyText := prep.PreviousSummary
		if historyText == "" {
			historyText = noPriorHistory
		}
		var historyUsage *ai.Usage
		if len(prep.MessagesToSummarize) > 0 {
			history, err := Summarize(ctx, prep.MessagesToSummarize, prep.PreviousSummary, opts.CustomInstructions, prep.Settings.ReserveTokens, opts.SummaryOptions)
			if err != nil {
				return nil, err
			}
			historyText = history.Text
			historyUsage = &history.Usage
		}
		turnPrefix, err := SummarizeTurnPrefix(ctx, prep.TurnPrefixMessages, prep.Settings.ReserveTokens, opts.SummaryOptions)
		if err != nil {
			return nil, err
		}
		summary = historyText + splitTurnSeparator + turnPrefix.Text
		if historyUsage != nil {
			usage = CombineUsage(*historyUsage, turnPrefix.Usage)
		} else {
			usage = turnPrefix.Usage
		}
	} else {
		history, err := Summarize(ctx, prep.MessagesToSummarize, prep.PreviousSummary, opts.CustomInstructions, prep.Settings.ReserveTokens, opts.SummaryOptions)
		if err != nil {
			return nil, err
		}
		summary = history.Text
		usage = history.Usage
	}

	readFiles, modifiedFiles := ComputeFileLists(prep.FileOps)
	summary += FormatFileOperations(readFiles, modifiedFiles)

	if prep.FirstKeptEntryID == "" {
		return nil, errors.New("compact: first kept entry has no id - session may need migration")
	}

	return &Result{
		Summary:          summary,
		FirstKeptEntryID: prep.FirstKeptEntryID,
		TokensBefore:     prep.TokensBefore,
		Usage:            usage,
		Details:          session.CompactionDetails{ReadFiles: readFiles, ModifiedFiles: modifiedFiles},
	}, nil
}

// collectMessages gathers the messages a set of projected entries
// contributes to a summary.
//
// pi: getMessagesFromProjectedEntryForCompaction (compaction.ts:98-102). The
// compaction entry itself is excluded (its checkpoint is the previous
// summary, already carried by Preparation.PreviousSummary) and so are system
// messages — prompt state, not conversation history.
func collectMessages(entries []session.ProjectedEntry) []ai.Message {
	var out []ai.Message
	for _, pe := range entries {
		if pe.Source.Type == session.TypeCompaction {
			continue
		}
		for _, m := range pe.Messages {
			if m.Role == ai.RoleSystem {
				continue
			}
			out = append(out, m)
		}
	}
	return out
}

// ProjectedTokens estimates the context size of a branch's compaction-aware
// projection — the number the auto-compaction trigger compares against the
// model's window.
//
// pi: _exceedsCompactionThreshold (agent-session.ts:766-774), which feeds
// estimateProjectedContextTokens into shouldCompact. Exported because the
// trigger lives above this package (cmd/ultrac's PrepareRequest hook) while
// the usage-invalidation rule below must stay in one place.
func ProjectedTokens(branch []*session.SessionEntry) int {
	return estimateProjectedContextTokens(session.BuildProjection(branch), branch)
}

// estimateProjectedContextTokens estimates the planning-time context size.
//
// pi: estimateProjectedContextTokens (compaction.ts:226-262). The plain
// estimate trusts the newest assistant usage, but a usage captured before
// the newest compaction describes a context that no longer exists — the
// compaction replaced it. In that case the projection is estimated from
// scratch instead.
func estimateProjectedContextTokens(projected []session.ProjectedEntry, branch []*session.SessionEntry) int {
	messages := session.ProjectedMessages(projected)
	estimate := ai.EstimateContextTokensDetailed(messages)
	if estimate.LastUsageIndex < 0 {
		// No usage anywhere: the plain estimate is already from scratch.
		return estimate.Tokens
	}

	// Map the anchoring message back to the entry that produced it.
	var usageEntryID string
	seen := 0
	for _, pe := range projected {
		if estimate.LastUsageIndex < seen+len(pe.Messages) {
			usageEntryID = pe.Source.ID
			break
		}
		seen += len(pe.Messages)
	}
	usageEntryIndex := -1
	if usageEntryID != "" {
		for i, e := range branch {
			if e.ID == usageEntryID {
				usageEntryIndex = i
				break
			}
		}
	}
	latestInvalidating := -1
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == session.TypeCompaction {
			latestInvalidating = i
			break
		}
	}
	if usageEntryIndex > latestInvalidating {
		return estimate.Tokens
	}

	tokens := 0
	if sys, ok := ai.GetCurrentSystemMessage(messages); ok {
		tokens += ai.EstimateMessageTokens(sys)
	}
	for _, m := range messages {
		if m.Role != ai.RoleSystem {
			tokens += ai.EstimateMessageTokens(m)
		}
	}
	return tokens
}

// entryFromHook reads the fromHook flag pi writes on hook-originated
// compactions (absent here — this version has no compaction hooks).
func entryFromHook(e *session.SessionEntry) bool {
	raw, ok := e.Extra["fromHook"]
	if !ok {
		return false
	}
	var fromHook bool
	return json.Unmarshal(raw, &fromHook) == nil && fromHook
}

// decodeCompactionDetails reads a compaction entry's details, nil when
// absent or unreadable.
func decodeCompactionDetails(e *session.SessionEntry) *session.CompactionDetails {
	raw, ok := e.Extra["details"]
	if !ok {
		return nil
	}
	var details session.CompactionDetails
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil
	}
	return &details
}
