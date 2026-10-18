// pi_json.go — the print/json wire protocol: one JSON object per line, the
// exact shape pi's modes/print-mode.ts writes (docs/pi-core-harness-plan.md
// §3.6 / §6.4, phase P5).
//
// # The protocol
//
// Every agent.AgentEvent becomes one line whose "type" is the pi
// AgentSessionEvent discriminant (packages/agent/src/types.ts:516-539 plus the
// session extensions in core/agent-session.ts:196-237 that this port emits):
//
//	agent_start
//	agent_end                messages
//	turn_start
//	turn_end                 message, toolResults
//	message_start            message
//	message_update           usage, assistantMessageEvent
//	message_end              message
//	tool_execution_start     toolCallId, toolName, args
//	tool_execution_update    toolCallId, toolName, args, partialResult
//	tool_execution_end       toolCallId, toolName, result, isError, durationMs?
//
// In JSON mode pi also writes the session header as the first line, before any
// event (print-mode.ts:122-127) — WriteHeader does that.
//
// # The message_update projection
//
// message_update is the one event that is not a pass-through: pi's
// json-event.ts:20-61 strips the cumulative `partial` snapshot from every
// assistant event (message_start carries the initial message, deltas build
// it, message_end carries the authoritative one) and adds the call's id and
// toolName to toolcall_start, read from the partial's content block. The
// assistant-event half of that projection is ai.MarshalEvent, frozen in P0;
// this file adds the outer {type, usage, assistantMessageEvent} envelope.
//
// # Why this lives beside the old writer
//
// ndjson.go speaks the Claude Code SDK dialect (stream_event / tool_use /
// result) for the deprecated engine path, which still runs until P7. The two
// protocols are not variants of each other — different discriminants,
// different message model — so they are two writers in one package rather
// than one writer with a mode flag that could silently mix shapes.
//
// # Layering
//
// This file imports internal/agent for the event union only. agent never
// imports transport, so the edge is one-way (same rule the tool package
// follows, plan §4.2).
package transport

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// PiJSONWriter serialises agent events as pi's JSON protocol.
//
// It is safe for concurrent use even though the current caller (print mode)
// writes from a single goroutine: P6's RPC mode pushes events and responses
// through the same writer, and a half-interleaved line is a stream no parser
// can read.
type PiJSONWriter struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
}

// NewPiJSONWriter wraps an io.Writer.
func NewPiJSONWriter(w io.Writer) *PiJSONWriter {
	enc := json.NewEncoder(w)
	// Same rule as the old writer: HTML escaping off (it would mangle > and
	// & inside tool arguments), U+2028/29 handled by write().
	enc.SetEscapeHTML(false)
	return &PiJSONWriter{w: w, enc: enc}
}

// WriteHeader writes the session header line that opens a JSON-mode stream
// (pi: print-mode.ts:122-127). It is a no-op for a nil header, matching pi's
// `if (header)`.
func (w *PiJSONWriter) WriteHeader(h *session.Header) error {
	if h == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.write(h)
}

// WriteEvent writes one agent event as a single JSON line.
func (w *PiJSONWriter) WriteEvent(ev agent.AgentEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeEventLocked(ev)
}

// WriteObject writes any value as a single JSON line.
//
// This is the RPC response path: responses are not agent events, but they
// share the stream with them, so they go through the same mutex and the
// same U+2028/29 escaping as write() — a half-interleaved line is a stream
// no parser can read, and a protocol that fixed framing in one message kind
// and not the other is a trap (same reasoning as the two writers in this
// package, file comment).
//
// pi: rpc-mode.ts:60-63 — one output() for responses, events and extension
// UI requests alike.
func (w *PiJSONWriter) WriteObject(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.write(v)
}

func (w *PiJSONWriter) writeEventLocked(ev agent.AgentEvent) error {
	switch e := ev.(type) {
	case agent.EvAgentStart:
		return w.write(map[string]any{"type": "agent_start"})

	case agent.EvAgentEnd:
		return w.write(map[string]any{
			"type":     "agent_end",
			"messages": e.Messages,
		})

	case agent.EvTurnStart:
		return w.write(map[string]any{"type": "turn_start"})

	case agent.EvTurnEnd:
		return w.write(map[string]any{
			"type":        "turn_end",
			"message":     e.Message,
			"toolResults": e.ToolResults,
		})

	case agent.EvMessageStart:
		return w.write(map[string]any{
			"type":    "message_start",
			"message": e.Message,
		})

	case agent.EvMessageUpdate:
		return w.writeMessageUpdate(e)

	case agent.EvMessageEnd:
		return w.write(map[string]any{
			"type":    "message_end",
			"message": e.Message,
		})

	case agent.EvToolExecutionStart:
		return w.write(map[string]any{
			"type":       "tool_execution_start",
			"toolCallId": e.ToolCallID,
			"toolName":   e.ToolName,
			"args":       e.Args,
		})

	case agent.EvToolExecutionUpdate:
		return w.write(map[string]any{
			"type":          "tool_execution_update",
			"toolCallId":    e.ToolCallID,
			"toolName":      e.ToolName,
			"args":          e.Args,
			"partialResult": wireResult(e.PartialResult),
		})

	case agent.EvToolExecutionEnd:
		line := map[string]any{
			"type":       "tool_execution_end",
			"toolCallId": e.ToolCallID,
			"toolName":   e.ToolName,
			"result":     wireResult(e.Result),
			"isError":    e.IsError,
		}
		// durationMs is absent, not null, when the tool did not run
		// (pi: agent-loop.ts:926).
		if e.DurationMS != nil {
			line["durationMs"] = *e.DurationMS
		}
		return w.write(line)

	default:
		return fmt.Errorf("transport: unknown agent event %T", ev)
	}
}

// writeMessageUpdate applies the json-event.ts projection: the cumulative
// partial is stripped from the assistant event and the usage moves up to the
// envelope.
func (w *PiJSONWriter) writeMessageUpdate(e agent.EvMessageUpdate) error {
	if e.Message.Role != ai.RoleAssistant {
		// pi throws here (json-event.ts:52-54): a message_update on a
		// non-assistant message is a producer bug, and silently emitting it
		// would hide the bug behind a plausible line.
		return fmt.Errorf("transport: message_update message role is %q, not assistant", e.Message.Role)
	}
	raw, err := ai.MarshalEvent(e.AssistantEvent)
	if err != nil {
		return err
	}
	// MarshalEvent returns a complete JSON object; splicing it in keeps the
	// assistant event's own field order and avoids a decode/encode round
	// trip that could reorder or drop fields.
	return w.writeRaw(fmt.Sprintf(
		`{"type":"message_update","usage":%s,"assistantMessageEvent":%s}`,
		usageJSON(e.Message.Usage), raw))
}

// usageJSON renders the usage object of the envelope. A nil usage becomes
// null: pi's Usage is a required field of the assistant message, so this is
// the "should not happen" shape rather than a designed one.
func usageJSON(u *ai.Usage) string {
	if u == nil {
		return "null"
	}
	data, err := json.Marshal(u)
	if err != nil {
		return "null"
	}
	return string(data)
}

// toolResultWire is the JSON shape of an agent.Result on the wire: pi's
// AgentToolResult (packages/agent/src/types.ts:423-446). Content is required;
// every other field is optional in pi, hence omitempty.
type toolResultWire struct {
	Content           []ai.ContentBlock `json:"content"`
	Details           any               `json:"details,omitempty"`
	StructuredContent any               `json:"structuredContent,omitempty"`
	Usage             *ai.Usage         `json:"usage,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
	Terminate         bool              `json:"terminate,omitempty"`
}

// wireResult projects an agent.Result onto the wire struct. The projection is
// explicit rather than struct tags on agent.Result: that type is a frozen
// contract (P0) whose Go shape is not the wire shape, and the protocol lives
// here.
func wireResult(r agent.Result) toolResultWire {
	return toolResultWire{
		Content:           r.Content,
		Details:           r.Details,
		StructuredContent: r.StructuredContent,
		Usage:             r.Usage,
		IsError:           r.IsError,
		Terminate:         r.Terminate,
	}
}

// write encodes one value per line with the U+2028/29 rule.
func (w *PiJSONWriter) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.writeRaw(string(data))
}

// writeRaw writes one already-encoded JSON object as a line.
//
// U+2028 and U+2029 are valid inside a JSON string but are line terminators
// to a JavaScript reader (and to `jq` on some builds): escaping them keeps
// every message on exactly one line, and the escaped form parses to the same
// string. This is the same rule the old writer applies (ndjson.go:331, plan
// R8) — shared deliberately, because a protocol that fixed it in one dialect
// and not the other would be a trap.
func (w *PiJSONWriter) writeRaw(line string) error {
	if strings.ContainsAny(line, "\u2028\u2029") {
		line = strings.NewReplacer("\u2028", `\u2028`, "\u2029", `\u2029`).Replace(line)
	}
	_, err := io.WriteString(w.w, line+"\n")
	return err
}
