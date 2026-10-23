package agent

// replace_messages_test.go — the compaction state-swap tests
// (docs/compaction-branch-plan.md D2 / R1).
//
// The risk these pin down: the Agent's state.messages is the durable
// transcript the loop snapshots per run, while a compaction entry lands in
// the session. If only the session were replaced, the next Prompt would
// replay the uncompressed history. ReplaceMessages closes that split, and
// the steering queues must survive it untouched — a queued message is
// delivered exactly once, never duplicated out of the old transcript.

import (
	"context"
	"testing"

	"ultrac/internal/ai"
)

// TestReplaceMessagesSwapsTranscript is the basic contract: the replacement
// is the new transcript, wholesale, with no baseline preservation beyond
// what the caller put in it.
func TestReplaceMessagesSwapsTranscript(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{textScript("first", ai.StopStop)}}
	agent := newTestAgent(fake)
	if err := agent.Prompt(context.Background(), userMessage("old history")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(agent.State().Messages) != 3 {
		t.Fatalf("precondition: %d messages, want 3 (system + user + assistant)", len(agent.State().Messages))
	}

	replacement := []ai.Message{
		ai.NewTextMessage(ai.RoleSystem, "compacted prompt"),
		ai.NewTextMessage(ai.RoleUser, "<checkpoint>"),
	}
	agent.ReplaceMessages(replacement)

	got := agent.State().Messages
	if len(got) != 2 {
		t.Fatalf("transcript = %d messages, want 2: %+v", len(got), got)
	}
	if got[0].Role != ai.RoleSystem || ai.MessageText(got[0]) != "compacted prompt" {
		t.Errorf("message 0 = %+v, want the replacement's system message", got[0])
	}
	if ai.MessageText(got[1]) != "<checkpoint>" {
		t.Errorf("message 1 = %q, want <checkpoint>", ai.MessageText(got[1]))
	}
	// The system prompt is replayed from the transcript, so it must follow
	// the swap — a stale one would send the old prompt on the next request.
	if agent.State().SystemPrompt != "compacted prompt" {
		t.Errorf("system prompt = %q, want the replacement's", agent.State().SystemPrompt)
	}

	// Mutating the caller's slice afterwards must not touch the agent's
	// copy (the snapshot-copy rule every other state mutation follows).
	replacement[1] = ai.NewTextMessage(ai.RoleUser, "tampered")
	if ai.MessageText(agent.State().Messages[1]) != "<checkpoint>" {
		t.Error("ReplaceMessages did not copy: the caller's slice is aliased")
	}
}

// TestReplaceMessagesMidRunCompressesHistory is the D2 pattern end to end: a
// PrepareRequest hook swaps the agent state AND returns the replacement
// context on the same move, the way cmd/ultrac's compaction executor does.
// The run keeps appending to the swapped transcript, and the NEXT prompt
// never replays the summarized-away history.
func TestReplaceMessagesMidRunCompressesHistory(t *testing.T) {
	read := behaviorTool("read", toolBehavior{text: "contents"})
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		toolCallScript(ai.StopToolUse, callBlock("tu_1", "read", nil)),
		textScript("second", ai.StopStop),
		textScript("third", ai.StopStop),
	}}
	var agent *Agent
	compacted := false
	agent = NewAgent(AgentOptions{
		InitialState: &AgentInitialState{
			SystemPrompt: "test prompt",
			Model:        modelPtr(fakeModel()),
			Tools:        []Tool{read},
		},
		StreamFn: fake.streamFn,
		PrepareRequest: func(_ context.Context, req PrepareRequestContext) AgentRequestUpdate {
			// The first request goes out unchanged; the second is where a
			// real run would have crossed the threshold, and the swap
			// happens exactly once — after it the context fits again, the
			// same reason a real compaction does not fire twice in a row.
			if fake.callCount() == 0 || compacted {
				return AgentRequestUpdate{}
			}
			compacted = true
			replacement := []ai.Message{
				req.Context.Messages[0], // the system baseline
				ai.NewTextMessage(ai.RoleUser, "<checkpoint>"),
			}
			agent.ReplaceMessages(replacement)
			return AgentRequestUpdate{Context: &AgentContext{Messages: replacement, Tools: req.Context.Tools}}
		},
	})

	if err := agent.Prompt(context.Background(), userMessage("go")); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// The second request must carry the compressed context, not the
	// original history: system + checkpoint only.
	second := fake.transcript(1)
	if len(second) != 2 {
		t.Fatalf("second request carried %d messages, want 2 (system + checkpoint): %+v", len(second), second)
	}
	if second[0].Role != ai.RoleSystem || ai.MessageText(second[1]) != "<checkpoint>" {
		t.Errorf("second request = %+v, want the compacted context", second)
	}
	for _, m := range second {
		if m.Role == ai.RoleToolResult {
			t.Error("the second request still carries the summarized-away tool result")
		}
	}

	// The run kept appending to the swapped transcript: checkpoint,
	// assistant("second") — the pre-compaction messages are gone from the
	// durable state, not just from the request.
	state := agent.State().Messages
	if len(state) != 3 {
		t.Fatalf("transcript after run = %d messages, want 3 (system + checkpoint + assistant): %+v", len(state), state)
	}
	if ai.MessageText(state[2]) != "second" {
		t.Errorf("final message = %q, want second", ai.MessageText(state[2]))
	}

	// The next prompt snapshots the swapped state: no old history rides
	// along (the R1 regression this whole design exists to prevent).
	if err := agent.Prompt(context.Background(), userMessage("next")); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	third := fake.transcript(2)
	if len(third) != 4 {
		t.Fatalf("third request carried %d messages, want 4 (system + checkpoint + assistant + user): %+v", len(third), third)
	}
	if ai.MessageText(third[3]) != "next" {
		t.Errorf("third request tail = %q, want next", ai.MessageText(third[3]))
	}
	for _, m := range third {
		if m.Role == ai.RoleUser && ai.MessageText(m) == "go" {
			t.Error("the next prompt replayed the summarized-away user message")
		}
	}
}

// TestReplaceMessagesKeepsQueuesUnduplicated pins the steering-queue half of
// D2: the swap replaces state.messages only. A message queued before the
// swap is delivered exactly once — by the loop's own drain, never replayed
// out of the old transcript.
func TestReplaceMessagesKeepsQueuesUnduplicated(t *testing.T) {
	fake := &fakeProvider{scripts: [][]ai.AssistantEvent{
		textScript("first", ai.StopStop),
		textScript("second", ai.StopStop),
	}}
	agent := newTestAgent(fake)
	if err := agent.Prompt(context.Background(), userMessage("old history")); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}

	agent.Steer(userMessage("queued steer"))
	agent.ReplaceMessages([]ai.Message{
		ai.NewTextMessage(ai.RoleSystem, "compacted prompt"),
		ai.NewTextMessage(ai.RoleUser, "<checkpoint>"),
	})

	if err := agent.Prompt(context.Background(), userMessage("next")); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}

	second := fake.transcript(1)
	var steers, olds int
	for _, m := range second {
		if m.Role == ai.RoleUser && ai.MessageText(m) == "queued steer" {
			steers++
		}
		if m.Role == ai.RoleUser && ai.MessageText(m) == "old history" {
			olds++
		}
	}
	if steers != 1 {
		t.Errorf("queued steer delivered %d times, want exactly 1: %+v", steers, second)
	}
	if olds != 0 {
		t.Errorf("the pre-swap history rode along %d times, want 0: %+v", olds, second)
	}
	// system, checkpoint, steer, next.
	if len(second) != 4 {
		t.Errorf("second request = %d messages, want 4: %+v", len(second), second)
	}
	// And the queue is empty afterwards — the drain consumed it, the swap
	// did not.
	if agent.HasQueuedMessages() {
		t.Error("steering queue still holds the delivered message")
	}
}
