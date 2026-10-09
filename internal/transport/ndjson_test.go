package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ultrac/internal/contract/event"
	"ultrac/internal/contract/provider"
)

// emitAll writes a batch of events, failing the test on the first error.
func emitAll(t *testing.T, w *Writer, events []event.Event) {
	t.Helper()
	for _, ev := range events {
		if err := w.Emit(context.Background(), ev); err != nil {
			t.Fatalf("emit %s: %v", ev.Kind, err)
		}
	}
}

func decodeLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i+1, err, line)
		}
		out = append(out, m)
	}
	return out
}

func TestEveryEventKindIsOneValidJSONLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "sess-1")

	call := provider.ToolCall{ID: "t1", Name: "Read", Input: json.RawMessage(`{"file_path":"/a"}`)}
	events := []event.Event{
		{Kind: event.KindMessageStart},
		event.Text("hello"),
		event.Thinking("hmm"),
		event.ToolCall(event.ToolUse{ID: call.ID, Name: call.Name, Input: call.Input}),
		event.ToolProgress(event.Progress{ToolUseID: "t1", Message: "reading"}),
		event.ToolOutput(event.ToolResult{ToolUseID: "t1", Name: "Read", Content: "file body"}),
		event.UsageEvent(provider.Usage{InputTokens: 3, OutputTokens: 4}),
		event.StopEvent(provider.StopEndTurn),
		event.NoticeEvent(event.Notice{Code: "n1", Message: "a note"}),
		event.FailureEvent(event.Failure{Code: "boom", Message: "it broke", Fatal: true}),
	}
	emitAll(t, w, events)

	lines := decodeLines(t, buf.String())
	if len(lines) != len(events)+1 {
		// message_stop produces two lines (message_delta + message_stop).
		t.Fatalf("got %d lines for %d events", len(lines), len(events))
	}
	for _, l := range lines {
		if l["type"] == nil {
			t.Errorf("a line has no type discriminator: %v", l)
		}
		if l["session_id"] != "sess-1" {
			t.Errorf("session_id = %v", l["session_id"])
		}
	}
}

func TestToolUseAndResultShapes(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s")
	_ = w.Emit(context.Background(), event.ToolCall(event.ToolUse{
		ID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`),
	}))
	_ = w.Emit(context.Background(), event.ToolOutput(event.ToolResult{
		ToolUseID: "t1", Name: "Bash", Content: "out", IsError: true,
	}))

	lines := decodeLines(t, buf.String())
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	use := lines[0]
	if use["type"] != "tool_use" || use["tool_use_id"] != "t1" || use["name"] != "Bash" {
		t.Errorf("tool_use line = %v", use)
	}
	// The input must be an object, not a JSON string: a reader parsing it as a
	// string gets a value that looks like JSON and is not.
	if _, ok := use["input"].(map[string]any); !ok {
		t.Errorf("tool_use input is not an object: %#v", use["input"])
	}

	res := lines[1]
	if res["type"] != "tool_result" || res["is_error"] != true || res["content"] != "out" {
		t.Errorf("tool_result line = %v", res)
	}
}

func TestEmptyInputBecomesAnObject(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s")
	_ = w.Emit(context.Background(), event.ToolCall(event.ToolUse{ID: "t", Name: "Glob"}))
	lines := decodeLines(t, buf.String())
	if _, ok := lines[0]["input"].(map[string]any); !ok {
		t.Errorf("empty input = %#v, want an empty object", lines[0]["input"])
	}
}

// TestLineSeparatorsAreEscaped is the ndjsonSafeStringify property: a model
// that emits U+2028 in prose must not split its own message in two for a
// JavaScript reader.
func TestLineSeparatorsAreEscaped(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s")
	_ = w.Emit(context.Background(), event.Text("before\u2028after\u2029end"))

	raw := buf.String()
	if strings.Count(raw, "\n") != 1 {
		t.Fatalf("output spans more than one line:\n%q", raw)
	}
	if !strings.Contains(raw, `\u2028`) || !strings.Contains(raw, `\u2029`) {
		t.Errorf("separators were not escaped: %q", raw)
	}
	// Escaped form must still parse to the original string.
	lines := decodeLines(t, raw)
	if lines[0]["event"].(map[string]any)["delta"].(map[string]any)["text"] != "before\u2028after\u2029end" {
		t.Errorf("text did not round-trip: %v", lines[0])
	}
}

func TestInitMessage(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "sess-2")
	w.SetInit(InitInfo{
		CWD: "/work", Model: "m", Tools: []string{"Read"}, PermissionMode: "default",
		Version: "1.2.3", APIKeySource: "ANTHROPIC_API_KEY",
	})
	if err := w.Init(); err != nil {
		t.Fatal(err)
	}
	lines := decodeLines(t, buf.String())
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	init := lines[0]
	if init["type"] != "system" || init["subtype"] != "init" {
		t.Errorf("init line = %v", init)
	}
	if init["protocol_version"] != ProtocolVersion {
		t.Errorf("protocol_version = %v", init["protocol_version"])
	}
	if init["cwd"] != "/work" || init["model"] != "m" {
		t.Errorf("init payload = %v", init)
	}
}

func TestInitWithoutInfoWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s")
	if err := w.Init(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("init wrote %q with no payload set", buf.String())
	}
}

func TestBlockIndexAdvancesPerBlockNotPerDelta(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s")
	_ = w.Emit(context.Background(), event.Event{Kind: event.KindMessageStart})
	_ = w.Emit(context.Background(), event.Text("a"))
	_ = w.Emit(context.Background(), event.Text("b"))
	_ = w.Emit(context.Background(), event.Text("c"))
	_ = w.Emit(context.Background(), event.Thinking("t"))
	_ = w.Emit(context.Background(), event.Text("d"))

	lines := decodeLines(t, buf.String())
	var indexes []float64
	for _, l := range lines {
		inner, ok := l["event"].(map[string]any)
		if !ok {
			continue
		}
		if inner["type"] != "content_block_delta" {
			continue
		}
		indexes = append(indexes, inner["index"].(float64))
	}
	want := []float64{0, 0, 0, 1, 2} // text, text, text, thinking, text
	if len(indexes) != len(want) {
		t.Fatalf("indexes = %v, want %v", indexes, want)
	}
	for i := range want {
		if indexes[i] != want[i] {
			t.Fatalf("indexes = %v, want %v", indexes, want)
		}
	}
}

func TestStopProducesMessageDeltaThenStop(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s")
	_ = w.Emit(context.Background(), event.StopEvent(provider.StopToolUse))
	lines := decodeLines(t, buf.String())
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	delta := lines[0]["event"].(map[string]any)
	if delta["type"] != "message_delta" {
		t.Errorf("first line = %v", delta)
	}
	if delta["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", delta["delta"])
	}
	if lines[1]["event"].(map[string]any)["type"] != "message_stop" {
		t.Errorf("second line = %v", lines[1])
	}
}

func TestResultMessage(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "sess-3")
	if err := w.Result(ResultPayload{
		Subtype:    "success",
		NumTurns:   2,
		Result:     "the answer",
		StopReason: "end_turn",
		DurationMS: 1234,
		Errors:     nil,
	}); err != nil {
		t.Fatal(err)
	}
	lines := decodeLines(t, buf.String())
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	r := lines[0]
	if r["type"] != "result" || r["subtype"] != "success" {
		t.Errorf("result line = %v", r)
	}
	if r["num_turns"].(float64) != 2 || r["result"] != "the answer" {
		t.Errorf("result payload = %v", r)
	}
	if _, hasErrors := r["errors"]; hasErrors {
		t.Errorf("an empty error list was emitted: %v", r)
	}
	// The SDK shape includes these fields; a reader may index them without
	// checking.
	for _, key := range []string{"modelUsage", "permission_denials", "usage", "total_cost_usd", "is_error", "duration_api_ms"} {
		if _, ok := r[key]; !ok {
			t.Errorf("result is missing %q", key)
		}
	}
}

func TestConcurrentEmitDoesNotInterleaveLines(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s")
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				_ = w.Emit(context.Background(), event.Text("some text"))
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	lines := decodeLines(t, buf.String())
	if len(lines) != 400 {
		t.Fatalf("lines = %d, want 400", len(lines))
	}
}
