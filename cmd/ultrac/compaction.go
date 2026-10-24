// compaction.go — the compaction orchestration: pi's agent-session compaction
// half (agent-session.ts:2789-2934 manual, :3120-3270 automatic) wired to the
// Go harness (docs/compaction-branch-plan.md P-C2).
//
// # The shape
//
// The engine (internal/compact) plans and summarizes; the session
// (internal/session) persists the checkpoint; this file is the layer that
// decides WHEN, persists THROUGH the session, keeps the agent's state in
// sync, and puts the two events on the wire. It is installed as the agent's
// PrepareRequest hook (assemble.go), which is where pi hangs its
// _compactBeforeNextAssistantResponse off.
//
// # Why the session is the authority
//
// The agent loop reads its context from agent state.messages, while a
// compaction entry can only land in the session. Compacting only the session
// would leave the next Prompt replaying the uncompressed history, so every
// successful compaction does both: AppendCompaction (the durable record) and
// ReplaceMessages (the live state), and the hook returns the compressed
// projection as the request context for the turn in progress (plan D2).
//
// # What is deliberately not here
//
//   - Overflow-triggered compaction: Ultrac's provider contract encodes
//     failures as in-stream EvError, and recognizing "context overflow"
//     would mean parsing provider error bodies (plan D1). The reason enum
//     keeps the value; nothing produces it.
//   - Retry: pi's retryAssistantCall wrapper. Ultrac's providers own
//     client-side retries through StreamOptions.MaxRetries (compact's doc
//     explains the double-count risk).
//   - per-model overrides: the settings section carries three global keys
//     only (plan D6/R6).
//
// # A known coupling
//
// With --no-transcript nothing is persisted, the session branch stays
// empty, and there is nothing to plan a compaction over — the same coupling
// pi has (a compaction entry IS a session entry). The hook then simply
// never fires; the request goes out uncompressed.
package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/compact"
	"ultrac/internal/config"
	"ultrac/internal/rpc"
	"ultrac/internal/session"
)

// The abort-and-wait a manual compaction starts with is bounded by the same
// settleWait the RPC layer's session swaps use: a wedged run must not wedge
// the compaction behind it.

// compactor drives compaction for one agent/session pair. A new pair (RPC
// new_session / resume_session) gets a new compactor; the hook closure
// captures the one built for it.
type compactor struct {
	mu sync.Mutex

	agent    *agent.Agent
	mgr      *session.SessionManager
	settings compact.Settings

	// credential resolves the API key for the summary request. It is the
	// harness's per-provider resolver (assemble.go), called with the
	// CURRENT model's provider at execution time rather than captured as
	// a string: a set_model or a resume can change the model under a
	// live compactor, and the summary must authenticate as that model's
	// provider (P3).
	credential func(provider string) string
	streamFn   agent.StreamFn // nil means the provider registry

	// emit puts compaction events on the wire (the mode's PiJSONWriter).
	// Nil means "no sink" — text-mode print drops them, which is pi's
	// behaviour for a mode that carries no event stream.
	emit func(agent.AgentEvent) error

	// compacting mirrors pi's isCompacting: a compaction (automatic or
	// manual) is running right now.
	compacting bool
}

// newCompactor builds the executor for one agent/session pair. The agent is
// attached after construction by newAgent, because the hook the agent needs
// at construction time is a method on this struct.
func newCompactor(mgr *session.SessionManager, settings compact.Settings, credential func(provider string) string, streamFn agent.StreamFn, emit func(agent.AgentEvent) error) *compactor {
	return &compactor{
		mgr:        mgr,
		settings:   settings,
		credential: credential,
		streamFn:   streamFn,
		emit:       emit,
	}
}

// resolveCompactionSettings maps the typed settings section onto the
// engine's shape, injecting pi's defaults for absent keys (plan D6: the
// defaults live on the read side, never in a file).
//
// A malformed value cannot reach here as a typed field — the config layer
// parks non-decodable sections in Extra and validate() rejects negative
// budgets — but buildHarness resolves from raw layers rather than through
// Get(), so the guard is repeated here as the runtime's last word: a
// negative budget would invert the trigger line, and "compaction never
// fires" with nothing pointing at the file is the worst way to learn that.
func resolveCompactionSettings(c *config.CompactionSettings, stderr io.Writer) compact.Settings {
	out := compact.DefaultSettings()
	if c == nil {
		return out
	}
	if c.Enabled != nil {
		out.Enabled = *c.Enabled
	}
	if c.ReserveTokens != nil {
		if *c.ReserveTokens < 0 {
			fmt.Fprintf(stderr, "warning: compaction.reserveTokens must be >= 0; using %d\n", out.ReserveTokens)
		} else {
			out.ReserveTokens = *c.ReserveTokens
		}
	}
	if c.KeepRecentTokens != nil {
		if *c.KeepRecentTokens < 0 {
			fmt.Fprintf(stderr, "warning: compaction.keepRecentTokens must be >= 0; using %d\n", out.KeepRecentTokens)
		} else {
			out.KeepRecentTokens = *c.KeepRecentTokens
		}
	}
	return out
}

// --- the automatic trigger ---------------------------------------------------

// prepareRequest is the PrepareRequest hook: before every conversational
// provider request, compact when the session's projection has crossed the
// threshold.
//
// pi: _installAgentRequestProjection + _compactBeforeNextAssistantResponse
// (agent-session.ts:787-785). One deliberate difference: pi replaces the
// request context with the projection on EVERY request; this port replaces
// only when a compaction happened (plan D2 — the narrower move keeps
// contextSnapshotLocked's semantics untouched, and the two views cannot
// drift because every message the loop emits is persisted before the next
// hook runs).
func (c *compactor) prepareRequest(ctx context.Context, req agent.PrepareRequestContext) agent.AgentRequestUpdate {
	if !c.settings.Enabled {
		return agent.AgentRequestUpdate{}
	}
	// A model with no recorded window never triggers — ShouldCompact
	// carries the same guard (pi: _exceedsCompactionThreshold).
	branch := c.mgr.GetBranch("")
	if !compact.ShouldCompact(compact.ProjectedTokens(branch), req.Model.ContextWindow, c.settings) {
		return agent.AgentRequestUpdate{}
	}

	prep := compact.Prepare(branch, c.settings)
	if prep == nil {
		// Nothing to do: the branch is empty, already ends in a
		// compaction, or fits inside the keep-recent window. pi's auto
		// path is silent here — no events, no error (agent-session.ts:
		// 3137-3139).
		return agent.AgentRequestUpdate{}
	}

	c.setCompacting(true)
	defer c.setCompacting(false)
	c.emitEvent(agent.EvCompactionStart{Reason: agent.CompactionReasonThreshold})

	result, err := c.execute(ctx, prep, "")
	if err != nil {
		c.emitCompactionEnd(ctx, agent.CompactionReasonThreshold, nil, err)
		// The session is untouched and the request goes out uncompressed:
		// a failed checkpoint must never take the turn down with it.
		return agent.AgentRequestUpdate{}
	}

	messages := c.projectedMessages()
	c.agent.ReplaceMessages(messages)
	c.emitCompactionEnd(ctx, agent.CompactionReasonThreshold, result, nil)
	return agent.AgentRequestUpdate{Context: &agent.AgentContext{Messages: messages, Tools: req.Context.Tools}}
}

// --- the manual trigger ------------------------------------------------------

// Compact is the manual path: abort the active run first (pi: compact()
// awaits this.abort(), agent-session.ts:2790), then the same plan → run →
// append → swap sequence. ctx is the host's run context, so a termination
// signal cancels an in-flight summarization instead of delaying exit.
//
// The returned error is pi's verbatim: "Already compacted" when the branch
// already ends in a checkpoint, "Nothing to compact (session too small)"
// when there is not enough conversation to summarize, or the summarization
// failure. None of them write anything.
func (c *compactor) Compact(ctx context.Context, customInstructions string) (*rpc.CompactResult, error) {
	a := c.agent
	a.Abort()
	waitCtx, cancel := context.WithTimeout(context.Background(), settleWait)
	defer cancel()
	_ = a.WaitForIdle(waitCtx)

	c.setCompacting(true)
	defer c.setCompacting(false)
	c.emitEvent(agent.EvCompactionStart{Reason: agent.CompactionReasonManual})

	branch := c.mgr.GetBranch("")
	prep := compact.Prepare(branch, c.settings)
	if prep == nil {
		err := nothingToCompactError(branch)
		c.emitCompactionEnd(ctx, agent.CompactionReasonManual, nil, err)
		return nil, err
	}

	result, err := c.execute(ctx, prep, customInstructions)
	if err != nil {
		c.emitCompactionEnd(ctx, agent.CompactionReasonManual, nil, err)
		return nil, err
	}

	messages := c.projectedMessages()
	a.ReplaceMessages(messages)
	c.emitCompactionEnd(ctx, agent.CompactionReasonManual, result, nil)
	return result, nil
}

// nothingToCompactError names WHY a preparation came back nil, the way pi's
// compact() does before throwing (agent-session.ts:2806-2813). The strings
// are wire literals: the RPC error response carries them verbatim, so a
// client can tell the idempotence cases apart from a summarization failure.
// ST1005 disagrees; protocol fidelity wins (same call SetModel makes).
func nothingToCompactError(branch []*session.SessionEntry) error {
	if len(branch) > 0 && branch[len(branch)-1].Type == session.TypeCompaction {
		//lint:ignore ST1005 wire literal matches pi
		return fmt.Errorf("Already compacted")
	}
	//lint:ignore ST1005 wire literal matches pi
	return fmt.Errorf("Nothing to compact (session too small)")
}

// --- shared execution ---------------------------------------------------------

// execute runs a prepared compaction and appends the checkpoint. It returns
// the wire result (compact.Result plus the post-append estimate) on success;
// every failure leaves the session untouched.
func (c *compactor) execute(ctx context.Context, prep *compact.Preparation, customInstructions string) (*rpc.CompactResult, error) {
	state := c.agent.State()
	opts := compact.RunOptions{
		SummaryOptions: compact.SummaryOptions{
			Model: state.Model,
			// The summary request authenticates as the CURRENT model's
			// provider, resolved per execution (P3) — never a key
			// captured when the compactor was built.
			APIKey:    c.credential(string(state.Model.Provider)),
			Reasoning: ai.ThinkingLevel(state.ThinkingLevel),
			SessionID: c.mgr.SessionID(),
			StreamFn:  c.streamFn,
		},
		CustomInstructions: customInstructions,
	}
	result, err := compact.Run(ctx, prep, opts)
	if err != nil {
		return nil, err
	}

	// The boundary snapshot is the complete system-prompt state at
	// compression time, replayed from the live transcript (pi computes it
	// inside appendCompaction from its own projection; the caller passes
	// it here so the entry never sees itself).
	var snapshot *ai.Message
	if sys, ok := ai.GetCurrentSystemMessage(state.Messages); ok {
		snapshot = &sys
	}
	if _, err := c.mgr.AppendCompaction(
		result.Summary, result.FirstKeptEntryID, result.TokensBefore,
		&result.Details, snapshot, &result.Usage,
	); err != nil {
		return nil, fmt.Errorf("compact: append checkpoint: %w", err)
	}

	return &rpc.CompactResult{
		Summary:              result.Summary,
		FirstKeptEntryID:     result.FirstKeptEntryID,
		TokensBefore:         result.TokensBefore,
		EstimatedTokensAfter: ai.EstimateContextTokens(c.projectedMessages()),
		Usage:                result.Usage,
		Details:              result.Details,
	}, nil
}

// projectedMessages is the compaction-aware projection of the current
// branch — the compressed transcript both the agent state and the request
// context are replaced with.
func (c *compactor) projectedMessages() []ai.Message {
	return session.ProjectedMessages(session.BuildProjection(c.mgr.GetBranch("")))
}

// --- events and state ---------------------------------------------------------

// emitCompactionEnd emits the closing event with pi's error shaping: an
// aborted compaction carries no errorMessage, a failed one carries the
// reason-prefixed message ("Auto-compaction failed: …" on the automatic
// path, "Compaction failed: …" on the manual one — agent-session.ts:2913,
// :3241-3245).
func (c *compactor) emitCompactionEnd(ctx context.Context, reason string, result *rpc.CompactResult, err error) {
	end := agent.EvCompactionEnd{Reason: reason, WillRetry: false}
	if err == nil {
		end.Result = result
		c.emitEvent(end)
		return
	}
	// A cancelled context means the caller asked to stop — pi reads its
	// AbortController the same way, and drops the message (plan R8).
	end.Aborted = ctx.Err() != nil
	if !end.Aborted {
		prefix := "Compaction failed: "
		if reason == agent.CompactionReasonThreshold {
			prefix = "Auto-compaction failed: "
		}
		end.ErrorMessage = prefix + err.Error()
	}
	c.emitEvent(end)
}

func (c *compactor) emitEvent(ev agent.AgentEvent) {
	if c.emit == nil {
		return
	}
	_ = c.emit(ev)
}

func (c *compactor) setCompacting(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.compacting = v
}

// IsCompacting reports whether a compaction is running right now (pi:
// agent-session.ts:1627-1633).
func (c *compactor) IsCompacting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.compacting
}

// AutoEnabled reports the auto-compaction gate.
func (c *compactor) AutoEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.Enabled
}

// SetEnabled flips the auto-compaction gate on the live executor. The
// persistence is the caller's (rpcHost.SetAutoCompaction writes the user
// settings layer and updates the harness's resolved copy); this keeps the
// executor free of the config layer.
func (c *compactor) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings.Enabled = enabled
}
