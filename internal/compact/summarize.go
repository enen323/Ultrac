package compact

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// The one-shot summarization call.
//
// pi splits this across completeSummarization / generateSummaryWithUsage /
// generateTurnPrefixSummary (compaction.ts:595-766, 1076-1119); the Go port
// keeps the same three layers behind two entry points, because the only
// difference between the main and turn-prefix calls is the prompt and the
// maxTokens factor.
//
// The call deliberately bypasses the agent loop: no tools, no steering, one
// request, one assistant message. pi's retryAssistantCall wrapper is not
// ported — Ultrac's provider contract owns client-side retries through
// StreamOptions.MaxRetries, and wrapping again would double-count them.

// SummaryOptions is everything the one-off call needs besides the messages
// and the prompt.
type SummaryOptions struct {
	// Model is the model the summary is requested from. Callers pass the
	// session's current model (P-C2), as pi does.
	Model ai.Model
	// APIKey is the resolved credential for this request.
	APIKey string
	// Reasoning is the requested thinking level, applied only when the
	// model supports reasoning (pi's createSummarizationOptions,
	// compaction.ts:595-610).
	Reasoning ai.ThinkingLevel
	// SessionID is the routing id forwarded to the provider. A fresh
	// session id is generated when empty (pi: options.sessionId ?? uuidv7()).
	SessionID string
	// StreamFn overrides the provider registry — tests and custom routing.
	// nil means ai.StreamRequest.
	StreamFn ai.StreamFn
	// MaxRetries caps provider client-side retries for this request.
	MaxRetries int
}

// SummaryResult is one generated checkpoint.
type SummaryResult struct {
	Text  string
	Usage ai.Usage
}

// Summarize generates a fresh checkpoint, or updates the previous one when
// previousSummary is non-empty.
//
// pi: generateSummaryWithUsage (compaction.ts:696-766). The conversation is
// serialized to text and wrapped in <conversation> tags so the model cannot
// treat it as a conversation to continue; the previous checkpoint rides in
// <previous-summary> tags when present.
func Summarize(ctx context.Context, messages []ai.Message, previousSummary, customInstructions string, reserveTokens int, opts SummaryOptions) (SummaryResult, error) {
	basePrompt := SummarizationPrompt
	if previousSummary != "" {
		basePrompt = UpdateSummarizationPrompt
	}
	if customInstructions != "" {
		basePrompt += "\n\nAdditional focus: " + customInstructions
	}

	var b strings.Builder
	b.WriteString("<conversation>\n")
	b.WriteString(SerializeConversation(messages))
	b.WriteString("\n</conversation>\n\n")
	if previousSummary != "" {
		b.WriteString("<previous-summary>\n")
		b.WriteString(previousSummary)
		b.WriteString("\n</previous-summary>\n\n")
	}
	b.WriteString(basePrompt)

	return summarize(ctx, b.String(), summaryMaxTokens(reserveTokens, opts.Model, 0.8), "Summarization", opts)
}

// SummarizeTurnPrefix generates the checkpoint for the prefix of a turn that
// was cut in half.
//
// pi: generateTurnPrefixSummary (compaction.ts:1076-1119). No previous
// summary, no custom instructions, and a smaller budget (half the reserve)
// because its output is only the first half of a merged summary.
func SummarizeTurnPrefix(ctx context.Context, messages []ai.Message, reserveTokens int, opts SummaryOptions) (SummaryResult, error) {
	promptText := "# Conversation\n" + SerializeConversation(messages) + "\n\n# Instructions\n" + TurnPrefixSummarizationPrompt
	return summarize(ctx, promptText, summaryMaxTokens(reserveTokens, opts.Model, 0.5), "Turn prefix summarization", opts)
}

// summarize runs the shared request: build the transcript context, stream
// once, drain, and validate the result.
//
// pi: completeSummarization + buildSummarizationContext
// (compaction.ts:619-639, 682-693). cacheRetention is "none": a one-off
// summary must not write a prompt-cache entry (compaction.ts:627-633).
func summarize(ctx context.Context, promptText string, maxTokens int, label string, opts SummaryOptions) (SummaryResult, error) {
	streamFn := opts.StreamFn
	if streamFn == nil {
		streamFn = defaultStreamFn
	}
	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = session.NewSessionID()
	}

	prompt := ai.NewTextMessage(ai.RoleUser, promptText)
	prompt.TimestampMS = time.Now().UnixMilli() // pi: timestamp: Date.now()
	tc := ai.NormalizeContext(ai.Context{
		SystemPrompt: SummarizationSystemPrompt,
		Messages:     []ai.Message{prompt},
	})

	streamOpts := ai.StreamOptions{
		MaxTokens:      maxTokens,
		APIKey:         opts.APIKey,
		CacheRetention: ai.CacheRetentionNone,
		SessionID:      sessionID,
		MaxRetries:     opts.MaxRetries,
	}
	if opts.Model.Reasoning && opts.Reasoning != "" {
		streamOpts.Reasoning = opts.Reasoning
	}

	stream := streamFn(ctx, opts.Model, tc, streamOpts)

	// Drain the events before Result: every consumer in the repo does, and a
	// producer that never finishes pushing must not be left blocked.
	for _, err := range stream.Events() {
		if err != nil {
			return SummaryResult{}, fmt.Errorf("compact: %s stream: %w", label, err)
		}
	}
	msg, err := stream.Result()
	if err != nil {
		return SummaryResult{}, fmt.Errorf("compact: %s: %w", label, err)
	}

	if failure := summarizationFailure(msg, label); failure != "" {
		return SummaryResult{}, errors.New(failure)
	}
	for _, block := range msg.Content {
		if block.Type == ai.BlockToolCall {
			return SummaryResult{}, errors.New(label + " attempted to call a tool")
		}
	}

	usage := ai.Usage{}
	if msg.Usage != nil {
		usage = *msg.Usage
	}
	return SummaryResult{Text: ai.MessageText(msg), Usage: usage}, nil
}

// defaultStreamFn resolves the provider through the registry, the same way
// the agent loop's fallback does (agent.go:253-273). A lookup failure is
// encoded in-stream, never returned: the StreamFn contract forbids both a
// nil stream and a panic, and an unregistered API is exactly that kind of
// configuration error.
func defaultStreamFn(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts ai.StreamOptions) ai.Stream {
	stream, err := ai.StreamRequest(ctx, model, tc, opts)
	if err != nil {
		s := ai.NewEventStream()
		msg := ai.Message{
			Role:         ai.RoleAssistant,
			API:          string(model.API),
			Provider:     string(model.Provider),
			Model:        model.ID,
			StopReason:   ai.StopError,
			ErrorMessage: err.Error(),
			TimestampMS:  time.Now().UnixMilli(),
		}
		s.Push(ai.EvError{Reason: ai.StopError, Error: msg})
		s.End()
		return s
	}
	return stream
}

// summarizationFailure reports why a response must not become a checkpoint,
// or "" when it may.
//
// pi: getSummarizationFailure (compaction.ts:585-593). A length stop contains
// partial text and must not be persisted; an error stop carries the
// provider's message. The aborted case is Ultrac's addition: pi would fall
// through and persist an empty summary, but a cancelled summarization means
// the caller asked to stop, and a checkpoint written from it is fiction.
func summarizationFailure(msg ai.Message, label string) string {
	switch msg.StopReason {
	case ai.StopError:
		message := msg.ErrorMessage
		if message == "" {
			message = "Unknown error"
		}
		return fmt.Sprintf("%s failed: %s", label, message)
	case ai.StopLength:
		return label + " failed: generation hit the token cap and the summary is incomplete"
	case ai.StopAborted:
		return label + " aborted"
	}
	return ""
}

// summaryMaxTokens applies pi's budget rule: min(floor(factor × reserve),
// model.maxTokens) when the model records a cap.
//
// pi: compaction.ts:712-715 and :1090-1093.
func summaryMaxTokens(reserveTokens int, model ai.Model, factor float64) int {
	maxTokens := int(math.Floor(factor * float64(reserveTokens)))
	if model.MaxTokens > 0 && model.MaxTokens < maxTokens {
		maxTokens = model.MaxTokens
	}
	return maxTokens
}

// CombineUsage sums two usages, keeping the optional splits when either side
// reports them.
//
// pi: combineUsage (usage-totals.ts:32-52). Used to merge the history and
// turn-prefix summaries of a split-turn compaction into one recorded usage.
func CombineUsage(first, second ai.Usage) ai.Usage {
	out := ai.Usage{
		Input:       first.Input + second.Input,
		Output:      first.Output + second.Output,
		CacheRead:   first.CacheRead + second.CacheRead,
		CacheWrite:  first.CacheWrite + second.CacheWrite,
		TotalTokens: first.TotalTokens + second.TotalTokens,
		Cost: ai.Cost{
			Input:      first.Cost.Input + second.Cost.Input,
			Output:     first.Cost.Output + second.Cost.Output,
			CacheRead:  first.Cost.CacheRead + second.Cost.CacheRead,
			CacheWrite: first.Cost.CacheWrite + second.Cost.CacheWrite,
			Total:      first.Cost.Total + second.Cost.Total,
		},
	}
	if first.CacheWrite1h != nil || second.CacheWrite1h != nil {
		sum := ptrInt(first.CacheWrite1h) + ptrInt(second.CacheWrite1h)
		out.CacheWrite1h = &sum
	}
	if first.Reasoning != nil || second.Reasoning != nil {
		sum := ptrInt(first.Reasoning) + ptrInt(second.Reasoning)
		out.Reasoning = &sum
	}
	return out
}

func ptrInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
