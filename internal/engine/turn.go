package engine

import (
	"context"
	"encoding/json"
	"strings"

	"ultrac/internal/contract/event"
	"ultrac/internal/contract/provider"
)

// close publishes the summary. It is idempotent so both the normal exit and a
// panic-path defer can call it.
func (t *tracker) finish() { t.closeOnce.Do(func() { close(t.done) }) }

// emit forwards one event and records it.
//
// The bool return is the consumer's decision to keep going. Its value is
// inverted at every call site on purpose: `if !yield(...) { return }` reads as
// "the consumer said stop", which is what it means, rather than as an error
// check that a reader would be tempted to skip.
func (t *tracker) emit(yield func(event.Event, error) bool, ev event.Event) bool {
	t.add(ev)
	return yield(ev, nil)
}

// fail emits a terminal failure.
//
// It always sets Fatal, because every caller of it is ending the run: a
// non-fatal failure would be a notice.
func fail(yield func(event.Event, error) bool, t *tracker, code, message string) {
	t.emit(yield, event.FailureEvent(event.Failure{
		Code:    code,
		Message: message,
		Fatal:   true,
	}))
}

// turnOutput is everything one assistant response produced.
type turnOutput struct {
	// blocks reconstructs the assistant message for the next request. It is
	// what makes the conversation coherent on turn two: the model must see the
	// tool_use blocks it emitted, or the tool_result blocks answer nothing.
	blocks []provider.Block
	// toolCalls are the complete calls, in emission order.
	toolCalls []provider.ToolCall
	stop      provider.StopReason
	usage     provider.Usage
	// sawThinking records that reasoning deltas arrived. They are streamed to
	// the frontend but not replayed; see the note in buildBlocks.
	sawThinking bool
}

// streamTurn runs one provider request and forwards its chunks as events.
//
// stopped reports that the consumer ended the iteration. It is separate from
// err because it is not a failure — the caller asked to stop.
func (e *Engine) streamTurn(
	ctx context.Context,
	req provider.Request,
	yield func(event.Event, error) bool,
	t *tracker,
) (*turnOutput, bool, error) {
	out := &turnOutput{}

	var text strings.Builder
	var calls []provider.ToolCall

	for chunk, err := range e.opts.Provider.Stream(ctx, req) {
		if err != nil {
			return out, false, err
		}
		switch chunk.Kind {
		case provider.ChunkText:
			if chunk.Text == "" {
				continue
			}
			text.WriteString(chunk.Text)
			if !t.emit(yield, event.Text(chunk.Text)) {
				return out, true, nil
			}

		case provider.ChunkThinking:
			if chunk.Text == "" {
				continue
			}
			out.sawThinking = true
			if !t.emit(yield, event.Thinking(chunk.Text)) {
				return out, true, nil
			}

		case provider.ChunkToolCall:
			if chunk.ToolCall == nil {
				continue
			}
			call := *chunk.ToolCall
			out.toolCalls = append(out.toolCalls, call)
			calls = append(calls, call)

			// Announce before running. The frontend shows this immediately, and
			// a tool that takes ten seconds otherwise looks like a hang.
			if !t.emit(yield, event.ToolCall(event.ToolUse{
				ID:    call.ID,
				Name:  call.Name,
				Input: call.Input,
			})) {
				return out, true, nil
			}

		case provider.ChunkUsage:
			if chunk.Usage == nil {
				continue
			}
			out.usage = *chunk.Usage
			if !t.emit(yield, event.UsageEvent(*chunk.Usage)) {
				return out, true, nil
			}

		case provider.ChunkStop:
			out.stop = chunk.Stop
		}
	}

	out.blocks = buildBlocks(text.String(), calls)
	return out, false, nil
}

// buildBlocks reconstructs the assistant turn.
//
// Order matters: thinking, then text, then tool_use. That is the order the
// response is generated in, and the API rejects a message whose blocks are out
// of sequence.
//
// Thinking blocks are NOT reconstructed. Replaying one requires its signature,
// which the provider accumulates but does not surface, and a thinking block
// without its signature makes the API reject the whole request. Dropping it is
// the safe failure: the conversation continues, and the model's own reasoning
// from the previous turn is not something it needs to re-read. Streaming
// thinking to the frontend still works — it is only the replay that is skipped.
func buildBlocks(text string, calls []provider.ToolCall) []provider.Block {
	blocks := make([]provider.Block, 0, 1+len(calls))
	if text != "" {
		blocks = append(blocks, provider.Block{Kind: provider.BlockText, Text: text})
	}
	for _, call := range calls {
		input := call.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, provider.Block{
			Kind:      provider.BlockToolUse,
			ToolUseID: call.ID,
			ToolName:  call.Name,
			Input:     input,
		})
	}
	return blocks
}

// toolResultMessage wraps tool results in the user message that answers the
// assistant's tool_use blocks.
//
// Every tool_use emitted in the previous assistant message must have exactly
// one tool_result here. A missing one makes the API reject the request, which
// is why runTools is careful to produce a result even for calls it refused.
func toolResultMessage(results []event.ToolResult) provider.Message {
	blocks := make([]provider.Block, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, provider.Block{
			Kind:      provider.BlockToolResult,
			ToolUseID: r.ToolUseID,
			Text:      r.Content,
			IsError:   r.IsError,
		})
	}
	return provider.Message{Role: provider.RoleUser, Blocks: blocks}
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	// The provider layer already distinguishes a truncated stream from a
	// transport failure; preserving the distinction matters because one is
	// worth retrying by the user and the other is worth retrying by the client.
	msg := err.Error()
	if strings.Contains(msg, "truncated") {
		return event.CodeTruncated
	}
	return event.CodeProviderError
}
