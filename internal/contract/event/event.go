// Package event defines the typed event stream the engine emits.
//
// # Why this package exists
//
// The TypeScript implementation wrote ANSI escape codes straight to the
// terminal from inside the agent loop. That is what made the TUI
// inseparable from the engine: there was no value you could hand to a second
// frontend. Here the loop only produces values, and a frontend decides how to
// render them.
//
// # Layering
//
// Part of the contract layer. It is imported from every layer above (engine,
// tools, frontends) and imports only its contract peer, provider, for the
// vocabulary the model already speaks (Usage, StopReason). Nothing in this
// package may import the engine, the tools, or a frontend.
package event

import (
	"context"
	"encoding/json"
	"iter"

	"ultrac/internal/contract/provider"
)

// Kind discriminates an Event.
//
// These values are stable wire identifiers: they are what the NDJSON stream
// carries between the Go engine and the TypeScript shell. Renaming one is a
// protocol break, not a refactor.
type Kind string

const (
	KindMessageStart  Kind = "message_start"
	KindTextDelta     Kind = "text_delta"
	KindThinkingDelta Kind = "thinking_delta"
	KindToolUse       Kind = "tool_use"
	KindToolProgress  Kind = "tool_progress"
	KindToolResult    Kind = "tool_result"
	KindUsage         Kind = "usage"
	KindMessageStop   Kind = "message_stop"
	KindNotice        Kind = "notice"
	KindFailure       Kind = "failure"
)

// ToolUse is a fully assembled tool invocation.
//
// The provider accumulates streaming deltas by index and only hands over
// complete calls, so Input is always parseable JSON — callers never have to
// stitch fragments together themselves.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolResult is what a tool handed back.
//
// IsError is a normal outcome, not a transport failure. The engine feeds it
// to the model as a tool_result block so the model can correct itself; it does
// not abort the turn.
type ToolResult struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
}

// Progress is a running tool reporting partial progress.
type Progress struct {
	ToolUseID string  `json:"toolUseId"`
	Message   string  `json:"message,omitempty"`
	Fraction  float64 `json:"fraction,omitempty"`
}

// Notice is host-authored, non-fatal information for the user.
type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Failure carries an identity, never a sentence.
//
// Callers must switch on Code. A caller that has to tell two failures apart
// gets a code it can compare, because matching on Message is exactly what
// breaks the moment the wording changes.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Fatal reports whether the turn ended because of this failure.
	Fatal bool `json:"fatal,omitempty"`
}

// Failure codes. These are wire values: a caller compares against them, so
// renaming one is a protocol break.
const (
	// CodeProviderError means the model endpoint failed or refused.
	CodeProviderError = "provider_error"
	// CodeNoProvider means no model was configured at all.
	CodeNoProvider = "no_provider"
	// CodeMaxTurns means the loop stopped at its turn limit.
	CodeMaxTurns = "max_turns"
	// CodeMaxTokens means the model hit its output ceiling.
	CodeMaxTokens = "max_tokens"
	// CodeTruncated means the stream ended before the model finished.
	CodeTruncated = "truncated_stream"
	// CodeCancelled means the caller cancelled.
	CodeCancelled = "cancelled"
	// CodePermissionDenied means policy refused a tool call.
	CodePermissionDenied = "permission_denied"
	// CodeUnknownTool means the model called a tool that is not registered.
	CodeUnknownTool = "unknown_tool"
	// CodeToolFailed means a tool returned a Go error, not a tool-level
	// failure. A tool that merely failed is a ToolResult with IsError.
	CodeToolFailed = "tool_failed"
	// CodeNoPrompt means the request had no user message.
	CodeNoPrompt = "empty_prompt"
)

// Event is one step of the stream.
//
// It is a single struct with a discriminator rather than an interface so that
// it maps to one JSON object on the wire without a custom marshaller.
type Event struct {
	Kind Kind `json:"kind"`

	Text string `json:"text,omitempty"`

	Tool     *ToolUse            `json:"tool,omitempty"`
	Result   *ToolResult         `json:"result,omitempty"`
	Progress *Progress           `json:"progress,omitempty"`
	Usage    *provider.Usage     `json:"usage,omitempty"`
	Notice   *Notice             `json:"notice,omitempty"`
	Failure  *Failure            `json:"failure,omitempty"`
	Stop     provider.StopReason `json:"stop,omitempty"`
}

// Text is an assistant text delta.
func Text(s string) Event { return Event{Kind: KindTextDelta, Text: s} }

// Thinking is an assistant reasoning delta.
func Thinking(s string) Event { return Event{Kind: KindThinkingDelta, Text: s} }

// ToolCall announces that a tool is about to run.
func ToolCall(u ToolUse) Event { return Event{Kind: KindToolUse, Tool: &u} }

// ToolOutput reports a finished tool.
func ToolOutput(r ToolResult) Event { return Event{Kind: KindToolResult, Result: &r} }

// ToolProgress reports a tool that is still running.
func ToolProgress(p Progress) Event { return Event{Kind: KindToolProgress, Progress: &p} }

// UsageEvent reports token accounting for the request that just finished.
func UsageEvent(u provider.Usage) Event { return Event{Kind: KindUsage, Usage: &u} }

// StopEvent reports why the model stopped generating.
func StopEvent(r provider.StopReason) Event { return Event{Kind: KindMessageStop, Stop: r} }

// NoticeEvent reports host-authored information.
func NoticeEvent(n Notice) Event { return Event{Kind: KindNotice, Notice: &n} }

// FailureEvent reports a failure the caller may need to handle.
func FailureEvent(f Failure) Event { return Event{Kind: KindFailure, Failure: &f} }

// Seq is the engine's output shape.
//
// It mirrors the TypeScript `async function* query(...)` one-to-one: lazy,
// cancellable by the consumer stopping the loop, and it carries a value or an
// error rather than doing both silently. A generator that returns early must
// not leak a goroutine, which is why the producer checks yield's return value.
type Seq = iter.Seq2[Event, error]

// Sink is a frontend.
//
// The terminal UI, the NDJSON transport, the ACP adapter and the desktop shell
// all implement it. The engine never learns which one is attached, which is
// what lets one core serve four surfaces.
type Sink interface {
	Emit(ctx context.Context, ev Event) error
}

// Drain forwards every event from seq to sink and stops at the first error,
// whether that error came from the engine or from the sink.
func Drain(ctx context.Context, seq Seq, sink Sink) error {
	for ev, err := range seq {
		if err != nil {
			return err
		}
		if err := sink.Emit(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}
