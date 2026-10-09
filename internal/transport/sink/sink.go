// Package sink holds the minimal output surfaces.
//
// # Why more than one
//
// The engine produces events, not text, precisely so that what a surface does
// with them can vary. Three surfaces cover everything T1 needs: a human
// reading a terminal, a program reading NDJSON, and a test that wants the
// events without the output. They live together because they are three
// answers to one question, and splitting them across packages would make the
// question harder to see.
//
// # Layering
//
// Part of the interface layer (L0). It imports the event contract and the
// transport protocol, never the engine.
package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"ultrac/internal/contract/event"
	"ultrac/internal/transport"
)

// Discard drops every event. It exists so a caller that only wants the
// summary does not have to nil-check a sink.
type Discard struct{}

// Emit implements event.Sink.
func (Discard) Emit(context.Context, event.Event) error { return nil }

// Text renders events for a human at a terminal.
//
// Assistant text goes to Out and everything else to Meta, so `ultrac -p ... >
// answer.md` produces a clean file: progress notes and tool activity are
// commentary, and mixing them into the answer is how a redirect produces a
// document with "[Bash] ls" in the middle of it.
type Text struct {
	Out  io.Writer
	Meta io.Writer

	// ShowToolActivity prints one line per tool call and result.
	ShowToolActivity bool
	// ShowUsage prints token accounting at the end of each request.
	ShowUsage bool

	mu          sync.Mutex
	atLineStart bool
	lastDelta   string
}

// NewText builds a Text sink. A nil Meta writer discards commentary.
func NewText(out, meta io.Writer) *Text {
	return &Text{Out: out, Meta: meta, ShowToolActivity: true, atLineStart: true}
}

// Emit implements event.Sink.
func (t *Text) Emit(_ context.Context, ev event.Event) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch ev.Kind {
	case event.KindTextDelta:
		if ev.Text == "" {
			return nil
		}
		t.writeOut(ev.Text)
		t.lastDelta = ev.Text

	case event.KindThinkingDelta:
		// Thinking is not part of the answer, so it goes to Meta dimmed rather
		// than into the answer a redirect is capturing.
		if ev.Text != "" {
			t.writeMeta(ev.Text)
		}

	case event.KindMessageStart:
		t.lastDelta = ""

	case event.KindToolUse:
		if ev.Tool == nil || !t.ShowToolActivity {
			return nil
		}
		t.endLineOut()
		t.line("  → " + ev.Tool.Name + " " + summarizeInput(ev.Tool.Input))

	case event.KindToolResult:
		if ev.Result == nil || !t.ShowToolActivity {
			return nil
		}
		mark := "  ✓ "
		if ev.Result.IsError {
			mark = "  ✗ "
		}
		t.line(mark + ev.Result.Name + " " + summarizeResult(ev.Result.Content))

	case event.KindToolProgress:
		if ev.Progress == nil || ev.Progress.Message == "" || !t.ShowToolActivity {
			return nil
		}
		t.line("  … " + ev.Progress.Message)

	case event.KindUsage:
		if ev.Usage == nil || !t.ShowUsage {
			return nil
		}
		t.line(fmt.Sprintf("  [tokens: in=%d out=%d cache_read=%d]",
			ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CacheReadTokens))

	case event.KindMessageStop:
		t.endLineOut()

	case event.KindNotice:
		if ev.Notice == nil {
			return nil
		}
		t.line("  note: " + ev.Notice.Message)

	case event.KindFailure:
		if ev.Failure == nil {
			return nil
		}
		t.endLineOut()
		t.line("  error [" + ev.Failure.Code + "]: " + ev.Failure.Message)
	}
	return nil
}

// Finish closes any dangling output line.
func (t *Text) Finish() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endLineOut()
	return nil
}

func (t *Text) writeOut(s string) {
	if t.Out == nil {
		return
	}
	_, _ = io.WriteString(t.Out, s)
	t.atLineStart = strings.HasSuffix(s, "\n")
}

func (t *Text) endLineOut() {
	if t.Out == nil || t.atLineStart {
		return
	}
	_, _ = io.WriteString(t.Out, "\n")
	t.atLineStart = true
}

// line writes one commentary line, flushing the answer first so the two do not
// interleave mid-word.
func (t *Text) line(s string) {
	if t.Meta == nil {
		return
	}
	// A commentary line must not land in the middle of a partially written
	// answer line: the reader would see the answer split by it.
	if !t.atLineStart && t.Out != nil {
		_, _ = io.WriteString(t.Out, "\n")
		t.atLineStart = true
	}
	_, _ = io.WriteString(t.Meta, s+"\n")
}

func (t *Text) writeMeta(s string) {
	if t.Meta == nil {
		return
	}
	_, _ = io.WriteString(t.Meta, s)
}

// summarizeInput renders a tool call's input compactly.
//
// The full JSON is already in the NDJSON stream for a program; a human wants
// the one argument that identifies the call.
func summarizeInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return truncateLine(string(raw), 120)
	}
	for _, key := range []string{"file_path", "command", "pattern", "url", "query", "description", "task_id"} {
		if v, ok := m[key]; ok {
			return truncateLine(key+"="+fmt.Sprint(v), 160)
		}
	}
	return truncateLine(string(raw), 120)
}

func summarizeResult(content string) string {
	oneLine := strings.ReplaceAll(content, "\n", " ⏎ ")
	return truncateLine(oneLine, 160)
}

func truncateLine(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// NDJSON is the machine-readable surface.
type NDJSON struct{ W *transport.Writer }

// Emit implements event.Sink.
func (n NDJSON) Emit(ctx context.Context, ev event.Event) error {
	return n.W.Emit(ctx, ev)
}

// JSON collects the run and prints a single object at the end.
//
// It buffers rather than streams because that is the point of the format: a
// caller that asked for one JSON document wants one JSON document, not a log.
type JSON struct {
	Out io.Writer

	mu   sync.Mutex
	text strings.Builder
	// toolCalls and results are kept so the object says what happened, not
	// only what was said.
	ToolCalls []map[string]any
	Notices   []map[string]any
	Failures  []map[string]any
	Usage     map[string]int
}

// Emit implements event.Sink.
func (j *JSON) Emit(_ context.Context, ev event.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch ev.Kind {
	case event.KindTextDelta:
		j.text.WriteString(ev.Text)
	case event.KindToolUse:
		if ev.Tool != nil {
			j.ToolCalls = append(j.ToolCalls, map[string]any{
				"id": ev.Tool.ID, "name": ev.Tool.Name, "input": rawOrObject(ev.Tool.Input),
			})
		}
	case event.KindUsage:
		if ev.Usage != nil {
			if j.Usage == nil {
				j.Usage = map[string]int{}
			}
			j.Usage["input_tokens"] += ev.Usage.InputTokens
			j.Usage["output_tokens"] += ev.Usage.OutputTokens
		}
	case event.KindNotice:
		if ev.Notice != nil {
			j.Notices = append(j.Notices, map[string]any{"code": ev.Notice.Code, "message": ev.Notice.Message})
		}
	case event.KindFailure:
		if ev.Failure != nil {
			j.Failures = append(j.Failures, map[string]any{
				"code": ev.Failure.Code, "message": ev.Failure.Message, "fatal": ev.Failure.Fatal,
			})
		}
	}
	return nil
}

// Text returns the accumulated assistant text.
func (j *JSON) Text() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.text.String()
}

// Write emits the final document.
func (j *JSON) Write(extra map[string]any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Out == nil {
		return nil
	}
	doc := map[string]any{
		"type":       "result",
		"result":     j.text.String(),
		"tool_calls": j.ToolCalls,
	}
	if len(j.Notices) > 0 {
		doc["notices"] = j.Notices
	}
	if len(j.Failures) > 0 {
		doc["failures"] = j.Failures
	}
	if j.Usage != nil {
		doc["usage"] = j.Usage
	}
	for k, v := range extra {
		doc[k] = v
	}
	enc := json.NewEncoder(j.Out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// Multi fans out to several sinks, stopping at the first error.
type Multi []event.Sink

// Emit implements event.Sink.
func (m Multi) Emit(ctx context.Context, ev event.Event) error {
	for _, s := range m {
		if s == nil {
			continue
		}
		if err := s.Emit(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func rawOrObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage("{}")
	}
	return raw
}
