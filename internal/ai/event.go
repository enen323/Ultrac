package ai

import (
	"encoding/json"
	"fmt"
	"iter"
	"sync"
	"time"
)

// AssistantEvent is one event of the streaming protocol (contract C2).
//
// The 12 variants and their fields are transcribed one-for-one from pi's
// AssistantMessageEvent union (packages/ai/src/types.ts:801-817):
//
//	start              partial
//	text_start         contentIndex, partial
//	text_delta         contentIndex, delta, partial
//	text_end           contentIndex, content, partial
//	thinking_start     contentIndex, partial
//	thinking_delta     contentIndex, delta, partial
//	thinking_end       contentIndex, content, partial
//	toolcall_start     contentIndex, partial
//	toolcall_delta     contentIndex, delta, partial
//	toolcall_end       contentIndex, toolCall, partial
//	done               reason (stop|length|toolUse|deferred), message
//	error              reason (aborted|error), error
//
// Protocol rules that are part of the contract (pi: types.ts:785-800):
//
//   - A successful stream emits `start` before any update and terminates
//     with `done`. A stream whose request setup fails may terminate directly
//     with `error`; after `start`, failures also terminate with `error`.
//   - Updates and `done` never appear before `start`.
//   - `partial` is the live response-so-far object, not an event-time
//     snapshot: text and thinking blocks are empty at their `*_start` and
//     grow only through `*_delta` until the authoritative `*_end`. Redacted
//     thinking may be complete at start and emit no deltas. Tool-call
//     arguments at `toolcall_start` are provider-specific; `toolcall_delta`
//     carries subsequent JSON updates.
type AssistantEvent interface {
	// eventName is the wire discriminant. It is unexported so the union is
	// closed: new variants are added here, in this package, never by
	// implementers outside it.
	eventName() string
}

// EvStart opens a response. pi: types.ts:802.
type EvStart struct {
	Partial Message `json:"-"`
}

// EvTextStart opens a text block at ContentIndex. pi: types.ts:803.
type EvTextStart struct {
	ContentIndex int     `json:"contentIndex"`
	Partial      Message `json:"-"`
}

// EvTextDelta appends to the text block at ContentIndex. pi: types.ts:804.
type EvTextDelta struct {
	ContentIndex int     `json:"contentIndex"`
	Delta        string  `json:"delta"`
	Partial      Message `json:"-"`
}

// EvTextEnd closes the text block at ContentIndex with its full text.
// pi: types.ts:805.
type EvTextEnd struct {
	ContentIndex int     `json:"contentIndex"`
	Content      string  `json:"content"`
	Partial      Message `json:"-"`
}

// EvThinkingStart opens a thinking block at ContentIndex. pi: types.ts:806.
type EvThinkingStart struct {
	ContentIndex int     `json:"contentIndex"`
	Partial      Message `json:"-"`
}

// EvThinkingDelta appends to the thinking block at ContentIndex.
// pi: types.ts:807.
type EvThinkingDelta struct {
	ContentIndex int     `json:"contentIndex"`
	Delta        string  `json:"delta"`
	Partial      Message `json:"-"`
}

// EvThinkingEnd closes the thinking block at ContentIndex. pi: types.ts:808.
type EvThinkingEnd struct {
	ContentIndex int     `json:"contentIndex"`
	Content      string  `json:"content"`
	Partial      Message `json:"-"`
}

// EvToolCallStart opens a tool-call block at ContentIndex. pi: types.ts:809.
type EvToolCallStart struct {
	ContentIndex int     `json:"contentIndex"`
	Partial      Message `json:"-"`
}

// EvToolCallDelta appends argument JSON to the tool call at ContentIndex.
// pi: types.ts:810.
type EvToolCallDelta struct {
	ContentIndex int     `json:"contentIndex"`
	Delta        string  `json:"delta"`
	Partial      Message `json:"-"`
}

// EvToolCallEnd closes the tool call at ContentIndex with the complete call.
// pi: types.ts:811.
type EvToolCallEnd struct {
	ContentIndex int          `json:"contentIndex"`
	ToolCall     ContentBlock `json:"toolCall"`
	Partial      Message      `json:"-"`
}

// EvDone terminates a successful stream. Reason is one of stop, length,
// toolUse, deferred (pi: types.ts:812-816).
type EvDone struct {
	Reason  StopReason `json:"reason"`
	Message Message    `json:"message"`
}

// EvError terminates a failed stream. Reason is aborted or error
// (pi: types.ts:817).
type EvError struct {
	Reason StopReason `json:"reason"`
	Error  Message    `json:"error"`
}

func (EvStart) eventName() string         { return "start" }
func (EvTextStart) eventName() string     { return "text_start" }
func (EvTextDelta) eventName() string     { return "text_delta" }
func (EvTextEnd) eventName() string       { return "text_end" }
func (EvThinkingStart) eventName() string { return "thinking_start" }
func (EvThinkingDelta) eventName() string { return "thinking_delta" }
func (EvThinkingEnd) eventName() string   { return "thinking_end" }
func (EvToolCallStart) eventName() string { return "toolcall_start" }
func (EvToolCallDelta) eventName() string { return "toolcall_delta" }
func (EvToolCallEnd) eventName() string   { return "toolcall_end" }
func (EvDone) eventName() string          { return "done" }
func (EvError) eventName() string         { return "error" }

// Name returns the wire discriminant of ev.
func Name(ev AssistantEvent) string { return ev.eventName() }

// Stream is one assistant response's event stream.
//
// It is the Go counterpart of pi's AssistantMessageEventStream
// (packages/ai/src/utils/event-stream.ts). Two shapes satisfy it: a provider
// that pushes into an EventStream, and a generator closed over by StreamOf.
//
// Events returns iter.Seq2 rather than a channel deliberately: the consumer
// stopping the loop is the cancellation signal, so a frontend that stops
// reading cannot leave a goroutine blocked on a send. Errors travel in the
// second slot, which makes ignoring them a visible choice.
//
// A Stream is single-consumption, like pi's async iterator: Events may be
// ranged once; a second range yields nothing. Result may be called from any
// goroutine, before or after consumption, and blocks until termination.
//
// Result blocks until the stream terminates and returns the final assistant
// message — the message carried by `done`, or the error message carried by
// `error`. Calling it before termination blocks; calling it after a stream
// that ended without a terminal event returns an error.
type Stream interface {
	Events() iter.Seq2[AssistantEvent, error]
	Result() (Message, error)
}

// EventStream is a concurrency-safe push/pull event stream. The producer
// calls Push for every event and End when finished; the consumer ranges over
// Events(). Push never blocks: like pi's FifoQueue, the pending queue is
// unbounded, so a producer is only ever waiting on its own I/O, which context
// cancellation unblocks.
//
// The final message is timed with a monotonic clock, mirroring
// AssistantMessageEventStream (event-stream.ts:96-129): durationMs is set on
// the `done`/`error` message unless it already has one or predates the
// stream.
type EventStream struct {
	mu       sync.Mutex
	nonEmpty *sync.Cond
	queue    []AssistantEvent
	done     bool
	final    *Message

	startedAt     time.Time
	startedAtMono time.Time
}

// NewEventStream returns an empty stream ready for Push/End.
func NewEventStream() *EventStream {
	s := &EventStream{
		startedAt:     time.Now(),
		startedAtMono: time.Now(),
	}
	s.nonEmpty = sync.NewCond(&s.mu)
	return s
}

// Push appends one event. Terminal events (done/error) are captured as the
// stream's result and close the stream; pushing after that is a no-op, as in
// pi (event-stream.ts:44-49).
func (s *EventStream) Push(ev AssistantEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	switch e := ev.(type) {
	case EvDone:
		msg := e.Message
		s.time(&msg)
		s.final = &msg
		ev = EvDone{Reason: e.Reason, Message: msg}
		s.done = true
	case EvError:
		msg := e.Error
		s.time(&msg)
		s.final = &msg
		ev = EvError{Reason: e.Reason, Error: msg}
		s.done = true
	}
	s.queue = append(s.queue, ev)
	s.nonEmpty.Broadcast()
}

// End marks the stream finished. Any consumer blocked in Events wakes up.
func (s *EventStream) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	s.nonEmpty.Broadcast()
}

// time stamps durationMs on a terminal message, following pi's rule
// (event-stream.ts:126-129): skip when already set or when the message
// predates the stream's creation.
func (s *EventStream) time(m *Message) {
	if m.DurationMS != 0 || m.TimestampMS < s.startedAt.UnixMilli() {
		return
	}
	m.DurationMS = time.Since(s.startedAtMono).Milliseconds()
}

func (s *EventStream) next() (AssistantEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.queue) == 0 && !s.done {
		s.nonEmpty.Wait()
	}
	if len(s.queue) > 0 {
		ev := s.queue[0]
		s.queue = s.queue[1:]
		return ev, true
	}
	return nil, false
}

// Events implements Stream.
func (s *EventStream) Events() iter.Seq2[AssistantEvent, error] {
	return func(yield func(AssistantEvent, error) bool) {
		for {
			ev, ok := s.next()
			if !ok {
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Result implements Stream. It blocks until the stream terminates. Failures
// travel inside the stream as EvError (pi's contract: a provider never throws
// once a stream exists), so a stream that ends without done/error is a
// protocol violation, reported as an error.
func (s *EventStream) Result() (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.done {
		s.nonEmpty.Wait()
	}
	if s.final != nil {
		return *s.final, nil
	}
	return Message{}, fmt.Errorf("ai: stream ended without a done or error event")
}

// StreamOf returns a Stream over a fixed event slice. It exists for tests and
// fake providers: the slice must end with EvDone or EvError. The terminal
// message is timed like a live stream.
func StreamOf(events ...AssistantEvent) Stream {
	s := NewEventStream()
	for _, ev := range events {
		s.Push(ev)
	}
	s.End()
	return s
}

// MarshalEvent encodes ev in the wire shape used by the print/json and RPC
// protocols. It follows pi's json-event.ts:20-61 projection: the cumulative
// `partial` snapshot is stripped (message_start carries the initial message,
// deltas build it, message_end carries the authoritative one), and
// toolcall_start additionally carries the id and toolName of the call it
// opens, taken from the partial's content block.
func MarshalEvent(ev AssistantEvent) ([]byte, error) {
	var body any
	var extra string
	var err error
	switch e := ev.(type) {
	case EvStart:
		body = struct{}{}
	case EvTextStart:
		body = e
	case EvTextDelta:
		body = e
	case EvTextEnd:
		body = e
	case EvThinkingStart:
		body = e
	case EvThinkingDelta:
		body = e
	case EvThinkingEnd:
		body = e
	case EvToolCallStart:
		extra, err = toolCallIdentity(e.Partial, e.ContentIndex)
		if err != nil {
			return nil, err
		}
		body = struct {
			ContentIndex int `json:"contentIndex"`
		}{ContentIndex: e.ContentIndex}
	case EvToolCallDelta:
		body = e
	case EvToolCallEnd:
		body = struct {
			ContentIndex int          `json:"contentIndex"`
			ToolCall     ContentBlock `json:"toolCall"`
		}{ContentIndex: e.ContentIndex, ToolCall: e.ToolCall}
	case EvDone:
		body = e
	case EvError:
		body = e
	default:
		return nil, fmt.Errorf("ai: unknown assistant event %T", ev)
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if len(payload) < 2 || payload[0] != '{' {
		return nil, fmt.Errorf("ai: event %s did not marshal to an object", Name(ev))
	}
	name, err := json.Marshal(Name(ev))
	if err != nil {
		return nil, err
	} // Splice: {"type":<name>[,<payload fields>[,<extra>]]}
	fields := payload[1 : len(payload)-1] // the payload's fields, without braces
	if extra != "" {
		if len(fields) > 0 {
			fields = append(fields, ',')
		}
		fields = append(fields, extra...)
	}
	out := make([]byte, 0, len(name)+len(fields)+10)
	out = append(out, '{', '"', 't', 'y', 'p', 'e', '"', ':')
	out = append(out, name...)
	if len(fields) > 0 {
		out = append(out, ',')
		out = append(out, fields...)
	}
	out = append(out, '}')
	return out, nil
}

// toolCallIdentity renders the `id`/`toolName` pair that toolcall_start
// carries on the wire (pi: json-event.ts:23-30). pi throws when the partial's
// block at that index is not a tool call — that is a producer bug, and
// silently dropping the identity would hide it, so the error is surfaced.
func toolCallIdentity(partial Message, index int) (string, error) {
	if index < 0 || index >= len(partial.Content) {
		return "", fmt.Errorf("ai: toolcall_start contentIndex %d out of range (partial has %d blocks)", index, len(partial.Content))
	}
	b := partial.Content[index]
	if b.Type != BlockToolCall {
		return "", fmt.Errorf("ai: toolcall_start content at index %d is %q, not a toolCall", index, b.Type)
	}
	id, _ := json.Marshal(b.ID)
	name, _ := json.Marshal(b.Name)
	return `"id":` + string(id) + `,"toolName":` + string(name), nil
}

// maxDecodedContentIndex bounds how much of a partial UnmarshalEvent will
// materialize for toolcall_start; beyond it the decoded partial stays empty.
const maxDecodedContentIndex = 4096

// UnmarshalEvent decodes the wire form produced by MarshalEvent. The
// `partial` field is absent on the wire, so decoded events carry a zero
// Message there; the loop rebuilds partials from deltas.
func UnmarshalEvent(data []byte) (AssistantEvent, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	switch probe.Type {
	case "start":
		return EvStart{}, nil
	case "text_start":
		var e EvTextStart
		return e, json.Unmarshal(data, &e)
	case "text_delta":
		var e EvTextDelta
		return e, json.Unmarshal(data, &e)
	case "text_end":
		var e EvTextEnd
		return e, json.Unmarshal(data, &e)
	case "thinking_start":
		var e EvThinkingStart
		return e, json.Unmarshal(data, &e)
	case "thinking_delta":
		var e EvThinkingDelta
		return e, json.Unmarshal(data, &e)
	case "thinking_end":
		var e EvThinkingEnd
		return e, json.Unmarshal(data, &e)
	case "toolcall_start":
		var e struct {
			ContentIndex int    `json:"contentIndex"`
			ID           string `json:"id"`
			ToolName     string `json:"toolName"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		// Rebuild a partial whose content block carries the identity, so a
		// decoded event is usable by the same code paths as a live one. The
		// wire form carries no partial; only a sane index is materialized.
		partial := Message{Role: RoleAssistant}
		if e.ContentIndex >= 0 && e.ContentIndex <= maxDecodedContentIndex {
			content := make([]ContentBlock, e.ContentIndex+1)
			content[e.ContentIndex] = ToolCallBlock(e.ID, e.ToolName, nil)
			partial.Content = content
		}
		return EvToolCallStart{ContentIndex: e.ContentIndex, Partial: partial}, nil
	case "toolcall_delta":
		var e EvToolCallDelta
		return e, json.Unmarshal(data, &e)
	case "toolcall_end":
		var e EvToolCallEnd
		return e, json.Unmarshal(data, &e)
	case "done":
		var e EvDone
		return e, json.Unmarshal(data, &e)
	case "error":
		var e EvError
		return e, json.Unmarshal(data, &e)
	default:
		return nil, fmt.Errorf("ai: unknown assistant event type %q", probe.Type)
	}
}
