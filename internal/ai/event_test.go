package ai

// C2 comparison tests: the 12-event streaming protocol and its wire shape.
//
// Expected literals are transcribed from pi's AssistantMessageEvent union
// (packages/ai/src/types.ts:801-817) and the json-event.ts projection
// (packages/coding-agent/src/modes/json-event.ts:20-61).

import (
	"encoding/json"
	"testing"
	"time"
)

// TestAssistantEventNames pins the 12 variants in pi's union order.
//
// pi: packages/ai/src/types.ts:801-817.
func TestAssistantEventNames(t *testing.T) {
	events := []AssistantEvent{
		EvStart{}, EvTextStart{}, EvTextDelta{}, EvTextEnd{},
		EvThinkingStart{}, EvThinkingDelta{}, EvThinkingEnd{},
		EvToolCallStart{}, EvToolCallDelta{}, EvToolCallEnd{},
		EvDone{}, EvError{},
	}
	want := []string{
		"start",
		"text_start", "text_delta", "text_end",
		"thinking_start", "thinking_delta", "thinking_end",
		"toolcall_start", "toolcall_delta", "toolcall_end",
		"done", "error",
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i, ev := range events {
		if got := Name(ev); got != want[i] {
			t.Errorf("event %d: got %q, want %q", i, got, want[i])
		}
	}
}

// TestEventReasonEnums pins the reason subsets: done terminates with
// stop|length|toolUse|deferred, error with aborted|error.
//
// pi: packages/ai/src/types.ts:812-817.
func TestEventReasonEnums(t *testing.T) {
	doneReasons := []StopReason{StopStop, StopLength, StopToolUse, StopDeferred}
	for _, r := range doneReasons {
		if r == StopError || r == StopAborted || r == StopPending {
			t.Errorf("done reason %q is not in pi's done set", r)
		}
	}
	errReasons := []StopReason{StopAborted, StopError}
	for _, r := range errReasons {
		if r == StopStop || r == StopLength || r == StopToolUse {
			t.Errorf("error reason %q is not in pi's error set", r)
		}
	}
}

// TestMarshalEventWireShape pins the wire projection: type first, partial
// stripped, toolcall_start carrying the call's id and toolName.
//
// pi: packages/coding-agent/src/modes/json-event.ts:20-61.
func TestMarshalEventWireShape(t *testing.T) {
	partial := Message{
		Role: RoleAssistant,
		Content: []ContentBlock{
			TextBlock("hi"),
			ToolCallBlock("tu_1", "read", map[string]any{"path": "a.go"}),
		},
	}
	cases := []struct {
		name string
		ev   AssistantEvent
		want string
	}{{
		name: "text_delta",
		ev:   EvTextDelta{ContentIndex: 1, Delta: "hi", Partial: partial},
		want: `{"type":"text_delta","contentIndex":1,"delta":"hi"}`,
	}, {
		name: "text_end",
		ev:   EvTextEnd{ContentIndex: 0, Content: "hello", Partial: partial},
		want: `{"type":"text_end","contentIndex":0,"content":"hello"}`,
	}, {
		name: "thinking_delta",
		ev:   EvThinkingDelta{ContentIndex: 0, Delta: "reason", Partial: partial},
		want: `{"type":"thinking_delta","contentIndex":0,"delta":"reason"}`,
	}, {
		name: "toolcall_start carries identity from the partial",
		ev:   EvToolCallStart{ContentIndex: 1, Partial: partial},
		want: `{"type":"toolcall_start","contentIndex":1,"id":"tu_1","toolName":"read"}`,
	}, {
		name: "toolcall_delta",
		ev:   EvToolCallDelta{ContentIndex: 1, Delta: `{"path":`, Partial: partial},
		want: `{"type":"toolcall_delta","contentIndex":1,"delta":"{\"path\":"}`,
	}, {
		name: "toolcall_end carries the complete call",
		ev:   EvToolCallEnd{ContentIndex: 1, ToolCall: ToolCallBlock("tu_1", "read", map[string]any{"path": "a.go"}), Partial: partial},
		want: `{"type":"toolcall_end","contentIndex":1,"toolCall":{"type":"toolCall","id":"tu_1","name":"read","arguments":{"path":"a.go"}}}`,
	}, {
		name: "done carries reason and final message",
		ev: EvDone{Reason: StopToolUse, Message: Message{
			Role: RoleAssistant, Content: []ContentBlock{TextBlock("done")},
			API: "anthropic-messages", Provider: "anthropic", Model: "m",
			Usage: &Usage{TotalTokens: 1}, StopReason: StopToolUse,
		}},
		want: `{"type":"done","reason":"toolUse","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"api":"anthropic-messages","provider":"anthropic","model":"m","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":1,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":0}}`,
	}, {
		name: "error carries reason and error message",
		// AssistantMessage.content is a required array; pi's own fake
		// assistant messages carry [] (packages/ai/src/providers/faux.ts).
		ev: EvError{Reason: StopAborted, Error: Message{
			Role: RoleAssistant, API: "anthropic-messages", Provider: "anthropic", Model: "m",
			Usage: &Usage{}, StopReason: StopAborted, ErrorMessage: "cancelled",
		}},
		want: `{"type":"error","reason":"aborted","error":{"role":"assistant","content":[],"api":"anthropic-messages","provider":"anthropic","model":"m","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"aborted","errorMessage":"cancelled","timestamp":0}}`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MarshalEvent(tc.ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("wire mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestMarshalEventRejectsBrokenToolCallStart pins the producer invariant pi
// enforces by throwing: a toolcall_start whose partial does not carry a
// toolCall at contentIndex is a bug, not a shape to silently encode.
//
// pi: packages/coding-agent/src/modes/json-event.ts:24-29.
func TestMarshalEventRejectsBrokenToolCallStart(t *testing.T) {
	bad := []AssistantEvent{
		EvToolCallStart{ContentIndex: 5, Partial: Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock("hi")}}},
		EvToolCallStart{ContentIndex: 0, Partial: Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock("hi")}}},
		EvToolCallStart{ContentIndex: -1, Partial: Message{Role: RoleAssistant, Content: []ContentBlock{ToolCallBlock("tu_1", "read", nil)}}},
	}
	for _, ev := range bad {
		if _, err := MarshalEvent(ev); err == nil {
			t.Errorf("%+v: MarshalEvent must reject a toolcall_start whose partial is not a tool call at the index", ev)
		}
	}
}

// TestUnmarshalEventRoundTrip checks the wire form decodes back to an
// equivalent event. `partial` is not on the wire, so only the decoded fields
// are compared.
func TestUnmarshalEventRoundTrip(t *testing.T) {
	wire := []string{
		`{"type":"text_start","contentIndex":0}`,
		`{"type":"text_delta","contentIndex":0,"delta":"hi"}`,
		`{"type":"text_end","contentIndex":0,"content":"hi"}`,
		`{"type":"thinking_start","contentIndex":0}`,
		`{"type":"thinking_delta","contentIndex":0,"delta":"r"}`,
		`{"type":"thinking_end","contentIndex":0,"content":"r"}`,
		`{"type":"toolcall_start","contentIndex":0,"id":"tu_1","toolName":"read"}`,
		`{"type":"toolcall_delta","contentIndex":0,"delta":"{}"}`,
		`{"type":"toolcall_end","contentIndex":0,"toolCall":{"type":"toolCall","id":"tu_1","name":"read","arguments":{}}}`,
		`{"type":"done","reason":"stop","message":{"role":"assistant","content":[],"api":"a","provider":"p","model":"m","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":0}}`,
		`{"type":"error","reason":"error","error":{"role":"assistant","content":[],"api":"a","provider":"p","model":"m","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"error","errorMessage":"boom","timestamp":0}}`,
	}
	wantNames := []string{
		"text_start", "text_delta", "text_end",
		"thinking_start", "thinking_delta", "thinking_end",
		"toolcall_start", "toolcall_delta", "toolcall_end",
		"done", "error",
	}
	for i, w := range wire {
		ev, err := UnmarshalEvent([]byte(w))
		if err != nil {
			t.Fatalf("case %d: unmarshal: %v", i, err)
		}
		if got := Name(ev); got != wantNames[i] {
			t.Errorf("case %d: got %q, want %q", i, got, wantNames[i])
		}
		// Re-marshalling must reproduce the wire form exactly.
		again, err := MarshalEvent(ev)
		if err != nil {
			t.Fatalf("case %d: re-marshal: %v", i, err)
		}
		if string(again) != w {
			t.Errorf("case %d: re-marshal mismatch:\n got: %s\nwant: %s", i, again, w)
		}
	}

	if _, err := UnmarshalEvent([]byte(`{"type":"nope"}`)); err == nil {
		t.Error("unknown event type must fail")
	}
}

// TestEventStreamLifecycle exercises push/consume/result, including the
// monotonic-clock timing of the terminal message.
//
// pi: packages/ai/src/utils/event-stream.ts:26-129.
func TestEventStreamLifecycle(t *testing.T) {
	s := NewEventStream()
	// Let some wall time pass so the terminal message's durationMs is
	// observable; its timestamp is set after the stream started, so pi's
	// "predates the stream" rule does not skip timing.
	time.Sleep(5 * time.Millisecond)

	final := Message{
		Role: RoleAssistant, Content: []ContentBlock{TextBlock("done")},
		API: "anthropic-messages", Provider: "anthropic", Model: "m",
		Usage: &Usage{TotalTokens: 1}, StopReason: StopStop,
		TimestampMS: time.Now().UnixMilli(),
	}
	s.Push(EvStart{Partial: Message{Role: RoleAssistant}})
	s.Push(EvTextDelta{ContentIndex: 0, Delta: "do", Partial: Message{Role: RoleAssistant}})
	s.Push(EvTextDelta{ContentIndex: 0, Delta: "ne", Partial: Message{Role: RoleAssistant}})
	s.Push(EvTextEnd{ContentIndex: 0, Content: "done", Partial: final})
	s.Push(EvDone{Reason: StopStop, Message: final})
	// Pushing after the terminal event is ignored, as in pi.
	s.Push(EvTextDelta{ContentIndex: 0, Delta: "late", Partial: final})
	s.End()

	var names []string
	for ev, err := range s.Events() {
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		names = append(names, Name(ev))
	}
	want := []string{"start", "text_delta", "text_delta", "text_end", "done"}
	if len(names) != len(want) {
		t.Fatalf("event names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, names[i], want[i])
		}
	}

	got, err := s.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if got.DurationMS < 0 {
		t.Errorf("durationMs = %d, want >= 0", got.DurationMS)
	}
	if MessageText(got) != "done" {
		t.Errorf("final text = %q, want %q", MessageText(got), "done")
	}
}

// TestEventStreamConcurrentPushConsume exercises the shape P2's loop uses:
// one goroutine produces while another consumes. A deadlock here would hang
// the agent, so it gets a test rather than a hope. (The race detector is not
// available on this machine — no C compiler — so this also serves as the
// concurrency smoke test.)
func TestEventStreamConcurrentPushConsume(t *testing.T) {
	s := NewEventStream()
	const n = 500

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range n {
			s.Push(EvTextDelta{ContentIndex: 0, Delta: "x"})
		}
		final := Message{Role: RoleAssistant, StopReason: StopStop, Usage: &Usage{}}
		s.Push(EvDone{Reason: StopStop, Message: final})
		s.End()
	}()

	var count int
	for ev, err := range s.Events() {
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		if _, ok := ev.(EvDone); ok {
			break
		}
		count++
	}
	<-done
	if count != n {
		t.Errorf("consumed %d deltas, want %d", count, n)
	}
	got, err := s.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if got.StopReason != StopStop {
		t.Errorf("stop reason = %q, want stop", got.StopReason)
	}
}

// TestStreamEndsWithoutTerminalEvent checks that Result reports the protocol
// violation instead of returning a zero message silently.
func TestStreamEndsWithoutTerminalEvent(t *testing.T) {
	s := NewEventStream()
	s.Push(EvTextDelta{ContentIndex: 0, Delta: "x"})
	s.End()
	if _, err := s.Result(); err == nil {
		t.Error("Result must fail when the stream ended without done/error")
	}
}

// TestStreamOf checks the fixed-slice stream used by fakes and tests.
func TestStreamOf(t *testing.T) {
	final := Message{Role: RoleAssistant, StopReason: StopStop, Usage: &Usage{}}
	s := StreamOf(
		EvStart{},
		EvTextDelta{ContentIndex: 0, Delta: "hi"},
		EvDone{Reason: StopStop, Message: final},
	)
	var count int
	for range s.Events() {
		count++
	}
	if count != 3 {
		t.Errorf("got %d events, want 3", count)
	}
	got, err := s.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if got.StopReason != StopStop {
		t.Errorf("stop reason = %q, want stop", got.StopReason)
	}
}

// TestMarshalEventIsJSON checks every event marshals to a JSON object whose
// first key is "type" — the NDJSON line shape consumers parse.
func TestMarshalEventIsJSON(t *testing.T) {
	events := []AssistantEvent{
		EvStart{},
		EvTextStart{ContentIndex: 0},
		EvTextDelta{ContentIndex: 0, Delta: "d"},
		EvTextEnd{ContentIndex: 0, Content: "c"},
		EvThinkingStart{ContentIndex: 0},
		EvThinkingDelta{ContentIndex: 0, Delta: "d"},
		EvThinkingEnd{ContentIndex: 0, Content: "c"},
		EvToolCallStart{ContentIndex: 0, Partial: Message{Role: RoleAssistant, Content: []ContentBlock{ToolCallBlock("tu_1", "read", nil)}}},
		EvToolCallDelta{ContentIndex: 0, Delta: "d"},
		EvToolCallEnd{ContentIndex: 0, ToolCall: ToolCallBlock("tu_1", "read", map[string]any{})},
		EvDone{Reason: StopStop, Message: Message{Role: RoleAssistant}},
		EvError{Reason: StopError, Error: Message{Role: RoleAssistant}},
	}
	for _, ev := range events {
		b, err := MarshalEvent(ev)
		if err != nil {
			t.Fatalf("%s: marshal: %v", Name(ev), err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			t.Fatalf("%s: not a JSON object: %s", Name(ev), b)
		}
		if _, ok := obj["type"]; !ok {
			t.Errorf("%s: missing type key: %s", Name(ev), b)
		}
		if len(b) < 8 || string(b[:8]) != `{"type":` {
			t.Errorf("%s: type is not the first key: %s", Name(ev), b)
		}
	}
}
