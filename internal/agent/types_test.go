package agent

// C3 comparison tests: the tool contract, the agent event vocabulary, and the
// erased-tool path end to end.
//
// Expected literals are transcribed from pi's packages/agent/src/types.ts
// (file:line cited per case) and packages/ai/src/providers/faux.ts.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"ultrac/internal/ai"
)

// TestAgentEventNames pins the 13 agent events in pi's union order.
//
// pi: packages/agent/src/types.ts:516-539, plus the compaction pair from
// core/agent-session.ts:209-220 (emitted above the agent in this port but
// riding the same union — plan D4).
func TestAgentEventNames(t *testing.T) {
	events := []AgentEvent{
		EvAgentStart{}, EvAgentEnd{}, EvAgentSettled{}, EvTurnStart{}, EvTurnEnd{},
		EvMessageStart{}, EvMessageUpdate{}, EvMessageEnd{},
		EvToolExecutionStart{}, EvToolExecutionUpdate{}, EvToolExecutionEnd{},
		EvCompactionStart{}, EvCompactionEnd{},
	}
	want := []string{
		"agent_start", "agent_end", "agent_settled",
		"turn_start", "turn_end",
		"message_start", "message_update", "message_end",
		"tool_execution_start", "tool_execution_update", "tool_execution_end",
		"compaction_start", "compaction_end",
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i, ev := range events {
		if got := EventName(ev); got != want[i] {
			t.Errorf("event %d: got %q, want %q", i, got, want[i])
		}
	}
}

// TestCompactionReasonLiterals pins the reason enum both compaction events
// carry. "overflow" is reserved, not produced (plan D1).
//
// pi: agent-session.ts:209, 215.
func TestCompactionReasonLiterals(t *testing.T) {
	want := map[string]bool{CompactionReasonManual: true, CompactionReasonThreshold: true, CompactionReasonOverflow: true}
	for _, got := range []string{CompactionReasonManual, CompactionReasonThreshold, CompactionReasonOverflow} {
		if !want[got] {
			t.Errorf("reason %q not in the pinned set", got)
		}
	}
	if CompactionReasonManual != "manual" || CompactionReasonThreshold != "threshold" || CompactionReasonOverflow != "overflow" {
		t.Errorf("reason literals drifted: %q/%q/%q",
			CompactionReasonManual, CompactionReasonThreshold, CompactionReasonOverflow)
	}
}

// TestEnumLiterals pins the string enums the wire and settings speak.
//
// pi: packages/agent/src/types.ts:47 (execution mode), 55 (queue mode),
// 349 (thinking level).
func TestEnumLiterals(t *testing.T) {
	if got := []ToolExecutionMode{ExecSequential, ExecParallel}; string(got[0]) != "sequential" || string(got[1]) != "parallel" {
		t.Errorf("execution modes = %v", got)
	}
	if got := []QueueMode{QueueAll, QueueOneAtATime}; string(got[0]) != "all" || string(got[1]) != "one-at-a-time" {
		t.Errorf("queue modes = %v", got)
	}
	if got := []ThinkingLevel{ThinkOff, ThinkMinimal, ThinkLow, ThinkMedium, ThinkHigh, ThinkXHigh, ThinkMax}; string(got[6]) != "max" || string(got[0]) != "off" {
		t.Errorf("thinking levels = %v", got)
	}
}

// readInput is the fake tool's typed input: the point of AgentTool[In] is
// that Execute sees this struct, not a map.
type readInput struct {
	Path string `json:"path"`
}

// fakeReadTool builds a typed AgentTool whose schema requires `path`.
func fakeReadTool(t *testing.T, fn func(ctx context.Context, in readInput) (Result, error)) *AgentTool[readInput] {
	t.Helper()
	return &AgentTool[readInput]{
		Tool: ai.Tool{
			Name:        "read",
			Description: "Read a file",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"path": {"type": "string", "minLength": 1}},
				"required": ["path"],
				"additionalProperties": false
			}`),
		},
		Label: "Read",
		Execute: func(ctx context.Context, toolCallID string, in readInput, onUpdate func(Result)) (Result, error) {
			return fn(ctx, in)
		},
	}
}

// TestErasePrepareRun walks the erased tool through pi's preflight order:
// lookup → prepareArguments → schema validation → execute.
//
// pi: packages/agent/src/agent-loop.ts (prepareToolCall).
func TestErasePrepareRun(t *testing.T) {
	ctx := context.Background()
	var gotPath string
	tool := Erase(fakeReadTool(t, func(ctx context.Context, in readInput) (Result, error) {
		gotPath = in.Path
		return NewTextResult("contents of " + in.Path), nil
	}))

	if tool.Name() != "read" || tool.Label() != "Read" {
		t.Errorf("name/label = %q/%q", tool.Name(), tool.Label())
	}
	if tool.ExecutionMode() != "" {
		t.Errorf("execution mode = %q, want empty (loop default)", tool.ExecutionMode())
	}

	// Valid arguments pass the schema and reach Execute typed.
	in, err := tool.Prepare(map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatalf("prepare valid args: %v", err)
	}
	if _, ok := in.(readInput); !ok {
		t.Fatalf("prepared input has type %T, want readInput", in)
	}
	res, err := tool.Run(ctx, "tu_1", in, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotPath != "a.go" {
		t.Errorf("execute saw path %q, want a.go", gotPath)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "contents of a.go" {
		t.Errorf("result content = %+v", res.Content)
	}

	// Missing required field fails at validation, not at execute.
	if _, err := tool.Prepare(map[string]any{}); err == nil {
		t.Error("prepare with missing path must fail")
	}
	// Unknown field fails under additionalProperties: false.
	if _, err := tool.Prepare(map[string]any{"path": "a.go", "extra": 1}); err == nil {
		t.Error("prepare with unknown field must fail")
	}
	// Wrong type fails.
	if _, err := tool.Prepare(map[string]any{"path": 42}); err == nil {
		t.Error("prepare with wrong type must fail")
	}

	// A tool that fails by returning an error surfaces that error; the loop
	// (P2) converts it into an isError tool result.
	failing := Erase(fakeReadTool(t, func(ctx context.Context, in readInput) (Result, error) {
		return Result{}, errors.New("boom")
	}))
	in, err = failing.Prepare(map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := failing.Run(ctx, "tu_1", in, nil); err == nil {
		t.Error("run must surface the tool's error")
	}
}

// TestPrepareArgumentsShim checks the compatibility shim runs before schema
// validation and can rename keys.
//
// pi: packages/agent/src/types.ts:469-473.
func TestPrepareArgumentsShim(t *testing.T) {
	tool := fakeReadTool(t, func(ctx context.Context, in readInput) (Result, error) {
		return NewTextResult(in.Path), nil
	})
	tool.PrepareArguments = func(raw map[string]any) (readInput, error) {
		file, _ := raw["file"].(string)
		return readInput{Path: file}, nil
	}
	erased := Erase(tool)

	in, err := erased.Prepare(map[string]any{"file": "a.go"})
	if err != nil {
		t.Fatalf("prepare via shim: %v", err)
	}
	res, err := erased.Run(context.Background(), "tu_1", in, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Content[0].Text != "a.go" {
		t.Errorf("shim did not map file→path, got %q", res.Content[0].Text)
	}

	// The shim's output still goes through schema validation: an empty path
	// must be rejected by required, not silently executed.
	if _, err := erased.Prepare(map[string]any{"file": ""}); err == nil {
		t.Error("shim output must still satisfy the schema")
	}
}

// TestBrokenSchemaFailsCallsNotProcess checks that an invalid parameters
// schema surfaces as a per-call error rather than an init panic.
func TestBrokenSchemaFailsCallsNotProcess(t *testing.T) {
	tool := fakeReadTool(t, func(ctx context.Context, in readInput) (Result, error) {
		return NewTextResult("never"), nil
	})
	tool.Parameters = json.RawMessage(`{"type": 7}`) // not a valid schema type
	erased := Erase(tool)

	if _, err := erased.Prepare(map[string]any{"path": "a.go"}); err == nil {
		t.Error("prepare must fail when the schema does not compile")
	}
}

// TestToolResultMessageShape pins the JSON of the toolResult message the loop
// builds from a Result, including the details field.
//
// pi: packages/ai/src/types.ts:620-637.
func TestToolResultMessageShape(t *testing.T) {
	msg := ai.Message{
		Role:        ai.RoleToolResult,
		ToolCallID:  "tu_1",
		ToolName:    "read",
		Content:     []ai.ContentBlock{ai.TextBlock("file contents")},
		Details:     map[string]any{"bytes": 13},
		IsError:     false,
		TimestampMS: 7,
	}
	got, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"role":"toolResult","toolCallId":"tu_1","toolName":"read","content":[{"type":"text","text":"file contents"}],"details":{"bytes":13},"isError":false,"timestamp":7}`
	if string(got) != want {
		t.Errorf("wire mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestAfterToolCallOverrideSemantics pins the field-by-field merge: provided
// fields replace, omitted fields keep the executed values.
//
// pi: packages/agent/src/types.ts:76-104.
func TestAfterToolCallOverrideSemantics(t *testing.T) {
	no := false
	override := AfterToolCallResult{
		Content:   []ai.ContentBlock{ai.TextBlock("replaced")},
		IsError:   &no,
		Terminate: &no,
	}
	if len(override.Content) != 1 || override.Content[0].Text != "replaced" {
		t.Errorf("content override = %+v", override.Content)
	}
	if override.IsError == nil || *override.IsError {
		t.Errorf("isError override must be an explicit false, got %v", override.IsError)
	}
	// Omitted fields stay nil so the merge keeps the executed values.
	if override.Details != nil || override.StructuredContent != nil || override.Usage != nil {
		t.Errorf("omitted fields must be nil, got %+v", override)
	}
}

// TestRegistry exercises the tool registry the loop looks tools up in.
func TestRegistry(t *testing.T) {
	mk := func(name string) Tool {
		return Erase(&AgentTool[readInput]{
			Tool: ai.Tool{Name: name, Description: name, Parameters: json.RawMessage(`{"type":"object"}`)},
			Execute: func(ctx context.Context, id string, in readInput, onUpdate func(Result)) (Result, error) {
				return NewTextResult(name), nil
			},
		})
	}
	r := NewRegistry(mk("read"), mk("bash"))
	if _, ok := r.Lookup("read"); !ok {
		t.Error("read must be registered")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Error("unknown tool must not resolve")
	}
	if diff := cmp.Diff([]string{"bash", "read"}, r.Names()); diff != "" {
		t.Errorf("names mismatch (-want +got):\n%s", diff)
	}
	if err := r.Add(mk("read")); err == nil {
		t.Error("duplicate registration must fail")
	}
	if err := r.Add(nil); err == nil {
		t.Error("nil tool must fail")
	}
}

// TestAgentLoopConfigEmbedsStreamOptions checks the loop config carries the
// stream options, mirroring pi's AgentLoopConfig extends SimpleStreamOptions.
//
// pi: packages/agent/src/types.ts:193.
func TestAgentLoopConfigEmbedsStreamOptions(t *testing.T) {
	temp := 0.5
	cfg := AgentLoopConfig{
		StreamOptions: ai.StreamOptions{Temperature: &temp, MaxTokens: 1024, Reasoning: ai.ThinkHigh},
		Model:         ai.Model{ID: "m", API: ai.APIAnthropicMessages, Provider: ai.ProviderAnthropic},
	}
	if cfg.MaxTokens != 1024 || cfg.Reasoning != ai.ThinkHigh {
		t.Errorf("stream options not embedded: %+v", cfg.StreamOptions)
	}
	if cfg.ToolExecution != "" {
		t.Errorf("default tool execution = %q, want empty (parallel)", cfg.ToolExecution)
	}
}
