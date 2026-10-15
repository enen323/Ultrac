package agent

// Agent state-machine tests: the Go transcription of pi's
// packages/agent/src/agent.ts — lifecycle, queues, state reduction, abort,
// reset, and the failure path.
//
// Like the loop tests, everything runs against the fake provider.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ultrac/internal/ai"
)

// newTestAgent builds an Agent over the fake provider with the given tools.
func newTestAgent(fake *fakeProvider, tools ...Tool) *Agent {
	return NewAgent(AgentOptions{
		InitialState: &AgentInitialState{
			SystemPrompt: "test prompt",
			Model:        modelPtr(fakeModel()),
			Tools:        tools,
		},
		StreamFn: fake.streamFn,
	})
}

func modelPtr(m ai.Model) *ai.Model { return &m }

// TestAgentPromptLifecycle checks the full run through the public API: the
// transcript accumulates, the state settles, and the event order is the
// documented one.
//
// pi: agent.ts:432-446 (runPromptMessages) and :565-612 (processEvents).
func TestAgentPromptLifecycle(t *testing.T) {
	read := behaviorTool("read", toolBehavior{text: "contents"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", nil)),
		textScript("done", ai.StopStop),
	}}
	agent := newTestAgent(fake, read)

	var mu sync.Mutex
	var names []string
	unsubscribe := agent.Subscribe(func(ctx context.Context, ev AgentEvent) error {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, EventName(ev))
		return nil
	})
	defer unsubscribe()

	if err := agent.Prompt(context.Background(), userMessage("read it")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), names...)
	mu.Unlock()
	wantNames(t, got,
		"agent_start", "turn_start",
		"message_start", "message_end", // user
		"message_start", "message_update", "message_update", "message_update", "message_end",
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_end", // toolResult
		"turn_end", "turn_start",
		"message_start", "message_update", "message_update", "message_update", "message_end",
		"turn_end", "agent_end",
	)

	state := agent.State()
	if state.IsStreaming {
		t.Error("agent must be idle after the run")
	}
	if state.ErrorMessage != "" {
		t.Errorf("error message = %q, want empty", state.ErrorMessage)
	}
	if len(state.Messages) != 5 {
		t.Fatalf("transcript = %d messages, want 5: %+v", len(state.Messages), state.Messages)
	}
	// system (seed), user, assistant(toolCall), toolResult, assistant(text).
	if state.Messages[0].Role != ai.RoleSystem || ai.MessageText(state.Messages[0]) != "test prompt" {
		t.Errorf("leading system message = %+v", state.Messages[0])
	}
	if ai.MessageText(state.Messages[4]) != "done" {
		t.Errorf("final message = %q", ai.MessageText(state.Messages[4]))
	}
	if state.SystemPrompt != "test prompt" {
		t.Errorf("state system prompt = %q", state.SystemPrompt)
	}
	if len(state.Tools) != 1 || state.Tools[0].Name() != "read" {
		t.Errorf("state tools = %v", state.Tools)
	}
}

// TestAgentInitialStateSeedsSystemMessage checks the leading system message
// is built from the seed prompt and tools.
//
// pi: agent.ts:85-86 (createInitialSystemMessage).
func TestAgentInitialStateSeedsSystemMessage(t *testing.T) {
	read := behaviorTool("read", toolBehavior{})
	fake := &fakeProvider{}
	agent := NewAgent(AgentOptions{
		InitialState: &AgentInitialState{
			SystemPrompt: "be nice",
			Tools:        []Tool{read},
		},
		StreamFn: fake.streamFn,
	})
	state := agent.State()
	if len(state.Messages) != 1 || state.Messages[0].Role != ai.RoleSystem {
		t.Fatalf("seeded transcript = %+v, want one system message", state.Messages)
	}
	if ai.MessageText(state.Messages[0]) != "be nice" {
		t.Errorf("system prompt = %q", ai.MessageText(state.Messages[0]))
	}
	tools := ai.GetCurrentTools(state.Messages)
	if len(tools) != 1 || tools[0].Name != "read" {
		t.Errorf("declared tools = %+v, want [read]", tools)
	}

	// A seeded transcript that already starts with a system message keeps it.
	seeded := ai.NewTextMessage(ai.RoleSystem, "mine")
	seeded.TimestampMS = 5
	agent2 := NewAgent(AgentOptions{
		InitialState: &AgentInitialState{
			SystemPrompt: "ignored",
			Messages:     []ai.Message{seeded},
		},
		StreamFn: fake.streamFn,
	})
	if got := agent2.State().Messages; len(got) != 1 || ai.MessageText(got[0]) != "mine" {
		t.Errorf("seeded system message was replaced: %+v", got)
	}
}

// TestAgentToolLoadoutAnnouncement checks the end-to-end announcement: a
// transcript declaring fewer tools than the runtime gets a system message
// announcing the delta before the first request.
//
// pi: agent-loop.ts:333-363 via the Agent's initial state.
func TestAgentToolLoadoutAnnouncement(t *testing.T) {
	read := behaviorTool("read", toolBehavior{})
	write := behaviorTool("write", toolBehavior{})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("ok", ai.StopStop),
	}}
	// Seed a transcript whose system message declares only `read`.
	seeded := ai.NewTextMessage(ai.RoleSystem, "partial loadout")
	seeded.ToolsAdded = toolDeclarations([]Tool{read})
	seeded.TimestampMS = 5
	agent := NewAgent(AgentOptions{
		InitialState: &AgentInitialState{
			Messages: []ai.Message{seeded},
			Tools:    []Tool{read, write},
			Model:    modelPtr(fakeModel()),
		},
		StreamFn: fake.streamFn,
	})

	if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// The provider's first request declares both tools after replay.
	declared := ai.GetCurrentTools(fake.transcript(0))
	if len(declared) != 2 {
		t.Fatalf("declared tools in request = %+v, want 2", declared)
	}
	names := map[string]bool{}
	for _, tool := range declared {
		names[tool.Name] = true
	}
	if !names["read"] || !names["write"] {
		t.Errorf("declared tools = %v, want read and write", names)
	}

	// The announcement is a real message in the transcript, positioned
	// before the user prompt.
	messages := agent.State().Messages
	if len(messages) != 4 {
		t.Fatalf("transcript = %d messages, want 4: %+v", len(messages), messages)
	}
	if messages[1].Role != ai.RoleSystem {
		t.Fatalf("announcement missing: %+v", messages[1])
	}
	if len(messages[1].ToolsAdded) != 1 || messages[1].ToolsAdded[0].Name != "write" {
		t.Errorf("announcement = %+v, want toolsAdded [write]", messages[1])
	}
	if messages[2].Role != ai.RoleUser {
		t.Errorf("user prompt must follow the announcement: %+v", messages[2])
	}
}

// TestAgentPromptConcurrentRejected checks the single-run rule.
func TestAgentPromptConcurrentRejected(t *testing.T) {
	gate := make(chan struct{})
	fake := &fakeProvider{
		scripts: [][]ai.AssistantEvent{textScript("ok", ai.StopStop)},
		gate:    gate,
	}
	agent := newTestAgent(fake)

	done := make(chan error, 1)
	go func() { done <- agent.Prompt(context.Background(), userMessage("go")) }()

	// Wait until the provider call is in flight (blocked on the gate).
	waitFor(t, 2*time.Second, func() bool { return fake.callCount() == 1 })

	if err := agent.Prompt(context.Background(), userMessage("again")); err == nil {
		t.Error("second Prompt must fail while a run is active")
	}
	if err := agent.Continue(context.Background()); err == nil {
		t.Error("Continue must fail while a run is active")
	}
	if err := agent.Reset(); err == nil {
		t.Error("Reset must fail while a run is active")
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
}

// TestAgentPromptRequiresMessages checks the empty-prompt guard.
func TestAgentPromptRequiresMessages(t *testing.T) {
	agent := newTestAgent(&fakeProvider{})
	if err := agent.Prompt(context.Background()); err == nil {
		t.Error("Prompt with no messages must fail")
	}
}

// TestAgentSteerAndFollowUp checks both queues through the public API, in
// both drain modes.
//
// pi: agent.ts:299-306 (steer/followUp) and :496-503 (queue drains).
func TestAgentSteerAndFollowUp(t *testing.T) {
	// Both modes inject pre-queued messages before the first request (the
	// loop polls steering at start); the difference is the ORDER when two
	// messages are queued: "all" puts both before the request,
	// "one-at-a-time" delivers the second at the next drain point.
	steerScripts := func() [][]ai.AssistantEvent {
		return [][]ai.AssistantEvent{
			toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", nil)),
			textScript("second", ai.StopStop),
		}
	}

	t.Run("one-at-a-time spreads the drains", func(t *testing.T) {
		fake := &fakeProvider{scripts: steerScripts()}
		agent := newTestAgent(fake, behaviorTool("read", toolBehavior{text: "contents"}))
		agent.Steer(userMessage("m1"))
		agent.Steer(userMessage("m2"))
		if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		messages := agent.State().Messages
		// system, user, m1, assistant, toolResult, m2, assistant.
		if len(messages) != 7 {
			t.Fatalf("transcript = %d messages, want 7: %+v", len(messages), messages)
		}
		if ai.MessageText(messages[2]) != "m1" {
			t.Errorf("m1 position = %q", ai.MessageText(messages[2]))
		}
		if ai.MessageText(messages[5]) != "m2" {
			t.Errorf("m2 position = %q, want after the tool result", ai.MessageText(messages[5]))
		}
		if fake.callCount() != 2 {
			t.Errorf("provider calls = %d, want 2", fake.callCount())
		}
	})

	t.Run("all drains both before the first request", func(t *testing.T) {
		fake := &fakeProvider{scripts: steerScripts()}
		agent := newTestAgent(fake, behaviorTool("read", toolBehavior{text: "contents"}))
		agent.SetSteeringMode(QueueAll)
		agent.Steer(userMessage("m1"))
		agent.Steer(userMessage("m2"))
		if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		messages := agent.State().Messages
		// system, user, m1, m2, assistant, toolResult, assistant.
		if len(messages) != 7 {
			t.Fatalf("transcript = %d messages, want 7: %+v", len(messages), messages)
		}
		if ai.MessageText(messages[2]) != "m1" || ai.MessageText(messages[3]) != "m2" {
			t.Errorf("steering order = %q, %q", ai.MessageText(messages[2]), ai.MessageText(messages[3]))
		}
		if fake.callCount() != 2 {
			t.Errorf("provider calls = %d, want 2", fake.callCount())
		}
	})

	t.Run("follow-up injects after the agent would stop", func(t *testing.T) {
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			textScript("first", ai.StopStop),
			textScript("followed up", ai.StopStop),
		}}
		agent := newTestAgent(fake)
		agent.FollowUp(userMessage("one more thing"))
		if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if fake.callCount() != 2 {
			t.Errorf("provider calls = %d, want 2", fake.callCount())
		}
		messages := agent.State().Messages
		if ai.MessageText(messages[len(messages)-2]) != "one more thing" {
			t.Errorf("follow-up position = %q", ai.MessageText(messages[len(messages)-2]))
		}
		if ai.MessageText(messages[len(messages)-1]) != "followed up" {
			t.Errorf("final text = %q", ai.MessageText(messages[len(messages)-1]))
		}
	})
}

// TestAgentSteerDuringRun checks a message queued while the provider call is
// in flight is picked up at the next drain point.
func TestAgentSteerDuringRun(t *testing.T) {
	read := behaviorTool("read", toolBehavior{text: "contents"})
	gate := make(chan struct{})
	fake := &fakeProvider{
		scripts: [][]ai.AssistantEvent{
			toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", nil)),
			textScript("second", ai.StopStop),
			textScript("third", ai.StopStop),
		},
		gate:   gate,
		gateAt: 2, // only the second request blocks, after the tool turn
	}
	agent := newTestAgent(fake, read)

	done := make(chan error, 1)
	go func() { done <- agent.Prompt(context.Background(), userMessage("go")) }()

	// The gate blocks the second provider request (after the tool turn).
	waitFor(t, 2*time.Second, func() bool { return fake.callCount() == 2 })
	agent.Steer(userMessage("mid-run steering"))
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if fake.callCount() != 3 {
		t.Errorf("provider calls = %d, want 3", fake.callCount())
	}
	messages := agent.State().Messages
	found := false
	for _, m := range messages {
		if m.Role == ai.RoleUser && ai.MessageText(m) == "mid-run steering" {
			found = true
		}
	}
	if !found {
		t.Errorf("steered message missing from transcript: %+v", messages)
	}
}

// TestAgentContinuePaths checks Continue's preconditions and the
// assistant-last queue drain.
//
// pi: agent.ts:384-411.
func TestAgentContinuePaths(t *testing.T) {
	t.Run("no messages", func(t *testing.T) {
		agent := newTestAgent(&fakeProvider{})
		if err := agent.Continue(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "no messages") {
			t.Errorf("error = %v, want a no-messages report", err)
		}
	})

	t.Run("assistant last with queued steering runs it as a prompt", func(t *testing.T) {
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			textScript("first", ai.StopStop),
			textScript("after steering", ai.StopStop),
		}}
		agent := newTestAgent(fake)
		if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		agent.Steer(userMessage("continue with this"))
		if err := agent.Continue(context.Background()); err != nil {
			t.Fatalf("Continue: %v", err)
		}
		if fake.callCount() != 2 {
			t.Errorf("provider calls = %d, want 2", fake.callCount())
		}
		messages := agent.State().Messages
		if ai.MessageText(messages[len(messages)-2]) != "continue with this" {
			t.Errorf("steering position = %q", ai.MessageText(messages[len(messages)-2]))
		}
	})

	t.Run("assistant last without queues fails", func(t *testing.T) {
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			textScript("first", ai.StopStop),
		}}
		agent := newTestAgent(fake)
		if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if err := agent.Continue(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "assistant") {
			t.Errorf("error = %v, want an assistant-role report", err)
		}
	})

	t.Run("user last continues the run", func(t *testing.T) {
		fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
			textScript("answer", ai.StopStop),
		}}
		agent := newTestAgent(fake)
		if err := agent.Prompt(context.Background(), userMessage("question")); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		// Seed a user message the way a retry would, then continue.
		agent.state.messages = append(agent.state.messages, userMessage("retry"))
		if err := agent.Continue(context.Background()); err != nil {
			t.Fatalf("Continue: %v", err)
		}
		if fake.callCount() != 2 {
			t.Errorf("provider calls = %d, want 2", fake.callCount())
		}
	})
}

// TestAgentAbortNoLeak is the plan's abort DoD: Abort() mid-tool, the event
// sequence closes with agent_end, the state settles, and no goroutine leaks.
func TestAgentAbortNoLeak(t *testing.T) {
	var started int32
	slow := behaviorTool("slow", toolBehavior{
		delay:   30 * time.Second, // only cancellation ends this
		started: &started,
	})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "slow", nil)),
		textScript("never requested", ai.StopStop),
	}}
	agent := newTestAgent(fake, slow)

	var mu sync.Mutex
	var names []string
	agent.Subscribe(func(ctx context.Context, ev AgentEvent) error {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, EventName(ev))
		return nil
	})

	before := goroutineCount()
	done := make(chan error, 1)
	go func() { done <- agent.Prompt(context.Background(), userMessage("go")) }()

	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&started) == 1 })
	agent.Abort()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt after abort: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt did not return after Abort")
	}

	mu.Lock()
	got := append([]string(nil), names...)
	mu.Unlock()
	if got[len(got)-1] != "agent_end" {
		t.Errorf("last event = %q, want agent_end (sequence: %v)", got[len(got)-1], got)
	}

	state := agent.State()
	if state.IsStreaming {
		t.Error("agent must be idle after abort")
	}
	if !strings.Contains(state.ErrorMessage, "aborted") {
		t.Errorf("error message = %q, want an abort report", state.ErrorMessage)
	}
	if len(state.PendingToolCalls) != 0 {
		t.Errorf("pending tool calls = %v, want empty", state.PendingToolCalls)
	}
	if n := waitForGoroutines(before, 5*time.Second); n > before {
		t.Errorf("goroutines after abort = %d, want <= %d", n, before)
	}
}

// TestAgentAbortBeforeProvider checks an abort that lands while the provider
// request is in flight ends the run cleanly.
func TestAgentAbortBeforeProvider(t *testing.T) {
	gate := make(chan struct{})
	fake := &fakeProvider{
		scripts: [][]ai.AssistantEvent{textScript("never", ai.StopStop)},
		gate:    gate,
	}
	agent := newTestAgent(fake)

	before := goroutineCount()
	done := make(chan error, 1)
	go func() { done <- agent.Prompt(context.Background(), userMessage("go")) }()

	waitFor(t, 2*time.Second, func() bool { return fake.callCount() == 1 })
	agent.Abort()
	close(gate) // the fake observes the canceled context and fails the stream

	if err := <-done; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if n := waitForGoroutines(before, 5*time.Second); n > before {
		t.Errorf("goroutines after run = %d, want <= %d", n, before)
	}
	state := agent.State()
	if state.IsStreaming {
		t.Error("agent must be idle")
	}
}

// TestAgentResetKeepsSystemBaseline checks reset retains the replayed
// prompt/tool baseline and clears the queues.
//
// pi: agent.ts:355-368.
func TestAgentResetKeepsSystemBaseline(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("answer", ai.StopStop),
	}}
	agent := newTestAgent(fake)
	if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	agent.Steer(userMessage("queued"))

	if err := agent.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	state := agent.State()
	if len(state.Messages) != 1 || state.Messages[0].Role != ai.RoleSystem {
		t.Fatalf("transcript after reset = %+v, want only the system baseline", state.Messages)
	}
	if state.SystemPrompt != "test prompt" {
		t.Errorf("system prompt = %q, want the baseline", state.SystemPrompt)
	}
	if agent.HasQueuedMessages() {
		t.Error("queues must be cleared")
	}
}

// TestAgentStateSnapshotCopies checks State() hands out copies.
func TestAgentStateSnapshotCopies(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("answer", ai.StopStop),
	}}
	agent := newTestAgent(fake)
	if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	first := agent.State()
	// Mutating the snapshot must not touch the agent.
	first.Messages[0] = ai.NewTextMessage(ai.RoleUser, "tampered")
	first.Messages = append(first.Messages, ai.NewTextMessage(ai.RoleUser, "extra"))
	second := agent.State()
	if len(second.Messages) != len(first.Messages)-1 {
		t.Errorf("transcript length changed through the snapshot: %d", len(second.Messages))
	}
	if ai.MessageText(second.Messages[0]) != "test prompt" {
		t.Errorf("leading message tampered: %q", ai.MessageText(second.Messages[0]))
	}
}

// TestAgentSubscribeUnsubscribe checks listener registration.
func TestAgentSubscribeUnsubscribe(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("answer", ai.StopStop),
	}}
	agent := newTestAgent(fake)
	var calls int
	unsubscribe := agent.Subscribe(func(ctx context.Context, ev AgentEvent) error {
		calls++
		return nil
	})
	unsubscribe()
	if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if calls != 0 {
		t.Errorf("listener calls = %d, want 0 after unsubscribe", calls)
	}
}

// TestAgentListenerErrorFailsRun checks a throwing subscriber surfaces as a
// failed run with the synthetic failure sequence, not a lost agent_end.
//
// pi: agent.ts:523-530 (catch → handleRunFailure).
func TestAgentListenerErrorFailsRun(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("answer", ai.StopStop),
	}}
	agent := newTestAgent(fake)

	var mu sync.Mutex
	var names []string
	agent.Subscribe(func(ctx context.Context, ev AgentEvent) error {
		mu.Lock()
		names = append(names, EventName(ev))
		mu.Unlock()
		if e, ok := ev.(EvMessageEnd); ok && e.Message.Role == ai.RoleUser {
			return errors.New("listener exploded")
		}
		return nil
	})

	// The failure is reported through events and state, not the return.
	if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
		t.Fatalf("Prompt must swallow run failures: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), names...)
	mu.Unlock()
	if got[len(got)-1] != "agent_end" {
		t.Errorf("last event = %q, want agent_end (sequence: %v)", got[len(got)-1], got)
	}
	state := agent.State()
	if !strings.Contains(state.ErrorMessage, "listener exploded") {
		t.Errorf("error message = %q, want the listener failure", state.ErrorMessage)
	}
	if state.IsStreaming {
		t.Error("agent must be idle after the failure")
	}
}

// TestAgentWaitForIdle checks idle waiting, including context cancellation.
func TestAgentWaitForIdle(t *testing.T) {
	gate := make(chan struct{})
	fake := &fakeProvider{
		scripts: [][]ai.AssistantEvent{textScript("ok", ai.StopStop)},
		gate:    gate,
	}
	agent := newTestAgent(fake)

	// No run: returns immediately.
	if err := agent.WaitForIdle(context.Background()); err != nil {
		t.Fatalf("WaitForIdle with no run: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- agent.Prompt(context.Background(), userMessage("go")) }()
	waitFor(t, 2*time.Second, func() bool { return fake.callCount() == 1 })

	// A canceled context gives up waiting.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := agent.WaitForIdle(ctx); err == nil {
		t.Error("WaitForIdle must fail when its context expires")
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := agent.WaitForIdle(context.Background()); err != nil {
		t.Fatalf("WaitForIdle after the run: %v", err)
	}
}

// TestAgentQueueHelpers checks the queue inspection API.
func TestAgentQueueHelpers(t *testing.T) {
	agent := newTestAgent(&fakeProvider{})
	if agent.HasQueuedMessages() {
		t.Error("fresh agent must have no queued messages")
	}
	agent.Steer(userMessage("s"))
	agent.FollowUp(userMessage("f"))
	if !agent.HasQueuedMessages() {
		t.Error("queues must report messages")
	}
	peeked := agent.PeekQueuedMessages()
	if len(peeked) != 1 || ai.MessageText(peeked[0]) != "s" {
		t.Errorf("peek = %+v, want the steering message", peeked)
	}
	// Peek does not consume.
	if !agent.HasQueuedMessages() {
		t.Error("peek must not drain")
	}
	agent.ClearSteeringQueue()
	peeked = agent.PeekQueuedMessages()
	if len(peeked) != 1 || ai.MessageText(peeked[0]) != "f" {
		t.Errorf("peek after steering clear = %+v, want the follow-up", peeked)
	}
	agent.ClearAllQueues()
	if agent.HasQueuedMessages() {
		t.Error("ClearAllQueues must empty both queues")
	}
	if agent.SteeringMode() != QueueOneAtATime || agent.FollowUpMode() != QueueOneAtATime {
		t.Errorf("default modes = %s/%s", agent.SteeringMode(), agent.FollowUpMode())
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
