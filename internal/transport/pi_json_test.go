package transport

// Tests for the pi print/json wire protocol (P5).
//
// The assertions are line-shape assertions against pi's json-event.ts and
// agent-session.ts: every line must carry the discriminant pi uses, the
// message_update envelope must strip `partial` and hoist `usage`, and
// toolcall_start must carry the call's id/toolName. The fixture events are
// built the way the P2 fakes build them, so the shapes under test are the
// shapes the loop actually emits.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
	"ultrac/internal/session"
)

// line is one decoded output line.
type line struct {
	Type string `json:"type"`
}

func writeEvents(t *testing.T, events ...agent.AgentEvent) []string {
	t.Helper()
	var buf bytes.Buffer
	w := NewPiJSONWriter(&buf)
	for _, ev := range events {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("WriteEvent(%T) error: %v", ev, err)
		}
	}
	out := buf.String()
	if out != "" && !strings.HasSuffix(out, "\n") {
		t.Fatalf("output does not end with a newline: %q", out)
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// writeLines writes events plus a header and returns the raw lines.
func writeLines(t *testing.T, header *session.Header, events ...agent.AgentEvent) []string {
	t.Helper()
	var buf bytes.Buffer
	w := NewPiJSONWriter(&buf)
	if err := w.WriteHeader(header); err != nil {
		t.Fatalf("WriteHeader error: %v", err)
	}
	for _, ev := range events {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("WriteEvent(%T) error: %v", ev, err)
		}
	}
	out := strings.TrimSuffix(buf.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func assistantMsg(text string) ai.Message {
	return ai.Message{
		Role:        ai.RoleAssistant,
		Content:     []ai.ContentBlock{ai.TextBlock(text)},
		API:         "anthropic-messages",
		Provider:    "deepseek",
		Model:       "deepseek-chat",
		StopReason:  ai.StopStop,
		Usage:       &ai.Usage{Input: 11, Output: 5, TotalTokens: 16},
		TimestampMS: 1000,
	}
}

func TestPiJSONWriterEventDiscriminants(t *testing.T) {
	msg := assistantMsg("hello")
	events := []agent.AgentEvent{
		agent.EvAgentStart{},
		agent.EvMessageStart{Message: msg},
		agent.EvMessageUpdate{
			Message: msg,
			AssistantEvent: ai.EvTextDelta{
				ContentIndex: 0,
				Delta:        "hel",
				Partial:      msg,
			},
		},
		agent.EvMessageEnd{Message: msg},
		agent.EvTurnEnd{Message: msg, ToolResults: nil},
		agent.EvAgentEnd{Messages: []ai.Message{msg}},
		agent.EvAgentSettled{},
	}
	lines := writeEvents(t, events...)
	want := []string{
		"agent_start",
		"message_start",
		"message_update",
		"message_end",
		"turn_end",
		"agent_end",
		"agent_settled",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(lines), len(want), lines)
	}
	for i, w := range want {
		var l line
		if err := json.Unmarshal([]byte(lines[i]), &l); err != nil {
			t.Fatalf("line %d is not JSON: %v (%q)", i, err, lines[i])
		}
		if l.Type != w {
			t.Errorf("line %d type = %q, want %q", i, l.Type, w)
		}
	}

	// agent_settled carries the aborted flag (pi: agent-session.ts:203).
	var settled struct {
		Type    string `json:"type"`
		Aborted bool   `json:"aborted"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &settled); err != nil {
		t.Fatalf("last line is not JSON: %v (%q)", err, lines[len(lines)-1])
	}
	if settled.Type != "agent_settled" || settled.Aborted {
		t.Errorf("settled line = %+v, want type agent_settled with aborted false", settled)
	}
}

func TestPiJSONWriterMessageUpdateProjection(t *testing.T) {
	// The partial must not appear; usage hoists to the envelope; the
	// assistant event keeps its own fields.
	msg := assistantMsg("hello")
	msg.Usage = &ai.Usage{Input: 3, Output: 7, TotalTokens: 10}
	lines := writeEvents(t, agent.EvMessageUpdate{
		Message:        msg,
		AssistantEvent: ai.EvTextDelta{ContentIndex: 0, Delta: "hel", Partial: msg},
	})
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	var got struct {
		Type                  string `json:"type"`
		Usage                 any    `json:"usage"`
		AssistantMessageEvent struct {
			Type         string `json:"type"`
			ContentIndex int    `json:"contentIndex"`
			Delta        string `json:"delta"`
			Partial      any    `json:"partial"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line is not JSON: %v (%q)", err, lines[0])
	}
	if got.Type != "message_update" {
		t.Errorf("type = %q, want message_update", got.Type)
	}
	if got.AssistantMessageEvent.Type != "text_delta" {
		t.Errorf("assistant event type = %q, want text_delta", got.AssistantMessageEvent.Type)
	}
	if got.AssistantMessageEvent.Delta != "hel" {
		t.Errorf("delta = %q, want %q", got.AssistantMessageEvent.Delta, "hel")
	}
	if got.AssistantMessageEvent.Partial != nil {
		t.Errorf("partial must be stripped, got %v", got.AssistantMessageEvent.Partial)
	}
	usage, ok := got.Usage.(map[string]any)
	if !ok {
		t.Fatalf("usage is not an object: %v", got.Usage)
	}
	if usage["input"] != float64(3) || usage["output"] != float64(7) {
		t.Errorf("usage = %v, want input 3 output 7", usage)
	}
}

func TestPiJSONWriterToolCallStartCarriesIdentity(t *testing.T) {
	// pi's json-event.ts:23-30: toolcall_start additionally carries the id
	// and toolName read from the partial's content block.
	partial := ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			ai.TextBlock("thinking out loud"),
			ai.ToolCallBlock("call_1", "read", map[string]any{"path": "go.mod"}),
		},
	}
	lines := writeEvents(t, agent.EvMessageUpdate{
		Message:        partial,
		AssistantEvent: ai.EvToolCallStart{ContentIndex: 1, Partial: partial},
	})
	var got struct {
		AssistantMessageEvent struct {
			Type         string `json:"type"`
			ContentIndex int    `json:"contentIndex"`
			ID           string `json:"id"`
			ToolName     string `json:"toolName"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line is not JSON: %v (%q)", err, lines[0])
	}
	if got.AssistantMessageEvent.ID != "call_1" {
		t.Errorf("id = %q, want call_1", got.AssistantMessageEvent.ID)
	}
	if got.AssistantMessageEvent.ToolName != "read" {
		t.Errorf("toolName = %q, want read", got.AssistantMessageEvent.ToolName)
	}
}

func TestPiJSONWriterToolExecutionEvents(t *testing.T) {
	duration := int64(42)
	result := agent.NewTextResult("file contents")
	result.Details = map[string]any{"lines": 3}
	lines := writeEvents(t,
		agent.EvToolExecutionStart{
			ToolCallID: "call_1",
			ToolName:   "read",
			Args:       map[string]any{"path": "go.mod"},
		},
		agent.EvToolExecutionEnd{
			ToolCallID: "call_1",
			ToolName:   "read",
			Result:     result,
			IsError:    false,
			DurationMS: &duration,
		},
		// A tool that did not run omits durationMs entirely (pi:
		// agent-loop.ts:926).
		agent.EvToolExecutionEnd{
			ToolCallID: "call_2",
			ToolName:   "bash",
			Result:     agent.NewTextResult("blocked"),
			IsError:    true,
		},
	)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}

	var start struct {
		Type       string `json:"type"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Args       any    `json:"args"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
		t.Fatalf("line 0: %v", err)
	}
	if start.Type != "tool_execution_start" || start.ToolCallID != "call_1" || start.ToolName != "read" {
		t.Errorf("start line = %+v", start)
	}

	var end struct {
		Type       string `json:"type"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Result     struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Details map[string]any `json:"details"`
		} `json:"result"`
		IsError    bool `json:"isError"`
		DurationMS any  `json:"durationMs"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &end); err != nil {
		t.Fatalf("line 1: %v", err)
	}
	if end.Type != "tool_execution_end" || end.ToolCallID != "call_1" {
		t.Errorf("end line = %+v", end)
	}
	if len(end.Result.Content) != 1 || end.Result.Content[0].Text != "file contents" {
		t.Errorf("result content = %+v", end.Result.Content)
	}
	if end.Result.Details["lines"] != float64(3) {
		t.Errorf("result details = %v", end.Result.Details)
	}
	if end.DurationMS != float64(42) {
		t.Errorf("durationMs = %v, want 42", end.DurationMS)
	}

	var noDuration struct {
		DurationMS any `json:"durationMs"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &noDuration); err != nil {
		t.Fatalf("line 2: %v", err)
	}
	if noDuration.DurationMS != nil {
		t.Errorf("durationMs must be absent when the tool did not run, got %v", noDuration.DurationMS)
	}
	if strings.Contains(lines[2], "durationMs") {
		t.Errorf("durationMs key must not appear at all: %q", lines[2])
	}
}

func TestPiJSONWriterHeaderLine(t *testing.T) {
	// JSON mode opens with the session header (pi: print-mode.ts:122-127).
	h := &session.Header{
		Type:      "session",
		Version:   session.CurrentSessionVersion,
		ID:        "0197f1e0-0000-7000-8000-000000000001",
		Timestamp: "2026-10-16T00:00:00.000Z",
		CWD:       `D:\develop\Project\Ultrac`,
	}
	lines := writeLines(t, h, agent.EvAgentStart{})
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (header + agent_start)", len(lines))
	}
	var got struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
		ID      string `json:"id"`
		CWD     string `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("header line: %v (%q)", err, lines[0])
	}
	if got.Type != "session" || got.Version != session.CurrentSessionVersion || got.ID != h.ID {
		t.Errorf("header = %+v", got)
	}
	if got.CWD != h.CWD {
		t.Errorf("header cwd = %q, want %q", got.CWD, h.CWD)
	}
	var second line
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second line: %v", err)
	}
	if second.Type != "agent_start" {
		t.Errorf("second line type = %q, want agent_start", second.Type)
	}
}

func TestPiJSONWriterNilHeaderIsNoop(t *testing.T) {
	lines := writeLines(t, nil, agent.EvAgentStart{})
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
}

func TestPiJSONWriterEscapesLineTerminators(t *testing.T) {
	// U+2028/U+2029 must not split a line (plan R8). A JS reader (and jq on
	// some builds) treats them as terminators.
	msg := assistantMsg("before\u2028after\u2029end")
	lines := writeEvents(t, agent.EvMessageStart{Message: msg})
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 — U+2028/29 split the output", len(lines))
	}
	if !strings.Contains(lines[0], `\u2028`) || !strings.Contains(lines[0], `\u2029`) {
		t.Errorf("escaped forms missing: %q", lines[0])
	}
	// The escaped form must decode back to the original text.
	var got struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("escaped line is not valid JSON: %v", err)
	}
	if got.Message.Content[0].Text != "before\u2028after\u2029end" {
		t.Errorf("round-trip text = %q", got.Message.Content[0].Text)
	}
}

func TestPiJSONWriterMessageUpdateNonAssistantIsError(t *testing.T) {
	// pi throws (json-event.ts:52-54); we surface the error rather than
	// emitting a line no reader could trust.
	var buf bytes.Buffer
	w := NewPiJSONWriter(&buf)
	err := w.WriteEvent(agent.EvMessageUpdate{
		Message:        ai.NewTextMessage(ai.RoleUser, "hi"),
		AssistantEvent: ai.EvTextDelta{Delta: "x"},
	})
	if err == nil {
		t.Fatal("expected an error for a non-assistant message_update")
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be written on error, got %q", buf.String())
	}
}

func TestPiJSONWriterTurnEndCarriesMessages(t *testing.T) {
	assistant := assistantMsg("answer")
	toolResult := ai.Message{
		Role:        ai.RoleToolResult,
		ToolCallID:  "call_1",
		ToolName:    "read",
		Content:     []ai.ContentBlock{ai.TextBlock("contents")},
		TimestampMS: 1001,
	}
	lines := writeEvents(t, agent.EvTurnEnd{
		Message:     assistant,
		ToolResults: []ai.Message{toolResult},
	})
	var got struct {
		Type    string `json:"type"`
		Message struct {
			Role string `json:"role"`
		} `json:"message"`
		ToolResults []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
		} `json:"toolResults"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line: %v", err)
	}
	if got.Type != "turn_end" || got.Message.Role != "assistant" {
		t.Errorf("turn_end = %+v", got)
	}
	if len(got.ToolResults) != 1 || got.ToolResults[0].Role != "toolResult" {
		t.Fatalf("toolResults = %+v", got.ToolResults)
	}
	if got.ToolResults[0].ToolCallID != "call_1" || got.ToolResults[0].ToolName != "read" {
		t.Errorf("toolResult identity = %+v", got.ToolResults[0])
	}
}

func TestPiJSONWriterWriteObjectOneLinePerObject(t *testing.T) {
	// The RPC response path: responses are not agent events but share the
	// stream with them, so WriteObject applies the same framing rules —
	// one object per line, U+2028/29 escaped (P6, plan §3.7).
	var buf bytes.Buffer
	w := NewPiJSONWriter(&buf)
	if err := w.WriteObject(map[string]any{"type": "response", "command": "get_state", "success": true}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteObject(map[string]any{"type": "response", "error": "line\u2028break"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if !strings.Contains(lines[1], `\u2028`) {
		t.Errorf("U+2028 not escaped in a response: %q", lines[1])
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("response line is not JSON: %v", err)
	}
	if first["command"] != "get_state" || first["success"] != true {
		t.Errorf("first response = %v", first)
	}
}

func TestPiJSONWriterEventsAndObjectsShareOneStream(t *testing.T) {
	// The P6 guarantee in one assertion: an event and a response written
	// through the same writer land as two whole lines, never interleaved.
	var buf bytes.Buffer
	w := NewPiJSONWriter(&buf)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = w.WriteObject(map[string]any{"type": "response", "command": "prompt", "i": i})
		}
	}()
	for i := 0; i < 50; i++ {
		_ = w.WriteEvent(agent.EvTurnStart{})
	}
	<-done
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 100 {
		t.Fatalf("got %d lines, want 100", len(lines))
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line is not whole JSON: %q: %v", l, err)
		}
	}
}

// TestPiJSONWriterCompactionEvents pins the compaction pair's wire shape
// (plan D4): the same discriminants and field names pi's RPC line carries,
// with result absent — not null — on failure and errorMessage absent when
// the compaction was aborted rather than failed.
func TestPiJSONWriterCompactionEvents(t *testing.T) {
	result := map[string]any{
		"summary":              "## Goal\nship it",
		"firstKeptEntryId":     "entry-42",
		"tokensBefore":         182340,
		"estimatedTokensAfter": 24110,
	}
	lines := writeEvents(t,
		agent.EvCompactionStart{Reason: agent.CompactionReasonThreshold},
		agent.EvCompactionEnd{
			Reason:    agent.CompactionReasonThreshold,
			Result:    result,
			WillRetry: false,
		},
		agent.EvCompactionEnd{
			Reason:       agent.CompactionReasonManual,
			Aborted:      true,
			WillRetry:    false,
			ErrorMessage: "",
		},
		agent.EvCompactionEnd{
			Reason:       agent.CompactionReasonThreshold,
			WillRetry:    false,
			ErrorMessage: "Auto-compaction failed: 401 unauthorized",
		},
	)
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4: %v", len(lines), lines)
	}

	var start struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
		t.Fatalf("line 0 is not JSON: %v (%q)", err, lines[0])
	}
	if start.Type != "compaction_start" || start.Reason != "threshold" {
		t.Errorf("line 0 = %+v, want compaction_start/threshold", start)
	}

	var ok map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &ok); err != nil {
		t.Fatalf("line 1 is not JSON: %v (%q)", err, lines[1])
	}
	if ok["type"] != "compaction_end" || ok["reason"] != "threshold" {
		t.Errorf("line 1 = %v, want compaction_end/threshold", ok)
	}
	if ok["aborted"] != false || ok["willRetry"] != false {
		t.Errorf("line 1 flags = %v/%v, want false/false", ok["aborted"], ok["willRetry"])
	}
	res, _ := ok["result"].(map[string]any)
	if res == nil || res["firstKeptEntryId"] != "entry-42" {
		t.Errorf("line 1 result = %v, want the compaction result object", ok["result"])
	}
	if _, has := ok["errorMessage"]; has {
		t.Error("a successful compaction_end must not carry errorMessage")
	}

	var aborted map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &aborted); err != nil {
		t.Fatalf("line 2 is not JSON: %v (%q)", err, lines[2])
	}
	if aborted["aborted"] != true {
		t.Errorf("line 2 aborted = %v, want true", aborted["aborted"])
	}
	if _, has := aborted["result"]; has {
		t.Error("an aborted compaction_end must not carry result")
	}
	if _, has := aborted["errorMessage"]; has {
		t.Error("an aborted compaction carries no errorMessage (pi: undefined)")
	}

	var failed map[string]any
	if err := json.Unmarshal([]byte(lines[3]), &failed); err != nil {
		t.Fatalf("line 3 is not JSON: %v (%q)", err, lines[3])
	}
	if failed["aborted"] != false {
		t.Errorf("line 3 aborted = %v, want false", failed["aborted"])
	}
	if _, has := failed["result"]; has {
		t.Error("a failed compaction_end must not carry result")
	}
	if got, _ := failed["errorMessage"].(string); got != "Auto-compaction failed: 401 unauthorized" {
		t.Errorf("line 3 errorMessage = %q, want the prefixed failure", got)
	}
}
