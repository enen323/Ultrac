package compact

import (
	"context"
	"math"
	"strings"
	"testing"

	"ultrac/internal/ai"
)

// capturingStreamFn records what the summarization call sent and replays a
// fixed event list. It is the test double for pi's streamFn parameter.
type capturingStreamFn struct {
	gotModel  ai.Model
	gotOpts   ai.StreamOptions
	gotSystem string
	gotPrompt string
	events    []ai.AssistantEvent
}

func (f *capturingStreamFn) fn(_ context.Context, model ai.Model, tc ai.TranscriptContext, opts ai.StreamOptions) ai.Stream {
	f.gotModel = model
	f.gotOpts = opts
	if len(tc.Messages) > 0 {
		f.gotSystem = ai.MessageText(tc.Messages[0])
	}
	if len(tc.Messages) > 1 {
		f.gotPrompt = ai.MessageText(tc.Messages[1])
	}
	return ai.StreamOf(f.events...)
}

// queueStreamFn hands out one event list per call, so a split-turn Run can
// be observed call by call.
type queueStreamFn struct {
	calls  []capturedCall
	events [][]ai.AssistantEvent
}

type capturedCall struct {
	opts   ai.StreamOptions
	prompt string
}

func (q *queueStreamFn) fn(_ context.Context, _ ai.Model, tc ai.TranscriptContext, opts ai.StreamOptions) ai.Stream {
	prompt := ""
	if len(tc.Messages) > 1 {
		prompt = ai.MessageText(tc.Messages[1])
	}
	q.calls = append(q.calls, capturedCall{opts: opts, prompt: prompt})
	i := len(q.calls) - 1
	if i >= len(q.events) {
		i = len(q.events) - 1
	}
	return ai.StreamOf(q.events[i]...)
}

func doneEvents(text string, usage ai.Usage) []ai.AssistantEvent {
	m := ai.NewTextMessage(ai.RoleAssistant, text)
	m.StopReason = ai.StopStop
	m.Usage = &usage
	return []ai.AssistantEvent{ai.EvDone{Reason: ai.StopStop, Message: m}}
}

func testModel() ai.Model {
	return ai.Model{ID: "test-model", API: ai.APIOpenAICompletions, Provider: ai.ProviderOpenAI, Reasoning: true, ContextWindow: 200000}
}

func testOptions(fn ai.StreamFn) SummaryOptions {
	return SummaryOptions{
		Model:      testModel(),
		APIKey:     "test-key",
		Reasoning:  ai.ThinkMedium,
		StreamFn:   fn,
		MaxRetries: 0,
	}
}

func TestSummarizeSuccess(t *testing.T) {
	fake := &capturingStreamFn{events: doneEvents("## Goal\nship it", ai.Usage{Input: 100, Output: 20, TotalTokens: 120})}
	got, err := Summarize(context.Background(),
		[]ai.Message{ai.NewTextMessage(ai.RoleUser, "hello")},
		"", "", 16384, testOptions(fake.fn))
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "## Goal\nship it" {
		t.Errorf("text = %q", got.Text)
	}
	if got.Usage.TotalTokens != 120 {
		t.Errorf("usage = %+v", got.Usage)
	}

	// The request shape: summarization system prompt, the serialized
	// conversation inside <conversation> tags, and the main prompt.
	if fake.gotSystem != SummarizationSystemPrompt {
		t.Errorf("system prompt drifted from the verbatim constant")
	}
	if !strings.HasPrefix(fake.gotPrompt, "<conversation>\n[User]: hello\n</conversation>\n\n") {
		t.Errorf("prompt = %q", fake.gotPrompt)
	}
	if !strings.Contains(fake.gotPrompt, "The messages above are a conversation to summarize") {
		t.Errorf("prompt is not the SUMMARIZATION_PROMPT: %q", fake.gotPrompt)
	}
	// No cache write for a one-off summary (pi: cacheRetention: "none").
	if fake.gotOpts.CacheRetention != ai.CacheRetentionNone {
		t.Errorf("cacheRetention = %q, want none", fake.gotOpts.CacheRetention)
	}
	// maxTokens = floor(0.8 x reserve).
	if want := int(math.Floor(0.8 * 16384)); fake.gotOpts.MaxTokens != want {
		t.Errorf("maxTokens = %d, want %d", fake.gotOpts.MaxTokens, want)
	}
	// A routing session id is generated when the caller supplies none.
	if fake.gotOpts.SessionID == "" {
		t.Error("session id not generated")
	}
	if fake.gotOpts.APIKey != "test-key" {
		t.Errorf("apiKey = %q", fake.gotOpts.APIKey)
	}
}

func TestSummarizeOptionsPassThrough(t *testing.T) {
	opts := testOptions(nil)
	opts.Reasoning = ai.ThinkHigh
	opts.SessionID = "fixed-session"
	fake := &capturingStreamFn{events: doneEvents("s", ai.Usage{})}
	opts.StreamFn = fake.fn
	if _, err := Summarize(context.Background(), nil, "", "", 1000, opts); err != nil {
		t.Fatal(err)
	}
	if fake.gotOpts.SessionID != "fixed-session" {
		t.Errorf("session id = %q, want the caller's", fake.gotOpts.SessionID)
	}
	if fake.gotOpts.Reasoning != ai.ThinkHigh {
		t.Errorf("reasoning = %q, want high", fake.gotOpts.Reasoning)
	}
}

func TestSummarizeReasoningOnlyForReasoningModels(t *testing.T) {
	// pi: createSummarizationOptions sets reasoning only when
	// model.reasoning && level && level !== "off".
	opts := testOptions(nil)
	opts.Model.Reasoning = false
	opts.Reasoning = ai.ThinkHigh
	fake := &capturingStreamFn{events: doneEvents("s", ai.Usage{})}
	opts.StreamFn = fake.fn
	if _, err := Summarize(context.Background(), nil, "", "", 1000, opts); err != nil {
		t.Fatal(err)
	}
	if fake.gotOpts.Reasoning != "" {
		t.Errorf("reasoning = %q, want unset for a non-reasoning model", fake.gotOpts.Reasoning)
	}
}

func TestSummarizeMaxTokensClamp(t *testing.T) {
	opts := testOptions(nil)
	opts.Model.MaxTokens = 100 // below floor(0.8 x 16384)
	fake := &capturingStreamFn{events: doneEvents("s", ai.Usage{})}
	opts.StreamFn = fake.fn
	if _, err := Summarize(context.Background(), nil, "", "", 16384, opts); err != nil {
		t.Fatal(err)
	}
	if fake.gotOpts.MaxTokens != 100 {
		t.Errorf("maxTokens = %d, want the model cap 100", fake.gotOpts.MaxTokens)
	}

	// A model with no recorded cap does not clamp.
	opts.Model.MaxTokens = 0
	fake2 := &capturingStreamFn{events: doneEvents("s", ai.Usage{})}
	opts.StreamFn = fake2.fn
	if _, err := Summarize(context.Background(), nil, "", "", 16384, opts); err != nil {
		t.Fatal(err)
	}
	if want := int(math.Floor(0.8 * 16384)); fake2.gotOpts.MaxTokens != want {
		t.Errorf("maxTokens = %d, want %d (no cap)", fake2.gotOpts.MaxTokens, want)
	}
}

func TestSummarizeUpdatePromptWithPreviousSummary(t *testing.T) {
	fake := &capturingStreamFn{events: doneEvents("merged", ai.Usage{})}
	if _, err := Summarize(context.Background(),
		[]ai.Message{ai.NewTextMessage(ai.RoleUser, "new work")},
		"## Goal\nold", "focus on tests", 16384, testOptions(fake.fn)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.gotPrompt, "<previous-summary>\n## Goal\nold\n</previous-summary>") {
		t.Errorf("previous summary not framed: %q", fake.gotPrompt)
	}
	if !strings.Contains(fake.gotPrompt, "The messages above are NEW conversation messages") {
		t.Errorf("not the UPDATE prompt: %q", fake.gotPrompt)
	}
	if !strings.Contains(fake.gotPrompt, "Additional focus: focus on tests") {
		t.Errorf("custom instructions not appended: %q", fake.gotPrompt)
	}
}

func TestSummarizeTurnPrefix(t *testing.T) {
	fake := &capturingStreamFn{events: doneEvents("prefix", ai.Usage{})}
	got, err := SummarizeTurnPrefix(context.Background(),
		[]ai.Message{ai.NewTextMessage(ai.RoleUser, "the request")}, 16384, testOptions(fake.fn))
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "prefix" {
		t.Errorf("text = %q", got.Text)
	}
	if !strings.HasPrefix(fake.gotPrompt, "# Conversation\n[User]: the request\n\n# Instructions\n") {
		t.Errorf("prompt = %q", fake.gotPrompt)
	}
	if !strings.Contains(fake.gotPrompt, "The messages above are earlier context from an ongoing conversation") {
		t.Errorf("not the TURN_PREFIX prompt: %q", fake.gotPrompt)
	}
	// Half the reserve, not 0.8.
	if want := int(math.Floor(0.5 * 16384)); fake.gotOpts.MaxTokens != want {
		t.Errorf("maxTokens = %d, want %d", fake.gotOpts.MaxTokens, want)
	}
}

func TestSummarizeFailures(t *testing.T) {
	errorMessage := ai.NewTextMessage(ai.RoleAssistant, "")
	errorMessage.StopReason = ai.StopError
	errorMessage.ErrorMessage = "provider exploded"

	lengthMessage := ai.NewTextMessage(ai.RoleAssistant, "partial")
	lengthMessage.StopReason = ai.StopLength

	abortedMessage := ai.NewTextMessage(ai.RoleAssistant, "")
	abortedMessage.StopReason = ai.StopAborted

	toolCallMessage := ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.ContentBlock{ai.ToolCallBlock("c1", "read", map[string]any{"path": "x"})},
		StopReason: ai.StopStop,
	}

	cases := []struct {
		name string
		msg  ai.Message
		want string
	}{
		{"error", errorMessage, "Summarization failed: provider exploded"},
		{"length", lengthMessage, "Summarization failed: generation hit the token cap and the summary is incomplete"},
		{"aborted", abortedMessage, "Summarization aborted"},
		{"tool call", toolCallMessage, "Summarization attempted to call a tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason := ai.StopError
			if c.msg.StopReason == ai.StopLength || c.msg.StopReason == ai.StopStop {
				reason = ai.StopStop
			}
			events := []ai.AssistantEvent{ai.EvDone{Reason: reason, Message: c.msg}}
			if c.msg.StopReason == ai.StopError || c.msg.StopReason == ai.StopAborted {
				events = []ai.AssistantEvent{ai.EvError{Reason: c.msg.StopReason, Error: c.msg}}
			}
			opts := testOptions(nil)
			opts.StreamFn = (&capturingStreamFn{events: events}).fn
			_, err := Summarize(context.Background(), []ai.Message{ai.NewTextMessage(ai.RoleUser, "hi")}, "", "", 16384, opts)
			if err == nil {
				t.Fatalf("no error for %s", c.name)
			}
			if err.Error() != c.want {
				t.Errorf("error = %q, want %q", err.Error(), c.want)
			}
		})
	}
}

func TestSummarizeRegistryFailureIsInStream(t *testing.T) {
	// A nil StreamFn falls back to the registry; an unregistered API must
	// surface as an in-stream error (the StreamFn contract), never a nil
	// stream or a panic.
	opts := testOptions(nil) // StreamFn nil → defaultStreamFn
	opts.Model = ai.Model{ID: "x", API: ai.Api("no-such-api")}
	_, err := Summarize(context.Background(), []ai.Message{ai.NewTextMessage(ai.RoleUser, "hi")}, "", "", 16384, opts)
	if err == nil || !strings.Contains(err.Error(), "no provider registered") {
		t.Fatalf("err = %v, want a registry failure", err)
	}
}

func TestCombineUsage(t *testing.T) {
	first := ai.Usage{Input: 10, Output: 2, CacheRead: 1, CacheWrite: 3, TotalTokens: 16, Cost: ai.Cost{Total: 0.5}}
	second := ai.Usage{Input: 100, Output: 20, TotalTokens: 120, Cost: ai.Cost{Total: 1.5}}
	got := CombineUsage(first, second)
	if got.Input != 110 || got.Output != 22 || got.CacheRead != 1 || got.CacheWrite != 3 || got.TotalTokens != 136 {
		t.Errorf("sums wrong: %+v", got)
	}
	if got.Cost.Total != 2.0 {
		t.Errorf("cost = %v, want 2", got.Cost.Total)
	}
	if got.CacheWrite1h != nil || got.Reasoning != nil {
		t.Errorf("optional splits should stay absent when neither side reports them: %+v", got)
	}
	r := 7
	first.Reasoning = &r
	second2 := ai.Usage{Reasoning: &r}
	if got := CombineUsage(first, second2); got.Reasoning == nil || *got.Reasoning != 14 {
		t.Errorf("reasoning split = %v, want 14", got.Reasoning)
	}
}
