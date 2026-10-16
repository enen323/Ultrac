package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"ultrac/internal/ai"
)

// streamEvent is one decoded SSE payload.
//
// The Type tag discriminates. Every field is optional because the API omits
// whatever does not apply to an event: a text delta carries no index, a block
// delta carries no id.
type streamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string     `json:"id"`
		Model string     `json:"model"`
		Role  string     `json:"role"`
		Usage *wireUsage `json:"usage"`
	} `json:"message"`

	Index        int `json:"index"`
	ContentBlock *struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		Data      string          `json:"data"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content_block"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	Usage *wireUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// wireUsage is the API's token accounting. Field names are the API's.
//
// Every field is a pointer because the API distinguishes "absent" from zero:
// message_delta routinely omits the input counts message_start already
// reported, and overwriting them with zero would destroy real accounting
// (pi: anthropic-messages.ts:826-853 checks each field against null).
type wireUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`

	// cache_creation carries the 1h TTL breakdown; the flat count above
	// includes it, so the two must not be double-billed.
	CacheCreation *struct {
		Ephemeral1hInputTokens *int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`

	OutputTokensDetails *struct {
		ThinkingTokens *int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// blockState accumulates one content block.
//
// The accumulated tool input is a raw JSON string, not a decoded map.
// Decoding partial JSON on every delta is O(n²) over the tool arguments,
// which for a large file write is the single most expensive thing in the
// turn — the TypeScript side moved off the SDK's BetaMessageStream for
// exactly this reason.
type blockState struct {
	// providerIndex is the API's content index; position is the block's
	// index within the assistant message being built, which is what the
	// event protocol's contentIndex carries.
	providerIndex int
	position      int
	kind          string

	text      strings.Builder
	thinking  strings.Builder
	signature string

	// Tool call fields.
	id    string
	name  string
	input strings.Builder
	// args is the best-effort parse of input so far, for partial snapshots.
	// Replaced wholesale, never mutated in place.
	args map[string]any

	closed bool
}

// translator turns a stream of SSE payloads into ai.AssistantEvent values.
//
// The index bookkeeping is the core of the design. The API interleaves
// blocks: index 0 might be text, index 1 a tool call whose input arrives in
// four pieces, index 2 more text. A tool call is only complete at its
// content_block_stop, and until then its input is not valid JSON.
// Accumulating by provider index and emitting by message position is what
// makes it possible to emit only complete calls.
type translator struct {
	model ai.Model
	out   ai.Message
	// blocks in emission order; byIndex maps the API's content index.
	blocks  []*blockState
	byIndex map[int]*blockState

	sawMessageStart bool
	sawStopReason   bool
}

func newTranslator(model ai.Model) *translator {
	return &translator{
		model:   model,
		out:     newAssistantMessage(model),
		byIndex: make(map[int]*blockState, 4),
	}
}

// consumeStream reads the whole SSE body and pushes every event, always
// terminating with done or error. It never returns an error: the contract
// puts failures inside the stream, and a truncated response is reported as
// an error event rather than silently accepted (a half answer that looks
// complete is worse than a visible failure).
func (t *translator) consumeStream(ctx context.Context, body io.Reader, push func(ai.AssistantEvent)) {
	scanner := bufio.NewScanner(body)
	// A single SSE line holds one JSON event. A large input_json_delta for a
	// big tool argument can be hundreds of kilobytes, well past bufio's 64KB
	// default, which would fail mid-tool-call.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		raw, ok := sseData(line)
		if !ok {
			continue
		}
		if err := t.consume(raw, push); err != nil {
			t.fail(ctx, push, err)
			return
		}
	}

	if err := scanner.Err(); err != nil {
		// A cancelled context shows up here rather than at the HTTP call:
		// reading the body is where cancellation lands.
		if ctx.Err() != nil {
			t.fail(ctx, push, ctx.Err())
			return
		}
		t.fail(ctx, push, fmt.Errorf("anthropic: read stream: %w", err))
		return
	}

	t.finish(ctx, push)
}

// consume processes one SSE payload. An error is returned only for a
// protocol violation: a delta for a block that was never started, input
// JSON that never became valid, a tool call with no name. Both kinds mean
// the stream is inconsistent with what we sent, and continuing would attach
// text to the wrong block and produce a corrupt message.
func (t *translator) consume(raw []byte, push func(ai.AssistantEvent)) error {
	var ev streamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		// A malformed event is skipped rather than fatal. The stream may
		// include keep-alives or fields added after this client was written,
		// and dropping one unknown event is better than failing a turn that
		// is otherwise fine.
		return nil
	}

	switch ev.Type {
	case "message_start":
		t.sawMessageStart = true
		if ev.Message != nil {
			t.out.ResponseID = ev.Message.ID
			if ev.Message.Model != "" && ev.Message.Model != t.model.ID {
				t.out.ResponseModel = ev.Message.Model
			}
			if ev.Message.Usage != nil {
				mergeUsage(t.out.Usage, ev.Message.Usage)
				t.refreshCost()
			}
		}
		push(ai.EvStart{Partial: t.snapshot()})
		return nil

	case "content_block_start":
		if ev.ContentBlock == nil {
			return nil
		}
		cb := ev.ContentBlock
		switch cb.Type {
		case "text", "thinking", "redacted_thinking", "tool_use":
		default:
			// Unknown block kinds (the API adds them over time) are not
			// modelled: no block, no events. Their stop, if any, finds
			// nothing and is ignored.
			return nil
		}
		b := t.openBlock(ev.Index, cb.Type)
		switch cb.Type {
		case "text":
			// Seed from content_block_start, exactly as pi does
			// (anthropic-messages.ts:696-703). The first-party API sends an
			// empty string here and streams the text as deltas, so seeding
			// is a no-op there; Anthropic-compatible endpoints exist that
			// put the whole text in the start event and send no deltas
			// (DeepSeek's /anthropic did exactly that for a thinking
			// block), and ignoring the start text produced an empty block
			// the endpoint then refused to replay.
			b.text.WriteString(cb.Text)
			t.out.Content = append(t.out.Content, ai.TextBlock(cb.Text))
			b.position = len(t.out.Content) - 1
			push(ai.EvTextStart{ContentIndex: b.position, Partial: t.snapshot()})
		case "thinking", "redacted_thinking":
			block := ai.ContentBlock{
				Type:              ai.BlockThinking,
				Thinking:          cb.Thinking,
				ThinkingSignature: cb.Signature,
				Redacted:          cb.Type == "redacted_thinking",
			}
			if cb.Type == "redacted_thinking" {
				// Redacted thinking carries its encrypted payload in data;
				// the visible text is a placeholder.
				block.ThinkingSignature = cb.Data
				block.Thinking = "[Reasoning redacted]"
			}
			// Seed the accumulator from the start event too: deltas append
			// to it (pi: anthropic-messages.ts:704-715), so a response that
			// arrives whole in content_block_start keeps its text.
			b.thinking.WriteString(block.Thinking)
			t.out.Content = append(t.out.Content, block)
			b.position = len(t.out.Content) - 1
			push(ai.EvThinkingStart{ContentIndex: b.position, Partial: t.snapshot()})
		case "tool_use":
			b.id = cb.ID
			b.name = cb.Name
			// A tool_use can arrive with its input already populated, which
			// is what happens when a proxy sends a non-streaming-style block
			// inside a stream.
			//
			// An EMPTY object is skipped on purpose. The API opens every
			// streaming tool_use with "input":{} and then streams the real
			// arguments as deltas, so seeding from it would prepend "{}" to
			// the accumulated fragments and produce "{}{\"file_path\":...}" —
			// which fails to decode at the one moment the consumer most
			// needs it to.
			if len(cb.Input) > 0 && !isEmptyJSON(cb.Input) {
				b.input.Write(cb.Input)
				b.args = decodeArgs(cb.Input)
			}
			t.out.Content = append(t.out.Content, ai.ToolCallBlock(cb.ID, cb.Name, cloneArgs(b.args)))
			b.position = len(t.out.Content) - 1
			push(ai.EvToolCallStart{ContentIndex: b.position, Partial: t.snapshot()})
		}
		return nil

	case "content_block_delta":
		if ev.Delta == nil {
			return nil
		}
		b, ok := t.byIndex[ev.Index]
		if !ok {
			return fmt.Errorf(
				"anthropic: %s delta for block %d, which was never started", ev.Delta.Type, ev.Index)
		}

		switch ev.Delta.Type {
		case "text_delta":
			if b.kind != "text" {
				return fmt.Errorf("anthropic: text_delta on a %s block at index %d", b.kind, ev.Index)
			}
			if ev.Delta.Text == "" {
				return nil
			}
			b.text.WriteString(ev.Delta.Text)
			t.out.Content[b.position].Text = b.text.String()
			push(ai.EvTextDelta{
				ContentIndex: b.position,
				Delta:        ev.Delta.Text,
				Partial:      t.snapshot(),
			})

		case "thinking_delta":
			if b.kind != "thinking" {
				return fmt.Errorf("anthropic: thinking_delta on a %s block at index %d", b.kind, ev.Index)
			}
			if ev.Delta.Thinking == "" {
				return nil
			}
			b.thinking.WriteString(ev.Delta.Thinking)
			t.out.Content[b.position].Thinking = b.thinking.String()
			push(ai.EvThinkingDelta{
				ContentIndex: b.position,
				Delta:        ev.Delta.Thinking,
				Partial:      t.snapshot(),
			})

		case "signature_delta":
			// The signature arrives in pieces and must be reassembled
			// before the thinking block can be replayed next turn.
			b.signature += ev.Delta.Signature
			t.out.Content[b.position].ThinkingSignature = b.signature

		case "input_json_delta":
			if b.kind != "tool_use" {
				return fmt.Errorf("anthropic: input_json_delta on a %s block at index %d", b.kind, ev.Index)
			}
			b.input.WriteString(ev.Delta.PartialJSON)
			// Best-effort parse for the partial snapshot: a prefix is
			// usually invalid JSON, and that is fine — the authoritative
			// parse happens at content_block_stop. The last good parse is
			// kept so the snapshot never regresses to empty.
			if args := decodeArgs([]byte(b.input.String())); args != nil {
				b.args = args
				t.out.Content[b.position].Arguments = cloneArgs(args)
			}
			push(ai.EvToolCallDelta{
				ContentIndex: b.position,
				Delta:        ev.Delta.PartialJSON,
				Partial:      t.snapshot(),
			})

		default:
			// citations_delta and anything the API adds later.
			return nil
		}
		return nil

	case "content_block_stop":
		b, ok := t.byIndex[ev.Index]
		if !ok {
			return nil
		}
		b.closed = true

		switch b.kind {
		case "text":
			push(ai.EvTextEnd{
				ContentIndex: b.position,
				Content:      b.text.String(),
				Partial:      t.snapshot(),
			})
		case "thinking":
			push(ai.EvThinkingEnd{
				ContentIndex: b.position,
				Content:      b.thinking.String(),
				Partial:      t.snapshot(),
			})
		case "tool_use":
			if b.name == "" {
				return fmt.Errorf("anthropic: tool_use block %d has no name", ev.Index)
			}
			raw := strings.TrimSpace(b.input.String())
			if raw == "" {
				raw = "{}"
			}
			if !json.Valid([]byte(raw)) {
				// Better to fail loudly than hand the engine JSON that will
				// fail to decode somewhere further from the cause.
				return fmt.Errorf(
					"anthropic: tool_use %s accumulated invalid JSON input %q", b.name, truncateForError(raw))
			}
			args := decodeArgs([]byte(raw))
			b.args = args
			t.out.Content[b.position].Arguments = cloneArgs(args)
			push(ai.EvToolCallEnd{
				ContentIndex: b.position,
				ToolCall:     t.out.Content[b.position],
				Partial:      t.snapshot(),
			})
		}
		return nil

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			t.out.RawStopReason = ev.Delta.StopReason
			t.out.StopReason, t.out.ErrorMessage = mapStopReason(ev.Delta.StopReason)
			t.sawStopReason = true
		}
		if ev.Usage != nil {
			mergeUsage(t.out.Usage, ev.Usage)
			t.refreshCost()
		}
		return nil

	case "message_stop":
		return nil

	case "error":
		if ev.Error != nil {
			// The type goes into Message as well as Code. A stream error
			// event carries no status code, and IsOverloaded has to be able
			// to see that it is an overload — otherwise a 529 arriving
			// mid-stream is not recognised as retryable, which is exactly
			// when capacity is scarce and recognition matters.
			return &apiError{Code: ev.Error.Type, Message: ev.Error.Message}
		}
		return fmt.Errorf("anthropic: stream reported an error with no detail")

	case "ping":
		return nil

	default:
		return nil
	}
}

// finish reports whether the stream was structurally complete and pushes the
// terminal event. A stream that ends without message_start, without a stop
// reason, or with an unclosed block was cut short. Silently returning what
// arrived makes a truncated answer look like a complete one, and the user has
// no way to tell.
func (t *translator) finish(ctx context.Context, push func(ai.AssistantEvent)) {
	if !t.sawMessageStart {
		t.fail(ctx, push, fmt.Errorf("anthropic: stream ended before any message started"))
		return
	}
	if !t.sawStopReason {
		t.fail(ctx, push, fmt.Errorf("anthropic: stream ended without a stop reason; the response was truncated"))
		return
	}
	for _, b := range t.blocks {
		if !b.closed {
			t.fail(ctx, push, fmt.Errorf(
				"anthropic: block %d (%s) never received content_block_stop", b.providerIndex, b.kind))
			return
		}
	}
	if t.out.StopReason == ai.StopError || t.out.StopReason == ai.StopAborted {
		msg := t.out.ErrorMessage
		if msg == "" {
			msg = "anthropic: stream ended with stop reason " + string(t.out.StopReason)
		}
		t.fail(ctx, push, fmt.Errorf("%s", msg))
		return
	}
	push(ai.EvDone{Reason: t.out.StopReason, Message: t.out})
}

// fail pushes the terminal error event, carrying the partial message so the
// consumer sees what arrived alongside the failure (pi: anthropic-messages.ts
// :887-897).
func (t *translator) fail(ctx context.Context, push func(ai.AssistantEvent), err error) {
	reason := ai.StopError
	if ctx.Err() != nil {
		reason = ai.StopAborted
	}
	t.out.StopReason = reason
	t.out.ErrorMessage = err.Error()
	push(ai.EvError{Reason: reason, Error: t.out})
}

func (t *translator) openBlock(providerIndex int, kind string) *blockState {
	if b, ok := t.byIndex[providerIndex]; ok {
		return b
	}
	b := &blockState{providerIndex: providerIndex, kind: kind}
	t.byIndex[providerIndex] = b
	t.blocks = append(t.blocks, b)
	return b
}

// snapshot deep-copies the message so far for an event's partial. The live
// message keeps mutating after the event is pushed; sharing it would make
// every earlier event's partial show the final content, which is exactly
// what "the response so far" must not mean.
func (t *translator) snapshot() ai.Message {
	return cloneMessage(t.out)
}

// refreshCost recomputes the derived usage totals and cost, as pi does on
// every usage-bearing event (anthropic-messages.ts:686-688, 854-857).
func (t *translator) refreshCost() {
	ai.SumTokens(t.out.Usage)
	ai.CalculateCost(t.model, t.out.Usage)
}

// mergeUsage folds a wire usage into the accumulator. Only fields the event
// actually carried are overwritten: message_delta often omits the input
// counts that message_start already reported, and zeroing them would lose
// real accounting (pi: anthropic-messages.ts:828-853).
func mergeUsage(into *ai.Usage, from *wireUsage) {
	if into == nil || from == nil {
		return
	}
	if from.InputTokens != nil {
		into.Input = *from.InputTokens
	}
	if from.OutputTokens != nil {
		into.Output = *from.OutputTokens
	}
	if from.CacheReadInputTokens != nil {
		into.CacheRead = *from.CacheReadInputTokens
	}
	if from.CacheCreationInputTokens != nil {
		into.CacheWrite = *from.CacheCreationInputTokens
	}
	if from.CacheCreation != nil && from.CacheCreation.Ephemeral1hInputTokens != nil {
		v := *from.CacheCreation.Ephemeral1hInputTokens
		into.CacheWrite1h = &v
	}
	if from.OutputTokensDetails != nil && from.OutputTokensDetails.ThinkingTokens != nil {
		v := *from.OutputTokensDetails.ThinkingTokens
		into.Reasoning = &v
	}
}

// mapStopReason maps the API's stop_reason onto the protocol's StopReason.
//
// max_tokens is the one that matters for safety: it means the answer was cut
// off, so the loop must not execute the batch's tool calls — their arguments
// may be half a JSON object (docs/pi-core-harness-plan.md §1.1 item 5).
// Unknown reasons are errors, not end_turn: mapping them to stop would make
// a truncated generation look complete.
//
// pi: anthropic-messages.ts:1613-1639.
func mapStopReason(reason string) (ai.StopReason, string) {
	switch reason {
	case "end_turn":
		return ai.StopStop, ""
	case "max_tokens":
		return ai.StopLength, ""
	case "tool_use":
		return ai.StopToolUse, ""
	case "refusal":
		return ai.StopError, "the model refused to complete the request"
	case "pause_turn":
		// A paused turn is resubmitted by the caller; to this layer it is a
		// clean stop.
		return ai.StopStop, ""
	case "stop_sequence":
		// We don't supply stop sequences, so this should never happen.
		return ai.StopStop, ""
	case "sensitive":
		return ai.StopError, "provider stopped with: sensitive"
	default:
		return ai.StopError, "unhandled anthropic stop reason: " + reason
	}
}

// decodeArgs parses a complete-or-partial JSON object, returning nil when it
// does not parse. Used only for partial snapshots; the authoritative parse
// happens at content_block_stop.
func decodeArgs(raw []byte) map[string]any {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil
	}
	return args
}

func cloneArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneArgs(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}

// cloneMessage deep-copies a message: content blocks, their argument maps,
// sections and tool declarations. Events carry partials that outlive later
// mutation of the live message.
func cloneMessage(m ai.Message) ai.Message {
	out := m
	out.Content = make([]ai.ContentBlock, len(m.Content))
	for i, b := range m.Content {
		b.Arguments = cloneArgs(b.Arguments)
		out.Content[i] = b
	}
	if m.Sections != nil {
		out.Sections = make(map[string]*string, len(m.Sections))
		for k, v := range m.Sections {
			value := v
			if v != nil {
				s := *v
				value = &s
			}
			out.Sections[k] = value
		}
	}
	if m.ToolsAdded != nil {
		out.ToolsAdded = append([]ai.Tool(nil), m.ToolsAdded...)
	}
	if m.ToolsRemoved != nil {
		out.ToolsRemoved = append([]ai.ToolReference(nil), m.ToolsRemoved...)
	}
	return out
}

// isEmptyJSON reports whether raw is an empty container or null — the shapes
// that carry no arguments and must not be prepended to streamed fragments.
func isEmptyJSON(raw []byte) bool {
	switch strings.TrimSpace(string(raw)) {
	case "{}", "null", "[]", `""`:
		return true
	}
	return false
}

func truncateForError(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
