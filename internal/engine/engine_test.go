package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"ultrac/internal/contract/event"
	"ultrac/internal/contract/provider"
	"ultrac/internal/contract/tool"
	"ultrac/internal/permission"
)

// scriptedProvider replays a canned response per call.
//
// It is a provider, not a mock of the loop: the loop's job is to drive
// whatever the provider returns, so the test can only be meaningful if the
// thing being driven is real.
type scriptedProvider struct {
	mu      sync.Mutex
	scripts [][]provider.Chunk
	calls   int
	reqs    []provider.Request
	err     error
}

func (p *scriptedProvider) Name() string { return "scripted" }
func (p *scriptedProvider) Kind() string { return "scripted" }

func (p *scriptedProvider) Stream(_ context.Context, req provider.Request) iter.Seq2[provider.Chunk, error] {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	idx := p.calls
	p.calls++
	p.mu.Unlock()

	return func(yield func(provider.Chunk, error) bool) {
		if p.err != nil {
			yield(provider.Chunk{}, p.err)
			return
		}
		if idx >= len(p.scripts) {
			yield(provider.Chunk{}, fmt.Errorf("scripted: no script for call %d", idx))
			return
		}
		for _, c := range p.scripts[idx] {
			if !yield(c, nil) {
				return
			}
		}
	}
}

func textChunks(text string, stop provider.StopReason) []provider.Chunk {
	return []provider.Chunk{
		{Kind: provider.ChunkText, Text: text},
		{Kind: provider.ChunkUsage, Usage: &provider.Usage{InputTokens: 10, OutputTokens: 5}},
		{Kind: provider.ChunkStop, Stop: stop},
	}
}

func toolChunks(calls ...provider.ToolCall) []provider.Chunk {
	out := []provider.Chunk{{Kind: provider.ChunkText, Text: "working"}}
	for i := range calls {
		c := calls[i]
		out = append(out, provider.Chunk{Kind: provider.ChunkToolCall, ToolCall: &c})
	}
	out = append(out,
		provider.Chunk{Kind: provider.ChunkUsage, Usage: &provider.Usage{InputTokens: 20, OutputTokens: 7}},
		provider.Chunk{Kind: provider.ChunkStop, Stop: provider.StopToolUse})
	return out
}

// testSpec is the static half of a tool definition, copied from the builtin
// package rather than imported because importing it would make this package's
// tests depend on the tool set they are supposed to be independent of.
type testSpec[In any] struct {
	Name_           string
	Aliases_        []string
	Hint            string
	Desc            string
	Schema          string
	ReadOnly        bool
	ConcurrencySafe bool
}

func (s testSpec[In]) Name() string       { return s.Name_ }
func (s testSpec[In]) Aliases() []string  { return s.Aliases_ }
func (s testSpec[In]) SearchHint() string { return s.Hint }
func (s testSpec[In]) Description(context.Context, In, tool.DescriptionOptions) (string, error) {
	return s.Desc, nil
}
func (s testSpec[In]) InputSchema() json.RawMessage      { return json.RawMessage(s.Schema) }
func (s testSpec[In]) OutputSchema() json.RawMessage     { return nil }
func (s testSpec[In]) IsConcurrencySafe(In) bool         { return s.ConcurrencySafe }
func (s testSpec[In]) IsReadOnly(In) bool                { return s.ReadOnly }
func (s testSpec[In]) IsDestructive(In) bool             { return false }
func (s testSpec[In]) IsOpenWorld(In) bool               { return false }
func (s testSpec[In]) IsSearchOrRead(In) tool.SearchRead { return tool.SearchRead{} }
func (s testSpec[In]) InterruptBehavior() tool.InterruptBehavior {
	return tool.InterruptBlock
}
func (s testSpec[In]) RequiresUserInteraction() bool { return false }

// echoTool is a minimal registered tool that reports back what it was given.
type echoTool struct{ testSpec[echoInput] }

type echoInput struct {
	Value string `json:"value,omitempty"`
	Wait  int    `json:"waitMs,omitempty"`
	Fail  bool   `json:"fail,omitempty"`
	Panic bool   `json:"panic,omitempty"`
}

const echoSchema = `{"type":"object","properties":{"value":{"type":"string"},"waitMs":{"type":"integer"},"fail":{"type":"boolean"},"panic":{"type":"boolean"}}}`

func newEchoTool() tool.Erased {
	return tool.Adapt[echoInput](echoTool{testSpec: testSpec[echoInput]{
		Name_:           "Echo",
		Hint:            "echo a value",
		Desc:            "Echoes a value back.",
		Schema:          echoSchema,
		ReadOnly:        true,
		ConcurrencySafe: true,
	}})
}

func (echoTool) Call(ctx context.Context, in echoInput, _ *tool.UseContext, _ tool.CanUseFunc, onProgress tool.ProgressFunc) (*tool.Result, error) {
	if in.Panic {
		return nil, errors.New("echo: deliberate tool error")
	}
	if in.Wait > 0 {
		select {
		case <-time.After(time.Duration(in.Wait) * time.Millisecond):
		case <-ctx.Done():
			return &tool.Result{Content: "cancelled", IsError: true}, nil
		}
	}
	if onProgress != nil {
		onProgress(tool.Progress{Message: "echoing", Fraction: 0.5})
	}
	if in.Fail {
		return &tool.Result{Content: "echo failed on purpose", IsError: true}, nil
	}
	return &tool.Result{Content: "echo:" + in.Value}, nil
}

func drain(t *testing.T, ctx context.Context, seq event.Seq) []event.Event {
	t.Helper()
	var out []event.Event
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("unexpected stream error: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func kinds(events []event.Event) []event.Kind {
	out := make([]event.Kind, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}

func TestSingleTurnText(t *testing.T) {
	p := &scriptedProvider{scripts: [][]provider.Chunk{textChunks("hello world", provider.StopEndTurn)}}
	eng := New(Options{Provider: p, Model: "test-model", MaxTurns: 4})

	handle, seq := eng.Result(context.Background(), Params{Prompt: "hi"})
	events := drain(t, context.Background(), seq)

	got := strings.Join(toStrings(kinds(events)), ",")
	want := "message_start,text_delta,usage,message_stop"
	if got != want {
		t.Fatalf("event kinds = %s, want %s", got, want)
	}

	res, err := handle.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello world" {
		t.Errorf("text = %q", res.Text)
	}
	if res.Turns != 1 {
		t.Errorf("turns = %d, want 1", res.Turns)
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Stop != provider.StopEndTurn {
		t.Errorf("stop = %q", res.Stop)
	}
	if res.IsError {
		t.Errorf("unexpected error: %+v", res.Failure)
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	call := provider.ToolCall{ID: "toolu_1", Name: "Echo", Input: json.RawMessage(`{"value":"abc"}`)}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(call),
		textChunks("done", provider.StopEndTurn),
	}}
	reg := tool.NewRegistry(newEchoTool())
	eng := New(Options{Provider: p, Tools: reg, Model: "m"})

	handle, seq := eng.Result(context.Background(), Params{Prompt: "echo abc"})
	events := drain(t, context.Background(), seq)

	var sawUse, sawResult bool
	for _, ev := range events {
		switch ev.Kind {
		case event.KindToolUse:
			sawUse = true
			if ev.Tool == nil || ev.Tool.Name != "Echo" {
				t.Fatalf("bad tool_use event: %+v", ev.Tool)
			}
		case event.KindToolResult:
			sawResult = true
			if ev.Result == nil {
				t.Fatal("tool_result with nil payload")
			}
			if ev.Result.Content != "echo:abc" {
				t.Errorf("tool result = %q, want echo:abc", ev.Result.Content)
			}
			if ev.Result.IsError {
				t.Error("tool result marked as an error")
			}
			if ev.Result.ToolUseID != "toolu_1" {
				t.Errorf("tool result id = %q", ev.Result.ToolUseID)
			}
		}
	}
	if !sawUse || !sawResult {
		t.Fatalf("missing events: use=%v result=%v", sawUse, sawResult)
	}

	res, _ := handle.Wait(context.Background())
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2", res.Turns)
	}
	if res.ToolCalls != 1 {
		t.Errorf("tool calls = %d, want 1", res.ToolCalls)
	}
	if !strings.Contains(res.Text, "done") {
		t.Errorf("final text = %q", res.Text)
	}

	// The second request must carry the assistant's tool_use block and the
	// matching tool_result, or the conversation is incoherent.
	if len(p.reqs) != 2 {
		t.Fatalf("provider called %d times, want 2", len(p.reqs))
	}
	second := p.reqs[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request has %d messages, want 3 (user, assistant, tool results)", len(second))
	}
	if second[1].Role != provider.RoleAssistant {
		t.Errorf("message 1 role = %q", second[1].Role)
	}
	var sawToolUseBlock bool
	for _, b := range second[1].Blocks {
		if b.Kind == provider.BlockToolUse {
			sawToolUseBlock = true
			if b.ToolUseID != "toolu_1" || b.ToolName != "Echo" {
				t.Errorf("tool_use block = %+v", b)
			}
		}
	}
	if !sawToolUseBlock {
		t.Error("assistant message did not carry the tool_use block")
	}
	if second[2].Role != provider.RoleUser {
		t.Errorf("message 2 role = %q, want user", second[2].Role)
	}
	if len(second[2].Blocks) != 1 || second[2].Blocks[0].Kind != provider.BlockToolResult {
		t.Errorf("message 2 blocks = %+v", second[2].Blocks)
	}
}

func TestMaxTurnsStopsTheLoop(t *testing.T) {
	call := provider.ToolCall{ID: "t", Name: "Echo", Input: json.RawMessage(`{"value":"x"}`)}
	// The model asks for a tool forever.
	scripts := make([][]provider.Chunk, 10)
	for i := range scripts {
		scripts[i] = toolChunks(call)
	}
	p := &scriptedProvider{scripts: scripts}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m", MaxTurns: 3})

	handle, seq := eng.Result(context.Background(), Params{Prompt: "loop"})
	events := drain(t, context.Background(), seq)

	var failure *event.Failure
	for _, ev := range events {
		if ev.Kind == event.KindFailure {
			failure = ev.Failure
		}
	}
	if failure == nil || failure.Code != event.CodeMaxTurns {
		t.Fatalf("failure = %+v, want %s", failure, event.CodeMaxTurns)
	}
	if !failure.Fatal {
		t.Error("max-turns failure not marked fatal")
	}
	res, _ := handle.Wait(context.Background())
	if res.Turns != 3 {
		t.Errorf("turns = %d, want 3", res.Turns)
	}
	if p.calls != 3 {
		t.Errorf("provider called %d times, want 3", p.calls)
	}
}

func TestProviderErrorIsReported(t *testing.T) {
	p := &scriptedProvider{err: errors.New("connection reset by peer")}
	eng := New(Options{Provider: p, Model: "m"})

	handle, seq := eng.Result(context.Background(), Params{Prompt: "hi"})
	events := drain(t, context.Background(), seq)

	var failure *event.Failure
	for _, ev := range events {
		if ev.Kind == event.KindFailure {
			failure = ev.Failure
		}
	}
	if failure == nil {
		t.Fatal("no failure event")
	}
	if failure.Code != event.CodeProviderError {
		t.Errorf("code = %q, want %q", failure.Code, event.CodeProviderError)
	}
	if !strings.Contains(failure.Message, "connection reset") {
		t.Errorf("message lost the cause: %q", failure.Message)
	}
	res, _ := handle.Wait(context.Background())
	if !res.IsError {
		t.Error("result not marked as an error")
	}
}

func TestTruncatedStreamIsItsOwnCode(t *testing.T) {
	p := &scriptedProvider{err: errors.New("anthropic: stream ended without a stop reason; the response was truncated")}
	eng := New(Options{Provider: p, Model: "m"})
	_, seq := eng.Result(context.Background(), Params{Prompt: "hi"})
	events := drain(t, context.Background(), seq)
	for _, ev := range events {
		if ev.Kind == event.KindFailure && ev.Failure.Code != event.CodeTruncated {
			t.Fatalf("code = %q, want %q", ev.Failure.Code, event.CodeTruncated)
		}
	}
}

func TestNoProviderFailsFast(t *testing.T) {
	eng := New(Options{Model: "m"})
	_, seq := eng.Result(context.Background(), Params{Prompt: "hi"})
	events := drain(t, context.Background(), seq)
	if len(events) != 1 || events[0].Kind != event.KindFailure {
		t.Fatalf("events = %v", kinds(events))
	}
	if events[0].Failure.Code != event.CodeNoProvider {
		t.Errorf("code = %q", events[0].Failure.Code)
	}
}

func TestEmptyPromptFailsFast(t *testing.T) {
	p := &scriptedProvider{scripts: [][]provider.Chunk{textChunks("x", provider.StopEndTurn)}}
	eng := New(Options{Provider: p, Model: "m"})
	_, seq := eng.Result(context.Background(), Params{})
	events := drain(t, context.Background(), seq)
	if len(events) == 0 || events[0].Kind != event.KindFailure || events[0].Failure.Code != event.CodeNoPrompt {
		t.Fatalf("events = %v", kinds(events))
	}
}

func TestUnknownToolProducesAResultNotABrokenTurn(t *testing.T) {
	call := provider.ToolCall{ID: "t1", Name: "NoSuchTool", Input: json.RawMessage(`{}`)}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(call),
		textChunks("recovered", provider.StopEndTurn),
	}}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m"})

	handle, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	events := drain(t, context.Background(), seq)

	var result *event.ToolResult
	for _, ev := range events {
		if ev.Kind == event.KindToolResult {
			result = ev.Result
		}
	}
	if result == nil {
		t.Fatal("no tool_result for an unknown tool")
	}
	if !result.IsError || !strings.Contains(result.Content, "unknown tool") {
		t.Errorf("result = %+v", result)
	}
	// The turn continued: the API requires a tool_result per tool_use, so
	// dropping the call would have made the next request invalid.
	res, _ := handle.Wait(context.Background())
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2", res.Turns)
	}
}

func TestToolGoErrorBecomesAResult(t *testing.T) {
	call := provider.ToolCall{ID: "t1", Name: "Echo", Input: json.RawMessage(`{"panic":true}`)}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(call),
		textChunks("handled", provider.StopEndTurn),
	}}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m"})
	_, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	events := drain(t, context.Background(), seq)

	var result *event.ToolResult
	for _, ev := range events {
		if ev.Kind == event.KindToolResult {
			result = ev.Result
		}
	}
	if result == nil || !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Content, "deliberate tool error") {
		t.Errorf("content = %q", result.Content)
	}
}

func TestToolLevelFailureIsNotATurnFailure(t *testing.T) {
	call := provider.ToolCall{ID: "t1", Name: "Echo", Input: json.RawMessage(`{"fail":true}`)}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(call),
		textChunks("saw the failure", provider.StopEndTurn),
	}}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m"})
	handle, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	events := drain(t, context.Background(), seq)

	for _, ev := range events {
		if ev.Kind == event.KindFailure {
			t.Fatalf("a tool-level failure ended the turn: %+v", ev.Failure)
		}
	}
	res, _ := handle.Wait(context.Background())
	if res.IsError {
		t.Error("tool-level failure marked the whole run as an error")
	}
}

func TestPermissionDenialSkipsTheToolButKeepsTheTurnValid(t *testing.T) {
	call := provider.ToolCall{ID: "t1", Name: "Writey", Input: json.RawMessage(`{"path":"/tmp/x"}`)}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(call),
		textChunks("understood", provider.StopEndTurn),
	}}
	// Plan mode refuses anything that is not read-only. Echo reports itself as
	// read-only, so use a tool that does not.
	reg := tool.NewRegistry(newWriteyTool())
	perm := permission.NewEngine(permission.ModePlan, t.TempDir())

	eng := New(Options{Provider: p, Tools: reg, Model: "m", Permission: perm})
	_, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	events := drain(t, context.Background(), seq)

	var result *event.ToolResult
	for _, ev := range events {
		if ev.Kind == event.KindToolResult {
			result = ev.Result
		}
	}
	if result == nil || !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Content, "plan mode") {
		t.Errorf("denial text = %q", result.Content)
	}
	// The denial must be actionable: a bare "denied" leaves the user with no
	// idea which lever to pull.
	if !strings.Contains(result.Content, "--permission-mode") {
		t.Errorf("denial text does not say what to do: %q", result.Content)
	}
}

// writeyTool is not read-only, so policy modes that forbid writes refuse it.
type writeyTool struct{ testSpec[writeyInput] }

type writeyInput struct {
	Path string `json:"path"`
}

func newWriteyTool() tool.Erased {
	return tool.Adapt[writeyInput](writeyTool{testSpec: testSpec[writeyInput]{
		Name_:  "Writey",
		Schema: `{"type":"object","properties":{"path":{"type":"string"}}}`,
		Desc:   "Pretends to write.",
	}})
}

func (writeyTool) Call(context.Context, writeyInput, *tool.UseContext, tool.CanUseFunc, tool.ProgressFunc) (*tool.Result, error) {
	return &tool.Result{Content: "wrote"}, nil
}

func TestCancellationStopsBeforeTheNextTurn(t *testing.T) {
	call := provider.ToolCall{ID: "t", Name: "Echo", Input: json.RawMessage(`{"value":"x"}`)}
	scripts := make([][]provider.Chunk, 5)
	for i := range scripts {
		scripts[i] = toolChunks(call)
	}
	p := &scriptedProvider{scripts: scripts}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, seq := eng.Result(ctx, Params{Prompt: "go"})

	// Cancel as soon as the first tool result lands.
	count := 0
	for ev, err := range seq {
		if err != nil {
			break
		}
		if ev.Kind == event.KindToolResult {
			count++
			cancel()
		}
	}

	res, _ := handle.Wait(context.Background())
	failure := res.Failure
	if failure == nil || failure.Code != event.CodeCancelled {
		t.Fatalf("failure = %+v, want %s", failure, event.CodeCancelled)
	}
	if p.calls > 2 {
		t.Errorf("provider called %d times after cancellation, want at most 2", p.calls)
	}
}

func TestConsumerStopEndsTheRun(t *testing.T) {
	call := provider.ToolCall{ID: "t", Name: "Echo", Input: json.RawMessage(`{"value":"x"}`)}
	scripts := make([][]provider.Chunk, 5)
	for i := range scripts {
		scripts[i] = toolChunks(call)
	}
	p := &scriptedProvider{scripts: scripts}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m"})

	handle, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	seen := 0
	for ev := range seq {
		_ = ev
		seen++
		break // the consumer walks away after one event
	}
	if seen != 1 {
		t.Fatalf("saw %d events", seen)
	}
	// The producer must have noticed. The summary is what proves it ran to a
	// stop rather than blocking.
	res, err := handle.Wait(context.Background())
	if err != nil {
		t.Fatalf("waiting for the result: %v", err)
	}
	if res.Turns > 1 {
		t.Errorf("loop kept running after the consumer stopped: %d turns", res.Turns)
	}
}

// TestReadOnlyToolsRunInParallel pins the scheduling contract: concurrency-safe
// calls of one batch overlap, so three 120ms calls finish well under 360ms.
func TestReadOnlyToolsRunInParallel(t *testing.T) {
	calls := []provider.ToolCall{
		{ID: "a", Name: "Echo", Input: json.RawMessage(`{"value":"a","waitMs":120}`)},
		{ID: "b", Name: "Echo", Input: json.RawMessage(`{"value":"b","waitMs":120}`)},
		{ID: "c", Name: "Echo", Input: json.RawMessage(`{"value":"c","waitMs":120}`)},
	}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(calls...),
		textChunks("done", provider.StopEndTurn),
	}}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m"})

	start := time.Now()
	handle, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	drain(t, context.Background(), seq)
	elapsed := time.Since(start)

	if elapsed > 300*time.Millisecond {
		t.Errorf("three concurrent 120ms tools took %s; they did not run in parallel", elapsed)
	}
	res, _ := handle.Wait(context.Background())
	if res.ToolCalls != 3 {
		t.Errorf("tool calls = %d, want 3", res.ToolCalls)
	}
}

// TestResultsKeepCallOrder checks that parallel execution does not reorder the
// results: the second request's tool_result blocks must line up with the
// tool_use blocks the model emitted.
func TestResultsKeepCallOrder(t *testing.T) {
	calls := []provider.ToolCall{
		{ID: "slow", Name: "Echo", Input: json.RawMessage(`{"value":"1","waitMs":80}`)},
		{ID: "fast", Name: "Echo", Input: json.RawMessage(`{"value":"2"}`)},
	}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(calls...),
		textChunks("done", provider.StopEndTurn),
	}}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m"})
	_, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	drain(t, context.Background(), seq)

	if len(p.reqs) != 2 {
		t.Fatalf("provider calls = %d", len(p.reqs))
	}
	blocks := p.reqs[1].Messages[len(p.reqs[1].Messages)-1].Blocks
	if len(blocks) != 2 {
		t.Fatalf("tool_result blocks = %d, want 2", len(blocks))
	}
	if blocks[0].ToolUseID != "slow" || blocks[1].ToolUseID != "fast" {
		t.Errorf("results out of order: %q then %q", blocks[0].ToolUseID, blocks[1].ToolUseID)
	}
}

func TestSerialToolsRunOneAtATime(t *testing.T) {
	// Writey is not concurrency-safe, so two of them must serialise.
	calls := []provider.ToolCall{
		{ID: "a", Name: "Writey", Input: json.RawMessage(`{"path":"a"}`)},
		{ID: "b", Name: "Writey", Input: json.RawMessage(`{"path":"b"}`)},
	}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(calls...),
		textChunks("done", provider.StopEndTurn),
	}}
	reg := tool.NewRegistry(newWriteyTool())
	// bypassPermissions so the writey tool is actually run.
	perm := permission.NewEngine(permission.ModeBypassPermissions, t.TempDir())
	eng := New(Options{Provider: p, Tools: reg, Model: "m", Permission: perm})

	handle, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	events := drain(t, context.Background(), seq)

	// Both must still run and report.
	results := 0
	for _, ev := range events {
		if ev.Kind == event.KindToolResult {
			results++
		}
	}
	if results != 2 {
		t.Fatalf("tool results = %d, want 2", results)
	}
	res, _ := handle.Wait(context.Background())
	if res.ToolCalls != 2 {
		t.Errorf("tool calls = %d", res.ToolCalls)
	}
}

func TestSystemPromptAndToolsAreSent(t *testing.T) {
	p := &scriptedProvider{scripts: [][]provider.Chunk{textChunks("ok", provider.StopEndTurn)}}
	eng := New(Options{
		Provider:     p,
		Tools:        tool.NewRegistry(newEchoTool()),
		Model:        "the-model",
		SystemPrompt: "you are a test",
		MaxTokens:    1234,
	})
	_, seq := eng.Result(context.Background(), Params{Prompt: "hi"})
	drain(t, context.Background(), seq)

	req := p.reqs[0]
	if req.System != "you are a test" {
		t.Errorf("system prompt = %q", req.System)
	}
	if req.Model != "the-model" {
		t.Errorf("model = %q", req.Model)
	}
	if req.MaxTokens != 1234 {
		t.Errorf("max tokens = %d", req.MaxTokens)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "Echo" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if len(req.Tools[0].InputSchema) == 0 {
		t.Error("tool advertised without an input schema")
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != provider.RoleUser {
		t.Fatalf("messages = %+v", req.Messages)
	}
}

func TestToolSchemasAreComputedOncePerRun(t *testing.T) {
	call := provider.ToolCall{ID: "t", Name: "Echo", Input: json.RawMessage(`{"value":"x"}`)}
	p := &scriptedProvider{scripts: [][]provider.Chunk{
		toolChunks(call),
		textChunks("done", provider.StopEndTurn),
	}}
	eng := New(Options{Provider: p, Tools: tool.NewRegistry(newEchoTool()), Model: "m"})
	_, seq := eng.Result(context.Background(), Params{Prompt: "go"})
	drain(t, context.Background(), seq)

	if len(p.reqs) != 2 {
		t.Fatalf("provider calls = %d", len(p.reqs))
	}
	if len(p.reqs[0].Tools) != len(p.reqs[1].Tools) {
		t.Errorf("tool list changed between turns: %d then %d", len(p.reqs[0].Tools), len(p.reqs[1].Tools))
	}
}

func TestPriorMessagesArePreserved(t *testing.T) {
	p := &scriptedProvider{scripts: [][]provider.Chunk{textChunks("ok", provider.StopEndTurn)}}
	eng := New(Options{Provider: p, Model: "m"})
	prior := []provider.Message{
		{Role: provider.RoleUser, Blocks: []provider.Block{{Kind: provider.BlockText, Text: "earlier question"}}},
		{Role: provider.RoleAssistant, Blocks: []provider.Block{{Kind: provider.BlockText, Text: "earlier answer"}}},
	}
	_, seq := eng.Result(context.Background(), Params{Prompt: "follow-up", Messages: prior})
	drain(t, context.Background(), seq)

	msgs := p.reqs[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	if msgs[0].Blocks[0].Text != "earlier question" {
		t.Errorf("first message = %+v", msgs[0])
	}
	if msgs[2].Blocks[0].Text != "follow-up" {
		t.Errorf("last message = %+v", msgs[2])
	}
	// The caller's slice must not have been appended to.
	if len(prior) != 2 {
		t.Errorf("the caller's message slice was modified: %d", len(prior))
	}
}

func toStrings(ks []event.Kind) []string {
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, string(k))
	}
	return out
}
