// Package transport carries engine events to a frontend.
//
// # The protocol
//
// One JSON object per line, on stdout, each carrying a `type` discriminator.
// This is the same shape the TypeScript implementation writes for
// --output-format=stream-json, so a parser written for one can read the other
// with a small adapter rather than a rewrite.
//
// Line-delimited rather than length-prefixed or framed, for three reasons that
// all showed up in practice on the TypeScript side: a truncated final line is
// recoverable while a truncated frame is not, `tail -f` shows progress, and a
// human can read it. Those are the properties that make a protocol debuggable.
//
// # Message types
//
//	system       subtype=init, once, before any turn
//	stream_event an anthropic-shaped streaming event (message_start, deltas, message_stop)
//	tool_use     a complete tool invocation, announced before it runs
//	tool_result  the outcome of one invocation
//	tool_progress a running tool reporting in
//	usage        token accounting for the request that finished
//	notice       host-authored, non-fatal information
//	failure      a failure the caller may need to handle
//	result       terminal summary, exactly one per run
//
// The set is a superset of the TypeScript SDK's: system/stream_event/result
// match, while tool_use, tool_result, tool_progress, notice and failure are
// our own and carry the same information the SDK spreads across `assistant`
// and `user` messages. Additive types are the safe direction — a reader that
// does not know one can skip it by line, which a reader facing a changed shape
// inside a known type cannot do.
//
// # Layering
//
// Part of the interface layer (L0). It imports the event contract and nothing
// from the engine, so a frontend can be built and tested against the protocol
// without linking the agent loop.
package transport

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"ultrac/internal/contract/event"
	"ultrac/internal/session"
)

// ProtocolVersion identifies the wire protocol.
//
// It is sent in the init message. A reader that sees a version it does not
// know should say so rather than guess: a protocol that changes silently is
// how a frontend ends up rendering tool results as assistant text.
const ProtocolVersion = "1"

// InitInfo is the payload of the init message.
type InitInfo struct {
	CWD            string   `json:"cwd"`
	Model          string   `json:"model"`
	Tools          []string `json:"tools"`
	PermissionMode string   `json:"permissionMode"`
	Version        string   `json:"claude_code_version"`
	APIKeySource   string   `json:"apiKeySource"`
}

// Writer serialises events as NDJSON.
//
// It is safe for concurrent use, because tool progress arrives from worker
// goroutines and interleaving half a line with half of another produces a
// stream no parser can read.
type Writer struct {
	mu   sync.Mutex
	w    io.Writer
	enc  *json.Encoder
	init *InitInfo

	sessionID string
	// blockIndex and blockKind track which content block the streamed deltas
	// belong to. The index advances once per block, not once per delta:
	// readers key blocks by index, and a fresh index for every token would
	// make a single paragraph look like two hundred blocks.
	blockIndex int
	blockKind  string
}

// NewWriter wraps an io.Writer.
func NewWriter(w io.Writer, sessionID string) *Writer {
	enc := json.NewEncoder(w)
	// The encoder's own escaping leaves U+2028 and U+2029 raw, which a
	// JavaScript reader treats as a line terminator and would therefore split
	// mid-string. SetEscapeHTML(false) plus the manual pass in write() keeps
	// each message on exactly one line.
	enc.SetEscapeHTML(false)
	return &Writer{w: w, enc: enc, sessionID: sessionID, blockIndex: -1}
}

// nextBlock advances to a new content block when the kind changes.
func (w *Writer) nextBlock(kind string) int {
	if w.blockKind != kind {
		w.blockIndex++
		w.blockKind = kind
	}
	return w.blockIndex
}

// SetInit records the init payload, emitted by Init.
func (w *Writer) SetInit(info InitInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.init = &info
}

// Init writes the one-time init message.
//
// It comes before any turn so a frontend can render its header — the working
// directory, the model, the tool list — without waiting for the first token.
func (w *Writer) Init() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.init == nil {
		return nil
	}
	return w.write(map[string]any{
		"type":                "system",
		"subtype":             "init",
		"protocol_version":    ProtocolVersion,
		"cwd":                 w.init.CWD,
		"model":               w.init.Model,
		"tools":               w.init.Tools,
		"permissionMode":      w.init.PermissionMode,
		"claude_code_version": w.init.Version,
		"apiKeySource":        w.init.APIKeySource,
		"session_id":          w.sessionID,
	})
}

// Emit writes one event. It satisfies event.Sink.
func (w *Writer) Emit(_ context.Context, ev event.Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.emitLocked(ev)
}

func (w *Writer) emitLocked(ev event.Event) error {
	switch ev.Kind {
	case event.KindMessageStart:
		// Reset to -1 so the first block of the message is index 0.
		w.blockIndex, w.blockKind = -1, ""
		return w.streamEvent(map[string]any{"type": "message_start", "message": map[string]any{
			"role": "assistant",
		}})

	case event.KindTextDelta:
		return w.streamEvent(map[string]any{
			"type":  "content_block_delta",
			"index": w.nextBlock("text"),
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})

	case event.KindThinkingDelta:
		return w.streamEvent(map[string]any{
			"type":  "content_block_delta",
			"index": w.nextBlock("thinking"),
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
		})

	case event.KindToolUse:
		if ev.Tool == nil {
			return nil
		}
		return w.write(map[string]any{
			"type":        "tool_use",
			"tool_use_id": ev.Tool.ID,
			"name":        ev.Tool.Name,
			"input":       rawOrObject(ev.Tool.Input),
			"uuid":        session.NewUUID(),
			"session_id":  w.sessionID,
		})

	case event.KindToolResult:
		if ev.Result == nil {
			return nil
		}
		return w.write(map[string]any{
			"type":        "tool_result",
			"tool_use_id": ev.Result.ToolUseID,
			"name":        ev.Result.Name,
			"content":     ev.Result.Content,
			"is_error":    ev.Result.IsError,
			"uuid":        session.NewUUID(),
			"session_id":  w.sessionID,
		})

	case event.KindToolProgress:
		if ev.Progress == nil {
			return nil
		}
		return w.write(map[string]any{
			"type":        "tool_progress",
			"tool_use_id": ev.Progress.ToolUseID,
			"message":     ev.Progress.Message,
			"fraction":    ev.Progress.Fraction,
			"uuid":        session.NewUUID(),
			"session_id":  w.sessionID,
		})

	case event.KindUsage:
		if ev.Usage == nil {
			return nil
		}
		return w.write(map[string]any{
			"type":       "usage",
			"usage":      ev.Usage,
			"uuid":       session.NewUUID(),
			"session_id": w.sessionID,
		})

	case event.KindMessageStop:
		if err := w.streamEvent(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": string(ev.Stop)},
		}); err != nil {
			return err
		}
		return w.streamEvent(map[string]any{"type": "message_stop"})

	case event.KindNotice:
		if ev.Notice == nil {
			return nil
		}
		return w.write(map[string]any{
			"type":       "notice",
			"code":       ev.Notice.Code,
			"message":    ev.Notice.Message,
			"uuid":       session.NewUUID(),
			"session_id": w.sessionID,
		})

	case event.KindFailure:
		if ev.Failure == nil {
			return nil
		}
		return w.write(map[string]any{
			"type":       "failure",
			"code":       ev.Failure.Code,
			"message":    ev.Failure.Message,
			"fatal":      ev.Failure.Fatal,
			"uuid":       session.NewUUID(),
			"session_id": w.sessionID,
		})
	}
	return nil
}

func (w *Writer) streamEvent(inner map[string]any) error {
	return w.write(map[string]any{
		"type":               "stream_event",
		"event":              inner,
		"parent_tool_use_id": nil,
		"uuid":               session.NewUUID(),
		"session_id":         w.sessionID,
	})
}

// Result writes the terminal summary.
//
// It is written from outside the event stream because it is only known once
// the stream has ended — the sink that reads the events is what triggers it.
func (w *Writer) Result(r ResultPayload) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	payload := map[string]any{
		"type":               "result",
		"subtype":            r.Subtype,
		"is_error":           r.IsError,
		"duration_ms":        r.DurationMS,
		"duration_api_ms":    r.DurationAPIMS,
		"num_turns":          r.NumTurns,
		"result":             r.Result,
		"stop_reason":        nullable(r.StopReason),
		"usage":              r.Usage,
		"total_cost_usd":     r.TotalCostUSD,
		"permission_denials": r.PermissionDenials,
		"modelUsage":         map[string]any{},
		"uuid":               session.NewUUID(),
		"session_id":         w.sessionID,
	}
	if len(r.Errors) > 0 {
		payload["errors"] = r.Errors
	}
	return w.write(payload)
}

// ResultPayload is the terminal summary.
type ResultPayload struct {
	Subtype           string
	IsError           bool
	DurationMS        int64
	DurationAPIMS     int64
	NumTurns          int
	Result            string
	StopReason        string
	Usage             any
	TotalCostUSD      float64
	PermissionDenials []any
	Errors            []string
}

// write encodes one line.
func (w *Writer) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// U+2028 and U+2029 are valid inside a JSON string but are line
	// terminators to a JavaScript reader. Escaping them keeps the message on
	// one line for any reader, and the escaped form parses to the same string.
	line := escapeLineTerminators(data)
	_, err = w.w.Write(append(line, '\n'))
	return err
}

// escapeLineTerminators is ndjsonSafeStringify's rule, applied to bytes.
//
// The TypeScript version is four lines of regexp on the string; the same
// substitution has to happen here or a model that emits U+2028 in prose would
// split its own message in two.
func escapeLineTerminators(data []byte) []byte {
	if !strings.ContainsAny(string(data), "\u2028\u2029") {
		return data
	}
	s := strings.ReplaceAll(string(data), "\u2028", `\u2028`)
	s = strings.ReplaceAll(s, "\u2029", `\u2029`)
	return []byte(s)
}

// rawOrObject embeds a raw JSON value rather than a base64 string.
//
// json.RawMessage already round-trips, but an empty one would emit nothing at
// all, and a reader expecting `input` would then see the field missing rather
// than empty.
func rawOrObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage("{}")
	}
	return raw
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
