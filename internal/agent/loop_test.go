package agent

// Loop-level tests: the Go transcription checks against pi's
// packages/agent/src/agent-loop.ts. Every case runs the loop against the fake
// provider (fakes_test.go) — no real API calls, per the plan's P2 gate.
//
// The expected event sequences below are the plan's P2 DoD, written out:
//
//	单轮:    agent_start turn_start message_start(message_end) message_start
//	         message_update* message_end agent_end
//	多轮工具: … tool_execution_start tool_execution_end message_start
//	         (message_end) turn_start …

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ultrac/internal/ai"
)

// declaredContext builds the realistic Agent-shaped context: a leading system
// message that already declares the runtime tools, so no loadout announcement
// fires and the test can assert on the messages it cares about. (The
// announcement itself has its own tests below.)
func declaredContext(tools []Tool) AgentContext {
	if len(tools) == 0 {
		return AgentContext{}
	}
	head := ai.Message{
		Role:        ai.RoleSystem,
		Content:     []ai.ContentBlock{ai.TextBlock("base prompt")},
		ToolsAdded:  toolDeclarations(tools),
		TimestampMS: time.Now().UnixMilli(),
	}
	return AgentContext{Messages: []ai.Message{head}, Tools: tools}
}

// runLoopCase runs one prompt through RunAgentLoop and returns the produced
// messages plus the recorder.
func runLoopCase(
	t *testing.T,
	fake *fakeProvider,
	prompts []ai.Message,
	tools []Tool,
	config AgentLoopConfig,
) ([]AgentMessage, *recorder) {
	t.Helper()
	rec := &recorder{}
	messages, err := RunAgentLoop(context.Background(), prompts, declaredContext(tools), config, rec.sink, fake.streamFn)
	if err != nil {
		t.Fatalf("RunAgentLoop: %v", err)
	}
	return messages, rec
}

func wantNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event sequence length = %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q\n got: %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}

func baseConfig() AgentLoopConfig {
	return AgentLoopConfig{Model: fakeModel()}
}

// TestSingleTurnEventSequence pins the plan's single-turn DoD:
// message_start(user) → message_start → message_update* → message_end →
// agent_end.
//
// pi: agent-loop.ts:117-124 (prompt events) and :414-456 (streaming).
func TestSingleTurnEventSequence(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("hello", ai.StopStop),
	}}
	messages, rec := runLoopCase(t, fake, []ai.Message{userMessage("hi")}, nil, baseConfig())

	wantNames(t, rec.names(),
		"agent_start", "turn_start",
		"message_start", "message_end", // the user prompt
		"message_start",                                      // assistant
		"message_update", "message_update", "message_update", // text_start/delta/end
		"message_end",
		"turn_end", // the turn closes before the run does
		"agent_end",
	)

	if len(messages) != 2 {
		t.Fatalf("produced %d messages, want 2 (user, assistant)", len(messages))
	}
	if messages[0].Role != ai.RoleUser || ai.MessageText(messages[0]) != "hi" {
		t.Errorf("first message = %+v", messages[0])
	}
	assistant := messages[1]
	if assistant.Role != ai.RoleAssistant || ai.MessageText(assistant) != "hello" {
		t.Errorf("assistant message = %+v", assistant)
	}
	if assistant.StopReason != ai.StopStop {
		t.Errorf("stop reason = %q, want stop", assistant.StopReason)
	}
	// The loop records the requested level on the final message.
	if assistant.ThinkingLevel != "off" {
		t.Errorf("thinking level = %q, want off", assistant.ThinkingLevel)
	}

	// Message payloads: the user prompt as sent, then the streamed
	// assistant message (the partial at message_start, the authoritative
	// one at message_end — provenance lands on the final message).
	seen := rec.messages()
	if len(seen) != 2 {
		t.Fatalf("message_start payloads = %d, want 2", len(seen))
	}
	if seen[0].Role != ai.RoleUser || ai.MessageText(seen[0]) != "hi" {
		t.Errorf("user payload = %+v", seen[0])
	}
	if seen[1].Role != ai.RoleAssistant {
		t.Errorf("assistant payload = %+v", seen[1])
	}
	if assistant.API != fakeAPI || assistant.Provider != fakeProviderID || assistant.Model != fakeModelID {
		t.Errorf("assistant provenance = %s/%s/%s", assistant.API, assistant.Provider, assistant.Model)
	}

	if fake.callCount() != 1 {
		t.Errorf("provider calls = %d, want 1", fake.callCount())
	}
}

// TestAPIKeyResolution checks the per-request key resolution order: the
// GetAPIKey hook wins over the config's static key.
//
// pi: agent-loop.ts:400-407.
func TestAPIKeyResolution(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("ok", ai.StopStop),
	}}
	config := baseConfig()
	config.APIKey = "static-key"
	_, _ = runLoopCase(t, fake, []ai.Message{userMessage("hi")}, nil, config)
	if got := fake.options(0).APIKey; got != "static-key" {
		t.Errorf("api key = %q, want the static key", got)
	}

	fake2 := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("ok", ai.StopStop),
	}}
	config2 := baseConfig()
	config2.APIKey = "static-key"
	config2.GetAPIKey = func(provider string) string {
		if provider != string(ai.ProviderAnthropic) {
			t.Errorf("GetAPIKey saw provider %q", provider)
		}
		return "resolved-key"
	}
	_, _ = runLoopCase(t, fake2, []ai.Message{userMessage("hi")}, nil, config2)
	if got := fake2.options(0).APIKey; got != "resolved-key" {
		t.Errorf("api key = %q, want the resolved key", got)
	}
}

// TestSingleTurnThinkingLevelRecorded checks the requested reasoning level
// lands on the final assistant message.
//
// pi: agent-loop.ts:409 (Object.assign(…, { thinkingLevel: config.reasoning ?? "off" })).
func TestSingleTurnThinkingLevelRecorded(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("thought", ai.StopStop),
	}}
	config := baseConfig()
	config.Reasoning = ai.ThinkHigh
	messages, _ := runLoopCase(t, fake, []ai.Message{userMessage("hi")}, nil, config)
	if messages[1].ThinkingLevel != "high" {
		t.Errorf("thinking level = %q, want high", messages[1].ThinkingLevel)
	}
}

// TestMultiTurnToolLoop pins the multi-turn DoD: tool_execution_start →
// tool_execution_end → toolResult message, with turn_start reappearing.
//
// pi: agent-loop.ts:258-299.
func TestMultiTurnToolLoop(t *testing.T) {
	var readCalls int32
	read := behaviorTool("read", toolBehavior{calls: &readCalls, text: "file contents"})

	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", map[string]any{"path": "a.go"})),
		textScript("done reading", ai.StopStop),
	}}
	messages, rec := runLoopCase(t, fake,
		[]ai.Message{userMessage("read a.go")},
		[]Tool{read}, baseConfig())

	wantNames(t, rec.names(),
		"agent_start", "turn_start",
		"message_start", "message_end", // user prompt
		"message_start",                                      // assistant with the tool call
		"message_update", "message_update", "message_update", // toolcall start/delta/end
		"message_end",
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_end", // toolResult message
		"turn_end",   // first turn closes
		"turn_start", // second turn
		"message_start", "message_update", "message_update", "message_update", "message_end",
		"turn_end",
		"agent_end",
	)

	if readCalls != 1 {
		t.Errorf("read executions = %d, want 1", readCalls)
	}

	// Transcript: user, assistant(toolCall), toolResult, assistant(text).
	if len(messages) != 4 {
		t.Fatalf("produced %d messages, want 4: %+v", len(messages), messages)
	}
	if messages[1].Role != ai.RoleAssistant || len(messages[1].Content) != 1 ||
		messages[1].Content[0].Type != ai.BlockToolCall || messages[1].Content[0].ID != "tu_1" {
		t.Errorf("assistant tool call = %+v", messages[1].Content)
	}
	result := messages[2]
	if result.Role != ai.RoleToolResult || result.ToolCallID != "tu_1" || result.ToolName != "read" {
		t.Fatalf("tool result message = %+v", result)
	}
	if result.IsError {
		t.Error("tool result must not be an error")
	}
	if ai.MessageText(result) != "file contents" {
		t.Errorf("tool result text = %q", ai.MessageText(result))
	}
	if ai.MessageText(messages[3]) != "done reading" {
		t.Errorf("final text = %q", ai.MessageText(messages[3]))
	}

	// The second provider request sees the full transcript: system, user,
	// assistant(toolCall), toolResult.
	second := fake.transcript(1)
	if len(second) != 4 {
		t.Fatalf("second request transcript = %d messages, want 4", len(second))
	}
	if second[3].Role != ai.RoleToolResult || second[3].ToolCallID != "tu_1" {
		t.Errorf("second request toolResult = %+v", second[3])
	}
}

// TestUnknownToolProducesResult checks the load-bearing rule that an unknown
// tool still yields a tool_result — the API requires one result per tool_use.
//
// pi: agent-loop.ts:717-723.
func TestUnknownToolProducesResult(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "nope", nil)),
		textScript("recovered", ai.StopStop),
	}}
	messages, rec := runLoopCase(t, fake,
		[]ai.Message{userMessage("use nope")}, nil, baseConfig())

	if fake.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2 (the loop must continue)", fake.callCount())
	}
	ends := rec.toolEnds()
	if len(ends) != 1 || !ends[0].IsError {
		t.Fatalf("tool ends = %+v, want one isError end", ends)
	}
	if !strings.Contains(ends[0].Result.Content[0].Text, "Tool nope not found") {
		t.Errorf("unknown tool message = %q", ends[0].Result.Content[0].Text)
	}
	result := messages[2]
	if !result.IsError {
		t.Error("tool result message must be marked isError")
	}
}

// TestToolFailureBecomesErrorResult checks a thrown tool error becomes an
// isError result the model can read, not a broken run.
//
// pi: agent-loop.ts:845-853.
func TestToolFailureBecomesErrorResult(t *testing.T) {
	boom := behaviorTool("boom", toolBehavior{err: errors.New("kaboom")})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "boom", nil)),
		textScript("handled", ai.StopStop),
	}}
	messages, _ := runLoopCase(t, fake,
		[]ai.Message{userMessage("fail please")}, []Tool{boom}, baseConfig())

	if fake.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", fake.callCount())
	}
	result := messages[2]
	if !result.IsError || !strings.Contains(ai.MessageText(result), "kaboom") {
		t.Errorf("tool result = %+v", result)
	}
}

// TestLengthStopFailsAllToolCalls pins the safety rule: a length-truncated
// response's tool calls all fail without executing.
//
// pi: agent-loop.ts:264-270 and :478-503.
func TestLengthStopFailsAllToolCalls(t *testing.T) {
	var calls int32
	read := behaviorTool("read", toolBehavior{calls: &calls, text: "should not run"})

	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopLength,
			callBlock("tu_1", "read", map[string]any{"path": "a.go"}),
			callBlock("tu_2", "read", map[string]any{"path": "b.go"})),
		textScript("retrying", ai.StopStop),
	}}
	messages, rec := runLoopCase(t, fake,
		[]ai.Message{userMessage("read both")}, []Tool{read}, baseConfig())

	if calls != 0 {
		t.Errorf("tool executions = %d, want 0 (truncated batch must not run)", calls)
	}
	ends := rec.toolEnds()
	if len(ends) != 2 {
		t.Fatalf("tool ends = %d, want 2", len(ends))
	}
	for _, e := range ends {
		if !e.IsError {
			t.Errorf("end for %s is not an error", e.ToolCallID)
		}
		text := e.Result.Content[0].Text
		if !strings.Contains(text, "hit the output token limit") ||
			!strings.Contains(text, "arguments may be truncated") {
			t.Errorf("truncation message = %q", text)
		}
	}
	if fake.callCount() != 2 {
		t.Errorf("provider calls = %d, want 2 (model gets to re-issue)", fake.callCount())
	}
	if len(messages) != 5 {
		t.Errorf("messages = %d, want 5 (user, assistant, 2 results, assistant)", len(messages))
	}
}

// TestTerminateBatchSemantics pins the all-or-nothing rule: the loop stops
// early only when every finalized result in the batch sets Terminate.
//
// pi: agent-loop.ts:690-692.
func TestTerminateBatchSemantics(t *testing.T) {
	t.Run("all terminate stops the loop", func(t *testing.T) {
		stop := behaviorTool("stop", toolBehavior{terminate: true})
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			toolCallScript(ai.StopToolUse,
				callBlock("tu_1", "stop", nil),
				callBlock("tu_2", "stop", nil)),
			textScript("must not be requested", ai.StopStop),
		}}
		_, _ = runLoopCase(t, fake,
			[]ai.Message{userMessage("stop")}, []Tool{stop}, baseConfig())
		if fake.callCount() != 1 {
			t.Errorf("provider calls = %d, want 1 (batch terminated)", fake.callCount())
		}
	})

	t.Run("one terminate among many continues", func(t *testing.T) {
		stop := behaviorTool("stop", toolBehavior{terminate: true})
		goOn := behaviorTool("go", toolBehavior{})
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			toolCallScript(ai.StopToolUse,
				callBlock("tu_1", "stop", nil),
				callBlock("tu_2", "go", nil)),
			textScript("continued", ai.StopStop),
		}}
		_, _ = runLoopCase(t, fake,
			[]ai.Message{userMessage("mixed")}, []Tool{stop, goOn}, baseConfig())
		if fake.callCount() != 2 {
			t.Errorf("provider calls = %d, want 2 (no early termination)", fake.callCount())
		}
	})
}

// TestSteeringInjectedAfterTools checks steering messages are picked up
// between turns and announced with their own message events.
//
// pi: agent-loop.ts:176 (initial poll) and :295 (post-turn poll).
func TestSteeringInjectedAfterTools(t *testing.T) {
	read := behaviorTool("read", toolBehavior{text: "contents"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", nil)),
		textScript("acknowledged", ai.StopStop),
	}}

	steering := userMessage("actually, also check b.go")
	config := baseConfig()
	polls := 0
	config.GetSteeringMessages = func(ctx context.Context) []ai.Message {
		polls++
		if polls == 1 {
			return nil // nothing queued before the first request
		}
		if polls == 2 {
			return []ai.Message{steering} // queued while the tool ran
		}
		return nil
	}

	messages, rec := runLoopCase(t, fake,
		[]ai.Message{userMessage("read a.go")}, []Tool{read}, config)

	// The steered message lands between the toolResult and the second
	// assistant response.
	if len(messages) != 5 {
		t.Fatalf("messages = %d, want 5: %+v", len(messages), messages)
	}
	if ai.MessageText(messages[3]) != "actually, also check b.go" {
		t.Errorf("steered message at position 3 = %q", ai.MessageText(messages[3]))
	}
	if messages[3].Role != ai.RoleUser {
		t.Errorf("steered message role = %q", messages[3].Role)
	}

	names := rec.names()
	// turn_start must reappear for the second turn, after the toolResult.
	if !containsSeq(names, "turn_start", "message_start", "message_end", "message_start") {
		t.Errorf("second turn_start missing from %v", names)
	}
}

// TestFollowUpInjectedAtStop checks follow-up messages arrive after the agent
// would otherwise stop, driving one more turn.
//
// pi: agent-loop.ts:301-314.
func TestFollowUpInjectedAtStop(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("first answer", ai.StopStop),
		textScript("follow-up answer", ai.StopStop),
	}}
	followUp := userMessage("and one more thing")
	config := baseConfig()
	config.GetFollowUpMessages = func(ctx context.Context) []ai.Message {
		if fake.callCount() == 1 {
			return []ai.Message{followUp}
		}
		return nil
	}

	messages, _ := runLoopCase(t, fake, []ai.Message{userMessage("question")}, nil, config)

	if fake.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", fake.callCount())
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(messages), messages)
	}
	if ai.MessageText(messages[2]) != "and one more thing" {
		t.Errorf("follow-up at position 2 = %q", ai.MessageText(messages[2]))
	}
	if ai.MessageText(messages[3]) != "follow-up answer" {
		t.Errorf("final text = %q", ai.MessageText(messages[3]))
	}
}

// TestSteeringQueueModes checks both drain modes at the loop boundary.
//
// pi: packages/agent/src/types.ts:49-55.
func TestSteeringQueueModes(t *testing.T) {
	build := func(mode QueueMode, queued []ai.Message) (*fakeProvider, []ai.Message) {
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			textScript("first", ai.StopStop),
			textScript("second", ai.StopStop),
		}}
		config := baseConfig()
		queue := &pendingMessageQueue{mode: mode}
		for _, m := range queued {
			queue.enqueue(m)
		}
		config.GetSteeringMessages = func(ctx context.Context) []ai.Message {
			return queue.drain()
		}
		messages, _ := runLoopCase(t, fake, []ai.Message{userMessage("go")}, nil, config)
		return fake, messages
	}

	t.Run("all drains everything before the first request", func(t *testing.T) {
		fake, messages := build(QueueAll, []ai.Message{userMessage("m1"), userMessage("m2")})
		// prompt + 2 steering + 1 assistant response; one provider request.
		if len(messages) != 4 {
			t.Fatalf("messages = %d, want 4: %+v", len(messages), messages)
		}
		if ai.MessageText(messages[1]) != "m1" || ai.MessageText(messages[2]) != "m2" {
			t.Errorf("steering order = %q, %q", ai.MessageText(messages[1]), ai.MessageText(messages[2]))
		}
		if fake.callCount() != 1 {
			t.Errorf("provider calls = %d, want 1", fake.callCount())
		}
	})

	t.Run("one-at-a-time drains one per poll", func(t *testing.T) {
		fake, messages := build(QueueOneAtATime, []ai.Message{userMessage("m1"), userMessage("m2")})
		// prompt, m1, assistant, m2, assistant; two provider requests.
		if len(messages) != 5 {
			t.Fatalf("messages = %d, want 5: %+v", len(messages), messages)
		}
		if ai.MessageText(messages[1]) != "m1" || ai.MessageText(messages[3]) != "m2" {
			t.Errorf("drain positions = %q, %q", ai.MessageText(messages[1]), ai.MessageText(messages[3]))
		}
		if fake.callCount() != 2 {
			t.Errorf("provider calls = %d, want 2", fake.callCount())
		}
	})
}

// TestFinishTurnDecisions checks both control-flow decisions.
//
// pi: agent-loop.ts:286-298.
func TestFinishTurnDecisions(t *testing.T) {
	t.Run("end stops after the turn", func(t *testing.T) {
		read := behaviorTool("read", toolBehavior{text: "contents"})
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", nil)),
			textScript("never requested", ai.StopStop),
		}}
		config := baseConfig()
		config.FinishTurn = func(ctx context.Context, turn AgentTurnContext) TurnDecision {
			return TurnEnd
		}
		_, rec := runLoopCase(t, fake, []ai.Message{userMessage("read")}, []Tool{read}, config)
		if fake.callCount() != 1 {
			t.Errorf("provider calls = %d, want 1", fake.callCount())
		}
		// The tool batch still ran; the decision applies after it.
		if !contains(rec.names(), "tool_execution_end") {
			t.Error("tool batch must still execute before finishTurn")
		}
	})

	t.Run("continue forces one more request", func(t *testing.T) {
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			textScript("first", ai.StopStop),
			textScript("second", ai.StopStop),
		}}
		config := baseConfig()
		continued := false
		config.FinishTurn = func(ctx context.Context, turn AgentTurnContext) TurnDecision {
			if !continued {
				continued = true
				return TurnContinue
			}
			return TurnDefault
		}
		messages, _ := runLoopCase(t, fake, []ai.Message{userMessage("go")}, nil, config)
		if fake.callCount() != 2 {
			t.Errorf("provider calls = %d, want 2", fake.callCount())
		}
		if len(messages) != 3 {
			t.Errorf("messages = %d, want 3", len(messages))
		}
	})
}

// TestPrepareRequestSwapsModel checks the per-request model replacement
// reaches the provider.
//
// pi: agent-loop.ts:219-239.
func TestPrepareRequestSwapsModel(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", nil)),
		textScript("switched", ai.StopStop),
	}}
	read := behaviorTool("read", toolBehavior{text: "contents"})
	swapped := fakeModel()
	swapped.ID = "swapped-model"
	config := baseConfig()
	config.PrepareRequest = func(ctx context.Context, req PrepareRequestContext) AgentRequestUpdate {
		// Swap once the first turn's tool result is in the transcript.
		hasResult := false
		for _, m := range req.Context.Messages {
			if m.Role == ai.RoleToolResult {
				hasResult = true
			}
		}
		if hasResult {
			return AgentRequestUpdate{Model: &swapped}
		}
		return AgentRequestUpdate{}
	}
	_, _ = runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{read}, config)

	if got := fake.model(0).ID; got != fakeModelID {
		t.Errorf("first model = %q, want %q", got, fakeModelID)
	}
	if got := fake.model(1).ID; got != "swapped-model" {
		t.Errorf("second model = %q, want swapped-model", got)
	}
}

// TestPrepareNextTurnInjectsMessages checks messages injected by
// prepareNextTurn are announced and land in the transcript.
//
// pi: agent-loop.ts:185-216.
func TestPrepareNextTurnInjectsMessages(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", nil)),
		textScript("after injection", ai.StopStop),
	}}
	read := behaviorTool("read", toolBehavior{text: "contents"})
	injected := userMessage("injected context")
	config := baseConfig()
	config.PrepareNextTurn = func(ctx context.Context, turn PrepareNextTurnContext) AgentLoopTurnUpdate {
		if len(turn.ToolResults) == 0 {
			return AgentLoopTurnUpdate{} // first turn: nothing to inject
		}
		return AgentLoopTurnUpdate{Messages: []ai.Message{injected}}
	}
	messages, _ := runLoopCase(t, fake, []ai.Message{userMessage("go")}, []Tool{read}, config)

	if len(messages) != 5 {
		t.Fatalf("messages = %d, want 5: %+v", len(messages), messages)
	}
	if ai.MessageText(messages[3]) != "injected context" {
		t.Errorf("injected message at position 3 = %q", ai.MessageText(messages[3]))
	}
}

// TestDeclareToolChanges unit-tests the tool loadout announcement.
//
// pi: agent-loop.ts:333-375.
func TestDeclareToolChanges(t *testing.T) {
	readDecl := ai.Tool{Name: "read", Description: "read", Parameters: []byte(`{"type":"object"}`)}
	readV2 := ai.Tool{Name: "read", Description: "read v2", Parameters: []byte(`{"type":"object"}`)}
	bashDecl := ai.Tool{Name: "bash", Description: "bash", Parameters: []byte(`{"type":"object"}`)}
	runtimeTools := []Tool{
		Erase(&AgentTool[map[string]any]{
			Tool: readV2,
			Execute: func(ctx context.Context, id string, in map[string]any, u func(Result)) (Result, error) {
				return Result{}, nil
			},
		}),
		Erase(&AgentTool[map[string]any]{
			Tool: bashDecl,
			Execute: func(ctx context.Context, id string, in map[string]any, u func(Result)) (Result, error) {
				return Result{}, nil
			},
		}),
	}

	systemWith := func(tools ...ai.Tool) ai.Message {
		m := ai.Message{Role: ai.RoleSystem, Content: []ai.ContentBlock{ai.TextBlock("base")}}
		if len(tools) > 0 {
			m.ToolsAdded = tools
		}
		return m
	}

	t.Run("no delta leaves messages untouched", func(t *testing.T) {
		ctx := AgentContext{
			Messages: []ai.Message{systemWith(readV2, bashDecl)},
			Tools:    runtimeTools,
		}
		pending := []ai.Message{userMessage("hi")}
		got := declareToolChanges(ctx, pending)
		if len(got) != 1 || ai.MessageText(got[0]) != "hi" {
			t.Fatalf("got %+v, want the pending message unchanged", got)
		}
	})

	t.Run("missing tool is announced before the first non-system message", func(t *testing.T) {
		ctx := AgentContext{
			Messages: []ai.Message{systemWith(readV2)}, // bash not declared
			Tools:    runtimeTools,
		}
		pending := []ai.Message{userMessage("hi")}
		got := declareToolChanges(ctx, pending)
		if len(got) != 2 {
			t.Fatalf("got %d messages, want 2 (announcement + pending)", len(got))
		}
		announce := got[0]
		if announce.Role != ai.RoleSystem {
			t.Fatalf("first message role = %q, want system", announce.Role)
		}
		if len(announce.ToolsAdded) != 1 || announce.ToolsAdded[0].Name != "bash" {
			t.Fatalf("toolsAdded = %+v, want [bash]", announce.ToolsAdded)
		}
		if len(announce.ToolsRemoved) != 0 {
			t.Errorf("toolsRemoved = %+v, want empty", announce.ToolsRemoved)
		}
	})

	t.Run("changed definition is a removal plus an addition", func(t *testing.T) {
		ctx := AgentContext{
			Messages: []ai.Message{systemWith(readDecl)}, // old description
			Tools:    runtimeTools,
		}
		got := declareToolChanges(ctx, []ai.Message{userMessage("hi")})
		announce := got[0]
		if len(announce.ToolsRemoved) != 1 || announce.ToolsRemoved[0].Name != "read" {
			t.Errorf("toolsRemoved = %+v, want [read]", announce.ToolsRemoved)
		}
		names := make([]string, 0, len(announce.ToolsAdded))
		for _, tool := range announce.ToolsAdded {
			names = append(names, tool.Name)
		}
		if strings.Join(names, ",") != "read,bash" {
			t.Errorf("toolsAdded = %v, want [read bash]", names)
		}
	})

	t.Run("pending system message intent is replaced by the real delta", func(t *testing.T) {
		ctx := AgentContext{
			Messages: []ai.Message{systemWith(readV2, bashDecl)}, // fully declared
			Tools:    runtimeTools,
		}
		// The pending system message claims a tool that does not exist at
		// runtime; the announcement must replace the intent, not merge it.
		ghost := systemWith(readV2, bashDecl, ai.Tool{Name: "ghost", Description: "gone"})
		got := declareToolChanges(ctx, []ai.Message{ghost, userMessage("hi")})
		if len(got) != 2 {
			t.Fatalf("got %d messages, want 2", len(got))
		}
		if got[0].Role != ai.RoleSystem {
			t.Fatalf("first message role = %q, want system", got[0].Role)
		}
		if len(got[0].ToolsAdded) != 0 || len(got[0].ToolsRemoved) != 0 {
			t.Errorf("announcement = %+v, want no tool fields (delta is empty)", got[0])
		}
		if ai.MessageText(got[1]) != "hi" {
			t.Errorf("pending user message moved: %q", ai.MessageText(got[1]))
		}
	})

	t.Run("all-system pending is patched in place", func(t *testing.T) {
		// With no non-system pending message there is no insertion point, so
		// the announcement is patched onto the pending system message itself
		// (pi maps over the baseline instead of appending).
		ctx := AgentContext{
			Messages: []ai.Message{systemWith(readV2)},
			Tools:    runtimeTools,
		}
		got := declareToolChanges(ctx, []ai.Message{systemWith()})
		if len(got) != 1 {
			t.Fatalf("got %d messages, want 1", len(got))
		}
		if got[0].Role != ai.RoleSystem || len(got[0].ToolsAdded) != 1 || got[0].ToolsAdded[0].Name != "bash" {
			t.Errorf("patched announcement = %+v", got[0])
		}
	})
}

// TestTransformContextAndConvertToLLM checks both conversion hooks run at the
// LLM boundary.
//
// pi: agent-loop.ts:389-395.
func TestTransformContextAndConvertToLLM(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("ok", ai.StopStop),
	}}
	config := baseConfig()
	config.TransformContext = func(ctx context.Context, messages []ai.Message) []ai.Message {
		// Drop everything before the last user message.
		return messages[len(messages)-1:]
	}
	config.ConvertToLLM = func(messages []ai.Message) []ai.Message {
		out := make([]ai.Message, 0, len(messages)+1)
		out = append(out, ai.NewTextMessage(ai.RoleSystem, "converted"))
		out = append(out, messages...)
		return out
	}
	_, _ = runLoopCase(t, fake, []ai.Message{userMessage("only me")}, nil, config)

	transcript := fake.transcript(0)
	if len(transcript) != 2 {
		t.Fatalf("transcript = %d messages, want 2 (system + user)", len(transcript))
	}
	if transcript[0].Role != ai.RoleSystem || ai.MessageText(transcript[0]) != "converted" {
		t.Errorf("first message = %+v", transcript[0])
	}
}

// TestStreamFailurePropagates checks an error response is a hard exit with
// turn_end and agent_end.
//
// pi: agent-loop.ts:245-256.
func TestStreamFailurePropagates(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		errorScript(ai.StopError, "upstream exploded"),
	}}
	rec := &recorder{}
	_, err := RunAgentLoop(context.Background(), []ai.Message{userMessage("hi")},
		AgentContext{}, baseConfig(), rec.sink, fake.streamFn)
	if err != nil {
		t.Fatalf("RunAgentLoop returned %v, want nil (the failure travels as an event)", err)
	}
	names := rec.names()
	if names[len(names)-1] != "agent_end" {
		t.Errorf("last event = %q, want agent_end", names[len(names)-1])
	}
	if !containsSeq(names, "message_start", "message_end", "turn_end", "agent_end") {
		t.Errorf("failure tail missing from %v", names)
	}
	// The error message must be visible to consumers.
	found := false
	for _, ev := range rec.snapshot() {
		if end, ok := ev.(EvTurnEnd); ok && end.Message.ErrorMessage == "upstream exploded" {
			found = true
		}
	}
	if !found {
		t.Error("turn_end must carry the failed assistant message")
	}
}

// TestContinueValidations checks RunAgentLoopContinue's preconditions.
//
// pi: agent-loop.ts:77-83.
func TestContinueValidations(t *testing.T) {
	fake := &fakeProvider{}
	config := baseConfig()

	_, err := RunAgentLoopContinue(context.Background(), AgentContext{}, config, func(AgentEvent) error { return nil }, fake.streamFn)
	if err == nil || !strings.Contains(err.Error(), "no messages") {
		t.Errorf("empty context error = %v", err)
	}

	_, err = RunAgentLoopContinue(context.Background(), AgentContext{
		Messages: []ai.Message{{Role: ai.RoleAssistant}},
	}, config, func(AgentEvent) error { return nil }, fake.streamFn)
	if err == nil || !strings.Contains(err.Error(), "assistant") {
		t.Errorf("assistant-last error = %v", err)
	}
}

// TestNilStreamFnRejected checks the loop surfaces a nil stream instead of
// panicking.
func TestNilStreamFnRejected(t *testing.T) {
	rec := &recorder{}
	_, err := RunAgentLoop(context.Background(), []ai.Message{userMessage("hi")},
		AgentContext{}, baseConfig(), rec.sink,
		func(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts ai.StreamOptions) ai.Stream {
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "nil stream") {
		t.Errorf("error = %v, want a nil-stream report", err)
	}
}

// --- helpers -----------------------------------------------------------------

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func containsSeq(names []string, seq ...string) bool {
	if len(seq) == 0 {
		return true
	}
	for i := 0; i+len(seq) <= len(names); i++ {
		match := true
		for j, s := range seq {
			if names[i+j] != s {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
