package openai

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

// chunk is one decoded SSE payload of the Chat Completions dialect.
//
// Pointers where the dialect sends null: content is null on tool-call-only
// chunks and on the usage-only final chunk, and finish_reason is null on
// every chunk but the last — conflating those with zero values would emit
// empty text deltas.
type chunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Usage   *wireUsage
	Choices []struct {
		Index        int     `json:"index"`
		Delta        delta   `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type delta struct {
	Role             string          `json:"role"`
	Content          *string         `json:"content"`
	ReasoningContent *string         `json:"reasoning_content"`
	Reasoning        *string         `json:"reasoning"`
	ToolCalls        []toolCallDelta `json:"tool_calls"`
}

type toolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// wireUsage is the dialect's usage block. Only the final chunk (with null
// choices) carries it, and only when stream_options.include_usage was set.
type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// streamingBlock is one block of the assistant message under construction.
//
// position is the block's index within the message (the event protocol's
// contentIndex); streamIndex is the dialect's tool-call index, which is what
// deltas are keyed by. The two differ because blocks are created lazily in
// first-sight order.
type streamingBlock struct {
	position  int
	streamIdx int
	hasStream bool
	kind      string // "text" | "thinking" | "toolCall"

	text      strings.Builder
	thinking  strings.Builder
	signature string

	id   string
	name string
	args strings.Builder
}

// translator turns SSE chunks into ai.AssistantEvent values.
//
// Unlike the block-indexed Anthropic dialect, this one opens no blocks:
// text and reasoning arrive as bare content deltas and tool calls arrive
// keyed by a stream index that may precede the call's id and name. Blocks
// are therefore created on first sight and closed in order when the stream
// ends (pi: openai-completions.ts:403-478).
type translator struct {
	model    ai.Model
	out      ai.Message
	blocks   []*streamingBlock
	byStream map[int]*streamingBlock
	byID     map[string]*streamingBlock

	text     *streamingBlock
	thinking *streamingBlock

	sawFinish bool
}

func newTranslator(model ai.Model) *translator {
	return &translator{
		model:    model,
		out:      newAssistantMessage(model),
		byStream: make(map[int]*streamingBlock),
		byID:     make(map[string]*streamingBlock),
	}
}

// consumeStream reads the whole SSE body and pushes every event, always
// terminating with done or error.
func (t *translator) consumeStream(ctx context.Context, body io.Reader, push func(ai.AssistantEvent)) {
	// start goes out as soon as the response is in hand, before any chunk:
	// the protocol's contract is start → updates → terminal, and a stream
	// that dies on the first chunk still showed the consumer a start.
	push(ai.EvStart{Partial: cloneMessage(t.out)})

	scanner := bufio.NewScanner(body)
	// Tool-call arguments arrive as fragments on one line; a large write is
	// hundreds of kilobytes, past bufio's 64KB default.
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
		if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
			break
		}
		var ch chunk
		if err := json.Unmarshal(raw, &ch); err != nil {
			// Skip unparseable payloads: keep-alives and forward-compatible
			// additions must not kill an otherwise fine turn.
			continue
		}
		t.consume(&ch, push)
	}

	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			t.fail(ctx, push, ctx.Err())
			return
		}
		t.fail(ctx, push, fmt.Errorf("openai: read stream: %w", err))
		return
	}

	t.finish(ctx, push)
}

func (t *translator) consume(ch *chunk, push func(ai.AssistantEvent)) {
	if ch.ID != "" && t.out.ResponseID == "" {
		t.out.ResponseID = ch.ID
	}
	if ch.Model != "" && ch.Model != t.model.ID && t.out.ResponseModel == "" {
		t.out.ResponseModel = ch.Model
	}
	if ch.Usage != nil {
		mergeUsage(t.out.Usage, ch.Usage)
		t.refreshCost()
	}

	if len(ch.Choices) == 0 {
		return // the usage-only final chunk
	}
	choice := ch.Choices[0]

	if choice.FinishReason != nil && *choice.FinishReason != "" {
		t.out.RawStopReason = *choice.FinishReason
		t.out.StopReason, t.out.ErrorMessage = mapStopReason(*choice.FinishReason)
		t.sawFinish = true
	}

	d := choice.Delta
	if d.Content != nil && *d.Content != "" {
		block := t.ensureText(push)
		block.text.WriteString(*d.Content)
		t.out.Content[block.position].Text = block.text.String()
		push(ai.EvTextDelta{
			ContentIndex: block.position,
			Delta:        *d.Content,
			Partial:      cloneMessage(t.out),
		})
	}

	// Endpoints disagree on the reasoning field name; the first non-empty
	// one wins so a server sending both does not duplicate the thinking.
	if reasoning := firstNonEmptyString(d.ReasoningContent, d.Reasoning); reasoning != nil {
		block := t.ensureThinking(push)
		block.thinking.WriteString(*reasoning)
		block.signature = reasoningFieldName(d)
		t.out.Content[block.position].Thinking = block.thinking.String()
		t.out.Content[block.position].ThinkingSignature = block.signature
		push(ai.EvThinkingDelta{
			ContentIndex: block.position,
			Delta:        *reasoning,
			Partial:      cloneMessage(t.out),
		})
	}

	for _, tc := range d.ToolCalls {
		block := t.ensureToolCall(tc, push)
		if block == nil {
			continue
		}
		if block.id == "" && tc.ID != "" {
			block.id = tc.ID
			t.byID[tc.ID] = block
			t.out.Content[block.position].ID = tc.ID
		}
		if block.name == "" && tc.Function != nil && tc.Function.Name != "" {
			block.name = tc.Function.Name
			t.out.Content[block.position].Name = tc.Function.Name
		}
		// One toolcall_delta per tool_calls entry, empty string included,
		// matching pi: the event sequence is part of the wire contract the
		// double-run diff checks, and a skipped empty delta would make the
		// two implementations disagree on every tool call.
		delta := ""
		if tc.Function != nil && tc.Function.Arguments != "" {
			delta = tc.Function.Arguments
			block.args.WriteString(tc.Function.Arguments)
			if args := decodeArgs([]byte(block.args.String())); args != nil {
				t.out.Content[block.position].Arguments = cloneArgs(args)
			}
		}
		push(ai.EvToolCallDelta{
			ContentIndex: block.position,
			Delta:        delta,
			Partial:      cloneMessage(t.out),
		})
	}
}

func (t *translator) ensureText(push func(ai.AssistantEvent)) *streamingBlock {
	if t.text != nil {
		return t.text
	}
	b := &streamingBlock{kind: "text"}
	t.out.Content = append(t.out.Content, ai.TextBlock(""))
	b.position = len(t.out.Content) - 1
	t.blocks = append(t.blocks, b)
	t.text = b
	push(ai.EvTextStart{ContentIndex: b.position, Partial: cloneMessage(t.out)})
	return b
}

func (t *translator) ensureThinking(push func(ai.AssistantEvent)) *streamingBlock {
	if t.thinking != nil {
		return t.thinking
	}
	b := &streamingBlock{kind: "thinking"}
	t.out.Content = append(t.out.Content, ai.ContentBlock{Type: ai.BlockThinking})
	b.position = len(t.out.Content) - 1
	t.blocks = append(t.blocks, b)
	t.thinking = b
	push(ai.EvThinkingStart{ContentIndex: b.position, Partial: cloneMessage(t.out)})
	return b
}

func (t *translator) ensureToolCall(tc toolCallDelta, push func(ai.AssistantEvent)) *streamingBlock {
	// The dialect keys deltas by stream index; some servers omit it and
	// only ever send the id once. Both are honoured, index first.
	var b *streamingBlock
	if tc.Index != nil {
		b = t.byStream[*tc.Index]
	}
	if b == nil && tc.ID != "" {
		b = t.byID[tc.ID]
	}
	if b == nil {
		b = &streamingBlock{kind: "toolCall"}
		if tc.Index != nil {
			b.streamIdx = *tc.Index
			b.hasStream = true
			t.byStream[*tc.Index] = b
		}
		b.id = tc.ID
		b.name = toolCallName(tc)
		if tc.ID != "" {
			t.byID[tc.ID] = b
		}
		t.out.Content = append(t.out.Content, ai.ToolCallBlock(b.id, b.name, nil))
		b.position = len(t.out.Content) - 1
		t.blocks = append(t.blocks, b)
		push(ai.EvToolCallStart{ContentIndex: b.position, Partial: cloneMessage(t.out)})
	}
	if tc.Index != nil && !b.hasStream {
		b.streamIdx = *tc.Index
		b.hasStream = true
		t.byStream[*tc.Index] = b
	}
	if tc.ID != "" && t.byID[tc.ID] == nil {
		t.byID[tc.ID] = b
	}
	return b
}

// finish closes every open block in creation order and terminates the
// stream. A stream that never reported a finish_reason is truncated: the
// dialect's usage-only final chunk is not a terminator, so accepting it
// would make a cut-off answer look complete.
func (t *translator) finish(ctx context.Context, push func(ai.AssistantEvent)) {
	for _, b := range t.blocks {
		switch b.kind {
		case "text":
			push(ai.EvTextEnd{
				ContentIndex: b.position,
				Content:      b.text.String(),
				Partial:      cloneMessage(t.out),
			})
		case "thinking":
			push(ai.EvThinkingEnd{
				ContentIndex: b.position,
				Content:      b.thinking.String(),
				Partial:      cloneMessage(t.out),
			})
		case "toolCall":
			raw := strings.TrimSpace(b.args.String())
			if raw == "" {
				raw = "{}"
			}
			if !json.Valid([]byte(raw)) {
				t.fail(ctx, push, fmt.Errorf(
					"openai: tool call %s accumulated invalid JSON arguments %q", b.name, truncateForError(raw)))
				return
			}
			args := decodeArgs([]byte(raw))
			t.out.Content[b.position].Arguments = cloneArgs(args)
			push(ai.EvToolCallEnd{
				ContentIndex: b.position,
				ToolCall:     t.out.Content[b.position],
				Partial:      cloneMessage(t.out),
			})
		}
	}

	if ctx.Err() != nil {
		t.fail(ctx, push, ctx.Err())
		return
	}
	if !t.sawFinish {
		t.fail(ctx, push, fmt.Errorf("openai: stream ended without a finish_reason; the response was truncated"))
		return
	}
	if t.out.StopReason == ai.StopError || t.out.StopReason == ai.StopAborted {
		msg := t.out.ErrorMessage
		if msg == "" {
			msg = "openai: stream ended with stop reason " + string(t.out.StopReason)
		}
		t.fail(ctx, push, fmt.Errorf("%s", msg))
		return
	}
	push(ai.EvDone{Reason: t.out.StopReason, Message: t.out})
}

func (t *translator) fail(ctx context.Context, push func(ai.AssistantEvent), err error) {
	reason := ai.StopError
	if ctx.Err() != nil {
		reason = ai.StopAborted
	}
	t.out.StopReason = reason
	t.out.ErrorMessage = err.Error()
	push(ai.EvError{Reason: reason, Error: t.out})
}

func (t *translator) refreshCost() {
	ai.SumTokens(t.out.Usage)
	ai.CalculateCost(t.model, t.out.Usage)
}

func mergeUsage(into *ai.Usage, from *wireUsage) {
	if into == nil || from == nil {
		return
	}
	into.Input = from.PromptTokens
	into.Output = from.CompletionTokens
	into.TotalTokens = from.TotalTokens
	if from.PromptTokensDetails != nil {
		into.CacheRead = from.PromptTokensDetails.CachedTokens
	}
	if from.CompletionTokensDetails != nil {
		v := from.CompletionTokensDetails.ReasoningTokens
		into.Reasoning = &v
	}
}

// mapStopReason maps the dialect's finish_reason onto the protocol's
// StopReason (pi: openai-completions.ts:1562-1586).
func mapStopReason(reason string) (ai.StopReason, string) {
	switch reason {
	case "stop", "end":
		return ai.StopStop, ""
	case "length":
		return ai.StopLength, ""
	case "function_call", "tool_calls":
		return ai.StopToolUse, ""
	case "content_filter":
		return ai.StopError, "provider finish_reason: content_filter"
	case "network_error":
		return ai.StopError, "provider finish_reason: network_error"
	default:
		return ai.StopError, "provider finish_reason: " + reason
	}
}

// reasoningFieldName records which field the reasoning arrived under, so a
// replay can send it back on the same field (pi keeps the field name in the
// thinking signature slot, openai-completions.ts:625-628).
func reasoningFieldName(d delta) string {
	if d.ReasoningContent != nil && *d.ReasoningContent != "" {
		return "reasoning_content"
	}
	if d.Reasoning != nil && *d.Reasoning != "" {
		return "reasoning"
	}
	return ""
}

func toolCallName(tc toolCallDelta) string {
	if tc.Function != nil {
		return tc.Function.Name
	}
	return ""
}

func firstNonEmptyString(values ...*string) *string {
	for _, v := range values {
		if v != nil && *v != "" {
			return v
		}
	}
	return nil
}

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

func truncateForError(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
