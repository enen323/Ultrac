package ai

import "testing"

func TestEstimateContextTokensCountsMessages(t *testing.T) {
	// 350 chars of text ≈ 100 tokens at 3.5 chars/token.
	text := string(make([]byte, 0)) + repeat("a", 350)
	msgs := []Message{NewTextMessage(RoleUser, text)}
	got := EstimateContextTokens(msgs)
	if got < 95 || got > 105 {
		t.Errorf("estimate = %d, want ~100", got)
	}
}

func TestEstimateContextTokensUsesLastAssistantUsage(t *testing.T) {
	long := repeat("x", 35000) // ~10000 estimated tokens
	msgs := []Message{
		NewTextMessage(RoleUser, long),
		{
			Role:    RoleAssistant,
			Content: []ContentBlock{TextBlock("ok")},
			Usage:   &Usage{Input: 10000, Output: 100, TotalTokens: 10100},
		},
		NewTextMessage(RoleUser, "short"),
	}
	got := EstimateContextTokens(msgs)
	// The assistant usage describes the prefix exactly; only "short"
	// (~2 tokens) is estimated on top.
	if got < 10100 || got > 10110 {
		t.Errorf("estimate = %d, want ~10102", got)
	}
}

func TestClampMaxTokensToContext(t *testing.T) {
	model := Model{ContextWindow: 100000, MaxTokens: 8192}
	// A tiny transcript leaves plenty of room: the cap is untouched.
	if got := ClampMaxTokensToContext(model, []Message{NewTextMessage(RoleUser, "hi")}, 4096); got != 4096 {
		t.Errorf("clamped = %d, want 4096", got)
	}
	// A huge transcript squeezes the answer room to the safety floor.
	huge := NewTextMessage(RoleUser, repeat("y", 400000)) // ~114k tokens
	got := ClampMaxTokensToContext(model, []Message{huge}, 4096)
	if got != 1 {
		t.Errorf("clamped = %d, want the 1-token floor", got)
	}
	// No recorded window means no clamping.
	open := Model{ContextWindow: 0, MaxTokens: 8192}
	if got := ClampMaxTokensToContext(open, []Message{huge}, 4096); got != 4096 {
		t.Errorf("clamped = %d, want 4096 (unknown window must not clamp)", got)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
