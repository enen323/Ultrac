package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"ultrac/internal/contract/provider"
	"ultrac/internal/logging"
)

// streamEvent is one decoded SSE payload.
//
// The Type tag discriminates. Every field is optional because the API omits
// whatever does not apply to an event: a text delta carries no index, a block
// delta carries no id.
type streamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Role  string `json:"role"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`

	Index        int `json:"index"`
	ContentBlock *struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Input any    `json:"input"`
	} `json:"content_block"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// blockState accumulates one content block's deltas.
//
// The accumulated input is a raw JSON string, not a decoded map. Decoding
// partial JSON on every delta is O(n²) over the tool arguments, which for a
// large file write is the single most expensive thing in the turn — the
// TypeScript side moved off the SDK's BetaMessageStream for exactly this
// reason.
type blockState struct {
	index int
	kind  string

	// text accumulates text and thinking deltas.
	text string
	// thinking accumulates reasoning deltas, kept apart from text because
	// they go to different places in the wire format.
	thinking  string
	signature string

	// Tool call fields.
	id    string
	name  string
	input strings.Builder

	closed bool
}

// eventResult is what one SSE payload produced.
//
// A named struct rather than four positional return values: with four, a
// mistaken "" in the *Usage slot is a compile error at best and a silent nil
// dereference at worst, and the call site has to remember the order of three
// mostly-empty fields.
type eventResult struct {
	Chunks []provider.Chunk
	Stop   provider.StopReason
	Usage  *provider.Usage
}

// accumulator turns a stream of SSE payloads into Chunks.
//
// The index-keyed map is the core of the design. The API interleaves blocks:
// index 0 might be text, index 1 a tool call whose input arrives in four
// pieces, index 2 more text. A tool call is only complete at its
// content_block_stop, and until then its input is not valid JSON. Accumulating
// by index is what makes it possible to emit only complete calls.
type accumulator struct {
	blocks map[int]*blockState
	// order preserves emission order, because the map alone would sort blocks
	// by index and interleave them differently from how the model wrote them.
	order []int
	// pendingStop holds a stop_reason until every block is closed.
	pendingStop provider.StopReason
	// sawMessage records that message_start arrived, so a stream that ends
	// without it can be reported rather than treated as a successful empty
	// response.
	sawMessage bool
	// sawStop records that a stop_reason arrived.
	sawStop bool
}

func newAccumulator() *accumulator {
	return &accumulator{blocks: make(map[int]*blockState, 4)}
}

func (a *accumulator) block(index int) *blockState {
	b, ok := a.blocks[index]
	if !ok {
		b = &blockState{index: index}
		a.blocks[index] = b
		a.order = append(a.order, index)
	}
	return b
}

// consume processes one SSE payload.
//
// It returns the chunks to emit, the stop reason if this event carried one,
// and the usage if it did. Returning all three from one call keeps the
// caller's loop free of event-type branching, which is where this logic
// normally goes wrong.
//
// An error is returned only for a protocol violation: a delta for a block that
// was never started, or input_json_delta on a text block. Both mean the stream
// is inconsistent with what we sent, and continuing would attach text to the
// wrong block and produce a corrupt message.
func (a *accumulator) consume(raw []byte) (eventResult, error) {
	var ev streamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		// A malformed event is skipped rather than fatal. The stream may
		// include keep-alives or fields added after this client was written,
		// and dropping one unknown event is better than failing a turn that
		// is otherwise fine.
		return eventResult{}, nil
	}

	switch ev.Type {
	case "message_start":
		a.sawMessage = true
		if ev.Message != nil && ev.Message.Usage != nil {
			u := convertUsage(ev.Message.Usage.InputTokens, ev.Message.Usage.OutputTokens,
				ev.Message.Usage.CacheReadInputTokens, ev.Message.Usage.CacheCreationInputTokens)
			return eventResult{Chunks: []provider.Chunk{{Kind: provider.ChunkUsage, Usage: &u}}}, nil
		}
		return eventResult{}, nil

	case "content_block_start":
		b := a.block(ev.Index)
		if ev.ContentBlock != nil {
			b.kind = ev.ContentBlock.Type
			b.id = ev.ContentBlock.ID
			b.name = ev.ContentBlock.Name
			// A tool_use can arrive with its input already populated, which is
			// what happens when a proxy sends a non-streaming-style block inside
			// a stream.
			//
			// An EMPTY object is skipped on purpose. The API opens every
			// streaming tool_use with "input":{} and then streams the real
			// arguments as deltas, so seeding from it would prepend "{}" to the
			// accumulated fragments and produce "{}{\"file_path\":...}" — which
			// fails to decode at the one moment the consumer most needs it to.
			if ev.ContentBlock.Input != nil {
				if enc, err := json.Marshal(ev.ContentBlock.Input); err == nil && !isEmptyJSON(enc) {
					b.input.Reset()
					b.input.Write(enc)
				}
			}
			// The text in a content_block_start is ignored on purpose. The API
			// repeats the same text in the following delta, so honouring both
			// would double every first word.
		}
		return eventResult{}, nil

	case "content_block_delta":
		if ev.Delta == nil {
			return eventResult{}, nil
		}
		b, ok := a.blocks[ev.Index]
		if !ok {
			return eventResult{}, fmt.Errorf(
				"anthropic: %s delta for block %d, which was never started", ev.Delta.Type, ev.Index)
		}

		switch ev.Delta.Type {
		case "text_delta":
			if b.kind != "text" {
				return eventResult{}, fmt.Errorf(
					"anthropic: text_delta on a %s block at index %d", b.kind, ev.Index)
			}
			if ev.Delta.Text == "" {
				return eventResult{}, nil
			}
			b.text += ev.Delta.Text
			return eventResult{Chunks: []provider.Chunk{{Kind: provider.ChunkText, Text: ev.Delta.Text}}}, nil

		case "thinking_delta":
			if b.kind != "thinking" {
				return eventResult{}, fmt.Errorf(
					"anthropic: thinking_delta on a %s block at index %d", b.kind, ev.Index)
			}
			if ev.Delta.Thinking == "" {
				return eventResult{}, nil
			}
			b.thinking += ev.Delta.Thinking
			return eventResult{Chunks: []provider.Chunk{{Kind: provider.ChunkThinking, Text: ev.Delta.Thinking}}}, nil

		case "signature_delta":
			b.signature = ev.Delta.Signature
			return eventResult{}, nil

		case "input_json_delta":
			if b.kind != "tool_use" && b.kind != "server_tool_use" {
				return eventResult{}, fmt.Errorf(
					"anthropic: input_json_delta on a %s block at index %d", b.kind, ev.Index)
			}
			b.input.WriteString(ev.Delta.PartialJSON)
			return eventResult{}, nil

		default:
			// citations_delta and anything the API adds later.
			return eventResult{}, nil
		}

	case "content_block_stop":
		b, ok := a.blocks[ev.Index]
		if !ok {
			return eventResult{}, nil
		}
		b.closed = true

		// Only a tool call is emitted here. Text and thinking were already
		// streamed as deltas; re-emitting the assembled value here would
		// duplicate every character in the transcript.
		if b.kind != "tool_use" && b.kind != "server_tool_use" {
			return eventResult{}, nil
		}
		if b.name == "" {
			return eventResult{}, fmt.Errorf("anthropic: tool_use block %d has no name", ev.Index)
		}

		raw := strings.TrimSpace(b.input.String())
		if raw == "" {
			raw = "{}"
		}
		if !json.Valid([]byte(raw)) {
			// Better to fail loudly than hand the engine JSON that will fail
			// to decode somewhere further from the cause.
			return eventResult{}, fmt.Errorf(
				"anthropic: tool_use %s accumulated invalid JSON input %q", b.name, truncateForError(raw))
		}

		id := b.id
		if id == "" {
			// A tool call with no id cannot be matched to its result later, so
			// synthesise a stable one rather than emitting an empty id that
			// would correlate with every other idless call.
			id = "toolu_" + b.name + "_" + fmt.Sprint(b.index)
		}
		call := provider.ToolCall{
			ID:    id,
			Name:  b.name,
			Input: json.RawMessage(raw),
		}
		return eventResult{Chunks: []provider.Chunk{{Kind: provider.ChunkToolCall, ToolCall: &call}}}, nil

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			a.pendingStop = convertStopReason(ev.Delta.StopReason)
			a.sawStop = true
		}
		var u *provider.Usage
		if ev.Usage != nil {
			usage := convertUsage(ev.Usage.InputTokens, ev.Usage.OutputTokens,
				ev.Usage.CacheReadInputTokens, ev.Usage.CacheCreationInputTokens)
			u = &usage
		}
		res := eventResult{Stop: a.pendingStop}
		if u != nil {
			res.Usage = u
		}
		return res, nil

	case "message_stop":
		return eventResult{}, nil

	case "error":
		if ev.Error != nil {
			// The type goes into Message as well as Code. A stream error event
			// carries no status code, and IsOverloaded has to be able to see
			// that it is an overload — otherwise a 529 arriving mid-stream is
			// not recognised as retryable, which is exactly when capacity is
			// scarce and recognition matters.
			return eventResult{}, &logging.APIError{
				Code:    ev.Error.Type,
				Message: ev.Error.Message,
			}
		}
		return eventResult{}, fmt.Errorf("anthropic: stream reported an error with no detail")

	case "ping":
		return eventResult{}, nil

	default:
		return eventResult{}, nil
	}
}

// finish reports whether the stream was structurally complete.
//
// A stream that ends without message_start, or without a stop reason, was cut
// short. Silently returning what arrived makes a truncated answer look like a
// complete one, and the user has no way to tell.
func (a *accumulator) finish() error {
	if !a.sawMessage {
		return fmt.Errorf("anthropic: stream ended before any message started")
	}
	if !a.sawStop {
		return fmt.Errorf("anthropic: stream ended without a stop reason; the response was truncated")
	}
	for _, idx := range a.order {
		if b := a.blocks[idx]; b != nil && !b.closed {
			return fmt.Errorf("anthropic: block %d (%s) never received content_block_stop", idx, b.kind)
		}
	}
	return nil
}

// blocksInOrder returns the accumulated blocks in emission order. Used by
// tests and by a future non-streaming path.
func (a *accumulator) blocksInOrder() []*blockState {
	out := make([]*blockState, 0, len(a.order))
	for _, idx := range a.order {
		if b := a.blocks[idx]; b != nil {
			out = append(out, b)
		}
	}
	return out
}

func convertStopReason(s string) provider.StopReason {
	switch s {
	case "end_turn":
		return provider.StopEndTurn
	case "tool_use":
		return provider.StopToolUse
	case "max_tokens":
		return provider.StopMaxTokens
	case "stop_sequence":
		return provider.StopStopSequence
	case "refusal":
		return provider.StopRefusal
	default:
		// Unknown reasons pass through rather than becoming end_turn. Mapping
		// them to end_turn would make a truncated generation look complete.
		return provider.StopReason(s)
	}
}

func convertUsage(in, out, cacheRead, cacheWrite int) provider.Usage {
	return provider.Usage{
		InputTokens:      in,
		OutputTokens:     out,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
	}
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
