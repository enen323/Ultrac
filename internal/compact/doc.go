// Package compact is the context-compaction engine: the pure planning and
// summarization half of pi's packages/coding-agent/src/core/compaction,
// ported for Ultrac's Go harness (docs/compaction-branch-plan.md P-C1).
//
// # The split with the session package
//
// session owns the transcript (append-only tree + compaction-aware read
// projection) and exposes the projection this package plans over
// (session.BuildProjection). This package owns everything that has no disk in
// it: the settings, the cut-point algorithm, the conversation serialization,
// the one-shot summarization LLM call, and the Prepare/Run orchestration that
// turns a branch into a Result. The caller (cmd/ultrac in P-C2) writes the
// Result through session.AppendCompaction and swaps the agent's context.
//
// # What is deliberately not here (yet)
//
//   - Automatic triggering, the PrepareRequest hook and the RPC commands are
//     P-C2; this package is inert until a caller drives it.
//   - The retryAssistantCall retry policy: Ultrac's provider contract
//     (StreamFn) already owns client-side retries through StreamOptions.
//     MaxRetries, so pi's wrapper would double-count them.
//   - Overflow-triggered compaction (provider-reported context overflow): the
//     plan's D1 keeps the reason enum's "overflow" value reserved but
//     unproduced in this version.
//   - Branch summaries (branchWithSummary / BRANCH_SUMMARY_PROMPT): P-C3.
package compact
