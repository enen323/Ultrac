package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ultrac/internal/contract/event"
	"ultrac/internal/contract/provider"
)

func TestTextSeparatesTheAnswerFromCommentary(t *testing.T) {
	var out, meta bytes.Buffer
	s := NewText(&out, &meta)

	_ = s.Emit(context.Background(), event.Event{Kind: event.KindMessageStart})
	_ = s.Emit(context.Background(), event.Text("the answer"))
	_ = s.Emit(context.Background(), event.ToolCall(event.ToolUse{
		ID: "t", Name: "Read", Input: json.RawMessage(`{"file_path":"/a/b.go"}`),
	}))
	_ = s.Emit(context.Background(), event.ToolOutput(event.ToolResult{
		ToolUseID: "t", Name: "Read", Content: "package main",
	}))
	_ = s.Emit(context.Background(), event.Text("."))
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}

	// The redirect target must be exactly the answer: `ultrac -p ... > file`
	// producing a file with tool chatter in it is the failure this prevents.
	//
	// The answer is split across lines because a tool call landed in the
	// middle of it — that is what happened, and closing the line before the
	// commentary is what keeps the two from interleaving mid-word.
	if got := out.String(); !strings.Contains(got, "the answer") || !strings.Contains(got, ".") {
		t.Errorf("stdout lost part of the answer: %q", got)
	}
	if got := out.String(); strings.Contains(got, "Read") || strings.Contains(got, "b.go") {
		t.Errorf("tool activity leaked into the answer: %q", got)
	}
	if !strings.Contains(meta.String(), "Read") {
		t.Errorf("tool activity missing from meta: %q", meta.String())
	}
	if !strings.Contains(meta.String(), "b.go") {
		t.Errorf("tool input summary missing: %q", meta.String())
	}
}

func TestTextThinkingGoesToMeta(t *testing.T) {
	var out, meta bytes.Buffer
	s := NewText(&out, &meta)
	_ = s.Emit(context.Background(), event.Thinking("internal reasoning"))
	_ = s.Emit(context.Background(), event.Text("answer"))

	if strings.Contains(out.String(), "internal reasoning") {
		t.Errorf("thinking leaked into the answer: %q", out.String())
	}
	if !strings.Contains(meta.String(), "internal reasoning") {
		t.Errorf("thinking missing from meta: %q", meta.String())
	}
}

func TestTextFlushesBeforeCommentary(t *testing.T) {
	var out, meta bytes.Buffer
	s := NewText(&out, &meta)
	_ = s.Emit(context.Background(), event.Text("partial"))
	_ = s.Emit(context.Background(), event.NoticeEvent(event.Notice{Code: "n", Message: "a note"}))

	// The answer line must be closed before the note is written, or a reader
	// sees the answer split by a diagnostic.
	if !strings.HasSuffix(out.String(), "partial\n") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestTextErrorResultIsMarked(t *testing.T) {
	var out, meta bytes.Buffer
	s := NewText(&out, &meta)
	_ = s.Emit(context.Background(), event.ToolOutput(event.ToolResult{
		ToolUseID: "t", Name: "Bash", Content: "boom", IsError: true,
	}))
	if !strings.Contains(meta.String(), "✗") {
		t.Errorf("a failed tool has no failure marker: %q", meta.String())
	}
}

func TestTextFailureIsReportedWithItsCode(t *testing.T) {
	var out, meta bytes.Buffer
	s := NewText(&out, &meta)
	_ = s.Emit(context.Background(), event.FailureEvent(event.Failure{
		Code: "permission_denied", Message: "nope", Fatal: true,
	}))
	if !strings.Contains(meta.String(), "permission_denied") {
		t.Errorf("failure code missing: %q", meta.String())
	}
}

func TestDrainToDiscard(t *testing.T) {
	seq := func(yield func(event.Event, error) bool) {
		yield(event.Text("a"), nil)
		yield(event.Text("b"), nil)
	}
	if err := event.Drain(context.Background(), seq, Discard{}); err != nil {
		t.Fatal(err)
	}
}

func TestJSONAccumulates(t *testing.T) {
	var buf bytes.Buffer
	j := &JSON{Out: &buf}
	_ = j.Emit(context.Background(), event.Event{Kind: event.KindMessageStart})
	_ = j.Emit(context.Background(), event.Text("hello "))
	_ = j.Emit(context.Background(), event.Text("world"))
	_ = j.Emit(context.Background(), event.UsageEvent(provider.Usage{InputTokens: 5, OutputTokens: 6}))
	_ = j.Emit(context.Background(), event.ToolCall(event.ToolUse{
		ID: "t", Name: "Glob", Input: json.RawMessage(`{"pattern":"*.go"}`),
	}))
	_ = j.Emit(context.Background(), event.NoticeEvent(event.Notice{Code: "n", Message: "note"}))
	_ = j.Emit(context.Background(), event.FailureEvent(event.Failure{Code: "f", Message: "bad"}))

	if got := j.Text(); got != "hello world" {
		t.Errorf("text = %q", got)
	}
	if err := j.Write(map[string]any{"num_turns": 1}); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not one JSON document: %v\n%s", err, buf.String())
	}
	if doc["result"] != "hello world" {
		t.Errorf("result = %v", doc["result"])
	}
	if doc["type"] != "result" {
		t.Errorf("type = %v", doc["type"])
	}
	if doc["num_turns"].(float64) != 1 {
		t.Errorf("extra field was dropped: %v", doc)
	}
	calls := doc["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", calls)
	}
	if calls[0].(map[string]any)["input"].(map[string]any)["pattern"] != "*.go" {
		t.Errorf("tool input is not an object: %v", calls[0])
	}
	if len(doc["notices"].([]any)) != 1 || len(doc["failures"].([]any)) != 1 {
		t.Errorf("notices/failures = %v / %v", doc["notices"], doc["failures"])
	}
	usage := doc["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 5 || usage["output_tokens"].(float64) != 6 {
		t.Errorf("usage = %v", usage)
	}
}

func TestMultiStopsAtTheFirstError(t *testing.T) {
	var order []string
	first := sinkFunc(func(context.Context, event.Event) error {
		order = append(order, "first")
		return nil
	})
	boom := sinkFunc(func(context.Context, event.Event) error {
		order = append(order, "boom")
		return errTest
	})
	third := sinkFunc(func(context.Context, event.Event) error {
		order = append(order, "third")
		return nil
	})

	err := Multi{first, boom, third}.Emit(context.Background(), event.Text("x"))
	if err != errTest {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(order, ",") != "first,boom" {
		t.Errorf("order = %v", order)
	}
}

func TestMultiSkipsNilSinks(t *testing.T) {
	if err := (Multi{nil, Discard{}}).Emit(context.Background(), event.Text("x")); err != nil {
		t.Fatal(err)
	}
}

type sinkFunc func(context.Context, event.Event) error

func (f sinkFunc) Emit(ctx context.Context, ev event.Event) error { return f(ctx, ev) }

var errTest = errTestType("test error")

type errTestType string

func (e errTestType) Error() string { return string(e) }
