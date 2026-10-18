package agent

// Tool-batch execution tests: the two-phase batch, the parallel ordering
// contract, the sequential degradation, the bounded semaphore, and the
// before/after hooks.
//
// The ordering test is the plan's P2 DoD in its purest form:
// tool_execution_end in COMPLETION order, tool-result messages in SOURCE
// order — verified with a deliberately slow-first batch so the two orders
// differ.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ultrac/internal/ai"
)

// TestParallelCompletionOrderSourceOrder is the dedicated concurrency-timing
// test: the batch is [slow, fast], so completion order (fast, slow) is the
// reverse of source order (slow, fast).
//
// pi: agent-loop.ts:586-660.
func TestParallelCompletionOrderSourceOrder(t *testing.T) {
	slow := behaviorTool("slow", toolBehavior{delay: 80 * time.Millisecond, text: "slow done"})
	fast := behaviorTool("fast", toolBehavior{delay: 5 * time.Millisecond, text: "fast done"})

	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse,
			callBlock("tu_slow", "slow", nil),
			callBlock("tu_fast", "fast", nil)),
		textScript("both done", ai.StopStop),
	}}
	messages, rec := runLoopCase(t, fake,
		[]ai.Message{userMessage("run both")}, []Tool{slow, fast}, baseConfig())

	// tool_execution_end in completion order: fast first.
	ends := rec.toolEnds()
	if len(ends) != 2 {
		t.Fatalf("tool ends = %d, want 2", len(ends))
	}
	if ends[0].ToolCallID != "tu_fast" || ends[1].ToolCallID != "tu_slow" {
		t.Errorf("end order = [%s, %s], want [tu_fast, tu_slow] (completion order)",
			ends[0].ToolCallID, ends[1].ToolCallID)
	}

	// toolResult messages in source order: slow first.
	results := rec.toolResultMessages()
	if len(results) != 2 {
		t.Fatalf("tool result messages = %d, want 2", len(results))
	}
	if results[0].ToolCallID != "tu_slow" || results[1].ToolCallID != "tu_fast" {
		t.Errorf("result order = [%s, %s], want [tu_slow, tu_fast] (source order)",
			results[0].ToolCallID, results[1].ToolCallID)
	}

	// Both starts precede every end (serial preflight).
	names := rec.names()
	firstEnd, lastStart := -1, -1
	for i, n := range names {
		if n == "tool_execution_start" {
			lastStart = i
		}
		if n == "tool_execution_end" && firstEnd == -1 {
			firstEnd = i
		}
	}
	if firstEnd < lastStart {
		t.Errorf("an end event (index %d) preceded a start event (index %d)", firstEnd, lastStart)
	}

	// Transcript order follows the source order too.
	if len(messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(messages))
	}
	if messages[2].ToolCallID != "tu_slow" || messages[3].ToolCallID != "tu_fast" {
		t.Errorf("transcript order = [%s, %s]", messages[2].ToolCallID, messages[3].ToolCallID)
	}
}

// TestSequentialModeInterleaves checks the whole batch degrades to
// start→end→result per call when the loop asks for sequential execution.
//
// pi: agent-loop.ts:519-521.
func TestSequentialModeInterleaves(t *testing.T) {
	a := behaviorTool("a", toolBehavior{text: "a done"})
	b := behaviorTool("b", toolBehavior{text: "b done"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse,
			callBlock("tu_1", "a", nil),
			callBlock("tu_2", "b", nil)),
		textScript("done", ai.StopStop),
	}}
	config := baseConfig()
	config.ToolExecution = ExecSequential
	_, rec := runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{a, b}, config)

	wantNames(t, rec.names(),
		"agent_start", "turn_start",
		"message_start", "message_end",
		"message_start",
		"message_update", "message_update", "message_update", // call a: start/delta/end
		"message_update", "message_update", "message_update", // call b: start/delta/end
		"message_end",
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_end", // result for a
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_end", // result for b
		"turn_end",
		"turn_start",
		"message_start", "message_update", "message_update", "message_update", "message_end",
		"turn_end",
		"agent_end", "agent_settled",
	)
}

// TestSequentialToolDegradesBatch checks one sequential tool forces the whole
// batch serial, even under the parallel default.
//
// pi: agent-loop.ts:516-521.
func TestSequentialToolDegradesBatch(t *testing.T) {
	ordered := behaviorTool("ordered", toolBehavior{
		executionMode: ExecSequential, text: "ordered done",
	})
	plain := behaviorTool("plain", toolBehavior{text: "plain done"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse,
			callBlock("tu_1", "plain", nil),
			callBlock("tu_2", "ordered", nil)),
		textScript("done", ai.StopStop),
	}}
	_, rec := runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{plain, ordered}, baseConfig())

	// Serial shape: each call fully completes (start, end, result) before
	// the next starts.
	wantNames(t, rec.names(),
		"agent_start", "turn_start",
		"message_start", "message_end",
		"message_start",
		"message_update", "message_update", "message_update", // call plain
		"message_update", "message_update", "message_update", // call ordered
		"message_end",
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_end", // plain
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_end", // ordered
		"turn_end",
		"turn_start",
		"message_start", "message_update", "message_update", "message_update", "message_end",
		"turn_end",
		"agent_end", "agent_settled",
	)
}

// TestParallelSemaphoreBound checks the concurrency cap: many parallel calls
// really overlap, but never beyond maxParallelTools.
func TestParallelSemaphoreBound(t *testing.T) {
	const n = 30
	var concurrent, maxSeen int32
	calls := make([]Tool, 0, n)
	scriptCalls := make([]ai.ContentBlock, 0, n)
	for i := 0; i < n; i++ {
		name := "t" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		calls = append(calls, behaviorTool(name, toolBehavior{
			delay:      20 * time.Millisecond,
			concurrent: &concurrent,
			maxSeen:    &maxSeen,
		}))
		scriptCalls = append(scriptCalls, callBlock("tu_"+name, name, nil))
	}
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, scriptCalls...),
		textScript("all done", ai.StopStop),
	}}
	_, rec := runLoopCase(t, fake, []ai.Message{userMessage("flood")}, calls, baseConfig())

	if got := int(atomic.LoadInt32(&maxSeen)); got < 2 {
		t.Errorf("max concurrency = %d, want >= 2 (the batch must actually parallelize)", got)
	}
	if got := int(atomic.LoadInt32(&maxSeen)); got > maxParallelTools {
		t.Errorf("max concurrency = %d, want <= %d", got, maxParallelTools)
	}
	if ends := rec.toolEnds(); len(ends) != n {
		t.Errorf("tool ends = %d, want %d", len(ends), n)
	}
}

// TestToolProgressUpdates checks updates are emitted before the tool's end
// event, in both execution modes.
func TestToolProgressUpdates(t *testing.T) {
	for _, mode := range []ToolExecutionMode{ExecParallel, ExecSequential} {
		t.Run(string(mode), func(t *testing.T) {
			chatty := behaviorTool("chatty", toolBehavior{onUpdate: true, text: "done"})
			fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
				toolCallScript(ai.StopToolUse, callBlock("tu_1", "chatty", nil)),
				textScript("ok", ai.StopStop),
			}}
			config := baseConfig()
			config.ToolExecution = mode
			_, rec := runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{chatty}, config)

			updates := rec.toolUpdates()
			if len(updates) != 1 {
				t.Fatalf("updates = %d, want 1", len(updates))
			}
			if textOf(updates[0].PartialResult.Content) != "chatty progress" {
				t.Errorf("update payload = %q", textOf(updates[0].PartialResult.Content))
			}
			// The update precedes the end event.
			names := rec.names()
			updateAt, endAt := -1, -1
			for i, n := range names {
				if n == "tool_execution_update" {
					updateAt = i
				}
				if n == "tool_execution_end" {
					endAt = i
				}
			}
			if updateAt < 0 || endAt < 0 || updateAt > endAt {
				t.Errorf("update at %d, end at %d; want update before end", updateAt, endAt)
			}
		})
	}
}

// TestBeforeToolCallBlocks checks the block path: the tool never runs and the
// reason reaches the model.
//
// pi: agent-loop.ts:745-755.
func TestBeforeToolCallBlocks(t *testing.T) {
	var ran int32
	tool := behaviorTool("guarded", toolBehavior{calls: &ran, text: "must not run"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "guarded", nil)),
		textScript("blocked, understood", ai.StopStop),
	}}
	config := baseConfig()
	config.BeforeToolCall = func(ctx context.Context, bcx BeforeToolCallContext) BeforeToolCallResult {
		return BeforeToolCallResult{Block: true, Reason: "not allowed here"}
	}
	messages, rec := runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{tool}, config)

	if ran != 0 {
		t.Errorf("tool executions = %d, want 0 (blocked before running)", ran)
	}
	ends := rec.toolEnds()
	if len(ends) != 1 || !ends[0].IsError {
		t.Fatalf("ends = %+v, want one isError end", ends)
	}
	if textOf(ends[0].Result.Content) != "not allowed here" {
		t.Errorf("block reason = %q", textOf(ends[0].Result.Content))
	}
	result := messages[2]
	if !result.IsError || !strings.Contains(ai.MessageText(result), "not allowed here") {
		t.Errorf("tool result = %+v", result)
	}
}

// TestBeforeToolCallTerminate checks a blocked call can also terminate the
// batch through the same all-or-nothing rule.
//
// pi: agent-loop.ts:747-749.
func TestBeforeToolCallTerminate(t *testing.T) {
	tool := behaviorTool("guarded", toolBehavior{text: "must not run"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse,
			callBlock("tu_1", "guarded", nil),
			callBlock("tu_2", "guarded", nil)),
		textScript("never requested", ai.StopStop),
	}}
	config := baseConfig()
	config.BeforeToolCall = func(ctx context.Context, bcx BeforeToolCallContext) BeforeToolCallResult {
		return BeforeToolCallResult{Block: true, Terminate: true}
	}
	_, _ = runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{tool}, config)
	if fake.callCount() != 1 {
		t.Errorf("provider calls = %d, want 1 (all blocked with terminate)", fake.callCount())
	}
}

// TestAfterToolCallOverride checks the field-by-field merge.
//
// pi: agent-loop.ts:883-897.
func TestAfterToolCallOverride(t *testing.T) {
	tool := behaviorTool("logged", toolBehavior{text: "original", isError: true})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "logged", nil)),
		textScript("ok", ai.StopStop),
	}}
	no := false
	config := baseConfig()
	config.AfterToolCall = func(ctx context.Context, acx AfterToolCallContext) AfterToolCallResult {
		if acx.ToolCall.ID != "tu_1" {
			t.Errorf("hook saw tool call %q", acx.ToolCall.ID)
		}
		return AfterToolCallResult{
			Content: []ai.ContentBlock{ai.TextBlock("replaced")},
			IsError: &no,
		}
	}
	messages, _ := runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{tool}, config)

	result := messages[2]
	if ai.MessageText(result) != "replaced" {
		t.Errorf("result text = %q, want replaced", ai.MessageText(result))
	}
	if result.IsError {
		t.Error("isError override to false must apply")
	}
}

// TestAfterToolCallErrorReplacesResult checks a throwing hook (surfaced as an
// error result) replaces the whole outcome.
//
// pi: agent-loop.ts:898-901.
func TestAfterToolCallErrorReplacesResult(t *testing.T) {
	// The Go contract has no error return on hooks; the equivalent failure
	// mode is a hook that returns an explicit error result through the
	// override. Covered here to pin the merge path.
	tool := behaviorTool("logged", toolBehavior{text: "original"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "logged", nil)),
		textScript("ok", ai.StopStop),
	}}
	yes := true
	config := baseConfig()
	config.AfterToolCall = func(ctx context.Context, acx AfterToolCallContext) AfterToolCallResult {
		return AfterToolCallResult{
			Content: []ai.ContentBlock{ai.TextBlock("audit failed")},
			IsError: &yes,
		}
	}
	messages, _ := runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{tool}, config)
	result := messages[2]
	if !result.IsError || ai.MessageText(result) != "audit failed" {
		t.Errorf("result = %+v", result)
	}
}

// TestRunToolCall checks the exported single-call path tools use for nested
// calls: same preflight, no events, no transcript changes.
//
// pi: agent-loop.ts:811-819.
func TestRunToolCall(t *testing.T) {
	read := behaviorTool("read", toolBehavior{text: "contents"})
	ctx := context.Background()
	agentCtx := AgentContext{Tools: []Tool{read}}

	outcome := RunToolCall(ctx, callBlock("tu_1", "read", nil), RunToolCallOptions{
		Tools:            []Tool{read},
		AssistantMessage: ai.Message{Role: ai.RoleAssistant},
		Context:          agentCtx,
	})
	if outcome.IsError || textOf(outcome.Result.Content) != "contents" {
		t.Errorf("outcome = %+v", outcome.Result)
	}
	if outcome.DurationMS == nil {
		t.Error("duration must be measured")
	}

	// Unknown tool: isError outcome, no panic.
	unknown := RunToolCall(ctx, callBlock("tu_2", "nope", nil), RunToolCallOptions{Context: agentCtx})
	if !unknown.IsError || !strings.Contains(textOf(unknown.Result.Content), "not found") {
		t.Errorf("unknown outcome = %+v", unknown.Result)
	}

	// Blocked by the hook: the hook applies to nested calls too.
	blocked := RunToolCall(ctx, callBlock("tu_3", "read", nil), RunToolCallOptions{
		Context: agentCtx,
		BeforeToolCall: func(context.Context, BeforeToolCallContext) BeforeToolCallResult {
			return BeforeToolCallResult{Block: true, Reason: "nested calls are audited"}
		},
	})
	if !blocked.IsError || !strings.Contains(textOf(blocked.Result.Content), "audited") {
		t.Errorf("blocked outcome = %+v", blocked.Result)
	}

	// A tool that throws: isError outcome carrying the message.
	boom := behaviorTool("boom", toolBehavior{err: errors.New("kaboom")})
	failed := RunToolCall(ctx, callBlock("tu_4", "boom", nil), RunToolCallOptions{Context: AgentContext{Tools: []Tool{boom}}})
	if !failed.IsError || !strings.Contains(textOf(failed.Result.Content), "kaboom") {
		t.Errorf("failed outcome = %+v", failed.Result)
	}
}

// TestAllImmediateBatchDoesNotHang is the regression test for the deadlock
// where a batch of immediate outcomes (unknown tools, validation failures)
// spawns no workers and nobody closes the event queue.
func TestAllImmediateBatchDoesNotHang(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse,
			callBlock("tu_1", "ghost1", nil),
			callBlock("tu_2", "ghost2", nil)),
		textScript("recovered", ai.StopStop),
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		messages, rec := runLoopCase(t, fake, []ai.Message{userMessage("go")}, nil, baseConfig())
		if ends := rec.toolEnds(); len(ends) != 2 {
			t.Errorf("tool ends = %d, want 2", len(ends))
		}
		if len(messages) != 5 {
			t.Errorf("messages = %d, want 5", len(messages))
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("batch of immediate outcomes deadlocked")
	}
}

// TestSinkFailureMidDrainAbortsRun checks a failing sink during the parallel
// drain aborts the run without leaking the in-flight workers.
func TestSinkFailureMidDrainAbortsRun(t *testing.T) {
	slow := behaviorTool("slow", toolBehavior{delay: 30 * time.Millisecond, text: "slow done"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "slow", nil)),
		textScript("never requested", ai.StopStop),
	}}
	rec := &recorder{failAt: 6} // fail on the tool_execution_end
	before := goroutineCount()
	_, err := RunAgentLoop(context.Background(), []ai.Message{userMessage("go")},
		declaredContext([]Tool{slow}), baseConfig(), rec.sink, fake.streamFn)
	if err == nil || !strings.Contains(err.Error(), "sink failure") {
		t.Fatalf("error = %v, want the sink failure", err)
	}
	if n := waitForGoroutines(before, 5*time.Second); n > before {
		t.Errorf("goroutines after run = %d, want <= %d", n, before)
	}
}
