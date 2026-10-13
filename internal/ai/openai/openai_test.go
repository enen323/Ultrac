package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ultrac/internal/ai"
	"ultrac/internal/logging"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sse(data string) string {
	return "data: " + data + "\n\n"
}

// textStream is a complete, valid single-text-block response in the Chat
// Completions dialect, including the usage-only final chunk.
func textStream(text string) string {
	var b strings.Builder
	b.WriteString(sse(`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`))
	for _, piece := range splitEvery(text, 4) {
		enc, _ := json.Marshal(piece)
		b.WriteString(sse(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":` + string(enc) + `}}]}`))
	}
	b.WriteString(sse(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	b.WriteString(sse(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func splitEvery(s string, n int) []string {
	var out []string
	r := []rune(s)
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

type collected struct {
	events []ai.AssistantEvent
	names  []string
	final  ai.Message
	err    error
}

func drain(t *testing.T, s ai.Stream) collected {
	t.Helper()
	var out collected
	for ev, err := range s.Events() {
		if err != nil {
			t.Fatalf("event iterator yielded an error; failures belong inside the stream: %v", err)
		}
		out.events = append(out.events, ev)
		out.names = append(out.names, ai.Name(ev))
	}
	msg, err := s.Result()
	out.final, out.err = msg, err
	return out
}

func testModel() ai.Model {
	return ai.Model{
		ID:            "test-model",
		Name:          "Test Model",
		API:           ai.APIOpenAICompletions,
		Provider:      ai.ProviderOpenAI,
		ContextWindow: 128000,
		MaxTokens:     4096,
		Cost: ai.ModelCost{
			Input: 1.0, Output: 2.0, CacheRead: 0.1, CacheWrite: 1.0,
		},
	}
}

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	model := testModel()
	model.BaseURL = srv.URL
	c, err := New(Config{
		Model:      model,
		APIKey:     "test-key",
		MaxRetries: 3,
		HTTPClient: srv.Client(),
		Logger:     logging.New(logging.Options{Disabled: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.retryBase = time.Millisecond
	c.retryMax = 2 * time.Millisecond
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	c.randFloat = func() float64 { return 0 }
	return c, srv
}

func simpleTranscript(prompt string) ai.TranscriptContext {
	return ai.NormalizeContext(ai.Context{
		SystemPrompt: "You are a test.",
		Messages:     []ai.Message{ai.NewTextMessage(ai.RoleUser, prompt)},
	})
}

func toolTranscript() ai.TranscriptContext {
	return ai.NormalizeContext(ai.Context{
		SystemPrompt: "You are a test.",
		Messages: []ai.Message{
			ai.NewTextMessage(ai.RoleUser, "read go.mod"),
			{
				Role: ai.RoleAssistant,
				Content: []ai.ContentBlock{
					ai.ToolCallBlock("call_1", "read", map[string]any{"path": "go.mod"}),
				},
			},
			ai.NewToolResultMessage("call_1", "read", "module ultrac", false),
		},
		Tools: []ai.Tool{{
			Name:        "read",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	})
}

// ---------------------------------------------------------------------------
// DoD: text_start → text_delta* → text_end → done
// ---------------------------------------------------------------------------

func TestStreamEmitsTwelveEventTextSequence(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textStream("hello world"))
	})

	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))

	want := []string{"start", "text_start", "text_delta", "text_delta", "text_delta", "text_end", "done"}
	if strings.Join(got.names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", got.names, want)
	}

	end := got.events[len(got.events)-2].(ai.EvTextEnd)
	if end.Content != "hello world" {
		t.Errorf("text_end content = %q", end.Content)
	}
	done := got.events[len(got.events)-1].(ai.EvDone)
	if done.Reason != ai.StopStop {
		t.Errorf("done reason = %q, want stop", done.Reason)
	}
	if ai.MessageText(done.Message) != "hello world" {
		t.Errorf("final text = %q", ai.MessageText(done.Message))
	}
	// The usage-only final chunk is the only accounting this dialect gives.
	if done.Message.Usage == nil || done.Message.Usage.Input != 11 || done.Message.Usage.Output != 7 {
		t.Errorf("usage = %+v", done.Message.Usage)
	}
	if done.Message.Usage.TotalTokens != 18 {
		t.Errorf("totalTokens = %d, want 18", done.Message.Usage.TotalTokens)
	}
	if done.Message.ResponseID != "chatcmpl-1" {
		t.Errorf("responseId = %q", done.Message.ResponseID)
	}
}

// ---------------------------------------------------------------------------
// DoD: tool calls accumulate by stream index
// ---------------------------------------------------------------------------

func TestToolCallsAccumulateByStreamIndex(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":"sure"}}]}`))
	// The id and name arrive with the first fragment; arguments stream after.
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":""}}]}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"go.mod\"}"}}]}}]}`))
	// A second call, interleaved by index only.
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"ls","arguments":"{}"}}]}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	b.WriteString(sse(`{"id":"c","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`))
	b.WriteString("data: [DONE]\n\n")

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))

	want := []string{
		"start",
		"text_start", "text_delta",
		"toolcall_start", "toolcall_delta", "toolcall_delta", "toolcall_delta",
		"toolcall_start", "toolcall_delta",
		"text_end", "toolcall_end", "toolcall_end",
		"done",
	}
	if strings.Join(got.names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", got.names, want)
	}

	firstStart := got.events[3].(ai.EvToolCallStart)
	if firstStart.ContentIndex != 1 {
		t.Errorf("first toolcall_start contentIndex = %d, want 1", firstStart.ContentIndex)
	}
	secondStart := got.events[7].(ai.EvToolCallStart)
	if secondStart.ContentIndex != 2 {
		t.Errorf("second toolcall_start contentIndex = %d, want 2", secondStart.ContentIndex)
	}

	first := got.events[10].(ai.EvToolCallEnd)
	if first.ToolCall.ID != "call_a" || first.ToolCall.Name != "read" {
		t.Errorf("first call = %+v", first.ToolCall)
	}
	if first.ToolCall.Arguments["path"] != "go.mod" {
		t.Errorf("first arguments = %+v", first.ToolCall.Arguments)
	}
	second := got.events[11].(ai.EvToolCallEnd)
	if second.ToolCall.ID != "call_b" || second.ToolCall.Name != "ls" {
		t.Errorf("second call = %+v", second.ToolCall)
	}
	done := got.events[len(got.events)-1].(ai.EvDone)
	if done.Reason != ai.StopToolUse {
		t.Errorf("reason = %q, want toolUse", done.Reason)
	}
	if len(done.Message.Content) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(done.Message.Content))
	}
}

// TestToolCallWithoutStreamIndex: some servers omit the index and send the
// id once; the block must still be matched across deltas.
func TestToolCallWithoutStreamIndex(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_x","function":{"name":"read","arguments":"{\"a\":"}}]}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_x","function":{"arguments":"1}"}}]}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	b.WriteString("data: [DONE]\n\n")

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	end := got.events[len(got.events)-2].(ai.EvToolCallEnd)
	if end.ToolCall.Arguments["a"] != float64(1) {
		t.Errorf("arguments = %+v", end.ToolCall.Arguments)
	}
	if n := countEvents(got, "toolcall_start"); n != 1 {
		t.Errorf("toolcall_start count = %d, want 1 (matched by id, not duplicated)", n)
	}
}

func countEvents(got collected, name string) int {
	n := 0
	for _, ev := range got.events {
		if ai.Name(ev) == name {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// DoD: reasoning streams as thinking, finish_reason length is length
// ---------------------------------------------------------------------------

func TestReasoningContentStreamsAsThinking(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"think "}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"more"}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"content":"answer"}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	b.WriteString("data: [DONE]\n\n")

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))

	want := []string{"start", "thinking_start", "thinking_delta", "thinking_delta", "text_start", "text_delta", "thinking_end", "text_end", "done"}
	if strings.Join(got.names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", got.names, want)
	}
	done := got.events[len(got.events)-1].(ai.EvDone)
	if done.Message.Content[0].Type != ai.BlockThinking {
		t.Fatalf("block 0 = %+v", done.Message.Content[0])
	}
	if done.Message.Content[0].Thinking != "think more" {
		t.Errorf("thinking = %q", done.Message.Content[0].Thinking)
	}
	// The field name is kept in the signature slot so a replay can send the
	// reasoning back on the same field.
	if done.Message.Content[0].ThinkingSignature != "reasoning_content" {
		t.Errorf("signature = %q, want the field name", done.Message.Content[0].ThinkingSignature)
	}
}

func TestFinishReasonLengthIsLength(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"content":"cut"}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`))
	b.WriteString("data: [DONE]\n\n")

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	done := got.events[len(got.events)-1].(ai.EvDone)
	if done.Reason != ai.StopLength {
		t.Errorf("reason = %q, want length", done.Reason)
	}
}

func TestContentFilterIsError(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"content":"cut"}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`))
	b.WriteString("data: [DONE]\n\n")

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	last := got.events[len(got.events)-1]
	if _, ok := last.(ai.EvError); !ok {
		t.Fatalf("last event = %s, want error", ai.Name(last))
	}
}

func TestTruncatedStreamIsReported(t *testing.T) {
	// No finish_reason before [DONE]: the usage chunk is not a terminator,
	// so this is a cut-off response and must not look complete.
	body := sse(`{"id":"c","choices":[{"index":0,"delta":{"content":"half"}}]}`) + "data: [DONE]\n\n"
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	last := got.events[len(got.events)-1]
	errEv, ok := last.(ai.EvError)
	if !ok {
		t.Fatalf("last event = %s, want error", ai.Name(last))
	}
	if !strings.Contains(errEv.Error.ErrorMessage, "finish_reason") {
		t.Errorf("error = %q", errEv.Error.ErrorMessage)
	}
}

func TestInvalidToolArgumentsAreReported(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`))
	b.WriteString(sse(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	b.WriteString("data: [DONE]\n\n")

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	for _, ev := range got.events {
		if _, ok := ev.(ai.EvToolCallEnd); ok {
			t.Error("invalid JSON arguments were emitted as a complete call")
		}
	}
	last, ok := got.events[len(got.events)-1].(ai.EvError)
	if !ok {
		t.Fatalf("last event = %s, want error", got.names[len(got.names)-1])
	}
	if !strings.Contains(last.Error.ErrorMessage, "invalid JSON") {
		t.Errorf("error = %q", last.Error.ErrorMessage)
	}
}

// ---------------------------------------------------------------------------
// cancellation and retries
// ---------------------------------------------------------------------------

func TestCancellationInterruptsInFlightStream(t *testing.T) {
	release := make(chan struct{})
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	s := client.stream(ctx, testModel(), simpleTranscript("hi"), Options{})

	done := make(chan ai.AssistantEvent, 8)
	go func() {
		for ev, err := range s.Events() {
			if err != nil {
				return
			}
			done <- ev
		}
		close(done)
	}()

	cancel()
	close(release)

	var last ai.AssistantEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-done:
			if !ok {
				errEv, ok := last.(ai.EvError)
				if !ok {
					t.Fatalf("last event = %v, want error", last)
				}
				if errEv.Reason != ai.StopAborted {
					t.Errorf("reason = %q, want aborted", errEv.Reason)
				}
				return
			}
			last = ev
		case <-timeout:
			t.Fatal("cancellation did not terminate the stream")
		}
	}
}

func TestRetriesOnRateLimitThenSucceeds(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
			return
		}
		fmt.Fprint(w, textStream("recovered"))
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 3 {
		t.Fatalf("requests = %d, want 3", hits.Load())
	}
	done := got.events[len(got.events)-1].(ai.EvDone)
	if ai.MessageText(done.Message) != "recovered" {
		t.Errorf("text = %q", ai.MessageText(done.Message))
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad request","type":"invalid_request_error"}}`)
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1", hits.Load())
	}
	last := got.events[len(got.events)-1].(ai.EvError)
	if !strings.Contains(last.Error.ErrorMessage, "bad request") {
		t.Errorf("error = %q", last.Error.ErrorMessage)
	}
}

func TestMidStreamFailureIsNotRetried(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, sse(`{"id":"c","choices":[{"index":0,"delta":{"content":"partial"}}]}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1", hits.Load())
	}
	if _, ok := got.events[len(got.events)-1].(ai.EvError); !ok {
		t.Error("a cut stream must end in error")
	}
}

// ---------------------------------------------------------------------------
// request encoding
// ---------------------------------------------------------------------------

func TestRequestBodyMatchesTheDialect(t *testing.T) {
	var body []byte
	client, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readBody(r)
		fmt.Fprint(w, textStream("ok"))
	})
	model := testModel()
	model.BaseURL = srv.URL

	drain(t, client.stream(context.Background(), model, toolTranscript(), Options{}))

	var wire struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		MaxCompletionTokens *int  `json:"max_completion_tokens"`
		Store               *bool `json:"store"`
		Messages            []struct {
			Role             string `json:"role"`
			Content          any    `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("request body is not valid JSON: %v: %s", err, body)
	}
	if wire.Model != "test-model" {
		t.Errorf("model = %q", wire.Model)
	}
	if !wire.Stream {
		t.Error("stream must be true")
	}
	if wire.StreamOptions == nil || !wire.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage must be set (it carries the only usage report)")
	}
	if wire.MaxCompletionTokens == nil || *wire.MaxCompletionTokens <= 0 {
		t.Errorf("max_completion_tokens = %v", wire.MaxCompletionTokens)
	}
	if wire.Store == nil || *wire.Store {
		t.Errorf("store = %v, want explicit false", wire.Store)
	}

	// system (developer/system) → user → assistant(tool_calls) → tool
	if len(wire.Messages) != 4 {
		t.Fatalf("messages = %d (%s), want 4", len(wire.Messages), body)
	}
	if wire.Messages[0].Role != "system" || wire.Messages[0].Content != "You are a test." {
		t.Errorf("message 0 = %+v", wire.Messages[0])
	}
	if wire.Messages[2].Role != "assistant" || len(wire.Messages[2].ToolCalls) != 1 {
		t.Errorf("message 2 = %+v", wire.Messages[2])
	}
	if wire.Messages[2].ToolCalls[0].Function.Name != "read" {
		t.Errorf("tool call = %+v", wire.Messages[2].ToolCalls[0])
	}
	if wire.Messages[3].Role != "tool" || wire.Messages[3].ToolCallID != "call_1" {
		t.Errorf("message 3 = %+v", wire.Messages[3])
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Type != "function" || wire.Tools[0].Function.Name != "read" {
		t.Errorf("tools = %+v", wire.Tools)
	}
}

func TestDeepSeekCompatUsesMaxTokensAndReasoningReplay(t *testing.T) {
	model := testModel()
	model.Provider = ai.ProviderDeepseek
	model.BaseURL = "https://api.deepseek.com"

	var body []byte
	client, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readBody(r)
		fmt.Fprint(w, textStream("ok"))
	})
	model.BaseURL = srv.URL

	// A transcript with a thinking block: DeepSeek requires it replayed as
	// reasoning_content on the assistant message.
	tc := ai.NormalizeContext(ai.Context{Messages: []ai.Message{
		ai.NewTextMessage(ai.RoleUser, "hi"),
		{
			Role: ai.RoleAssistant,
			Content: []ai.ContentBlock{
				{Type: ai.BlockThinking, Thinking: "private thoughts"},
				ai.TextBlock("answer"),
			},
		},
		ai.NewTextMessage(ai.RoleUser, "more"),
	}})
	drain(t, client.stream(context.Background(), model, tc, Options{}))

	var wire struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
		Messages            []struct {
			Role             string `json:"role"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.MaxTokens == nil {
		t.Error("deepseek endpoints only accept max_tokens")
	}
	if wire.MaxCompletionTokens != nil {
		t.Error("max_completion_tokens must not be set for deepseek")
	}
	found := false
	for _, m := range wire.Messages {
		if m.Role == "assistant" && m.ReasoningContent == "private thoughts" {
			found = true
		}
	}
	if !found {
		t.Errorf("thinking was not replayed as reasoning_content: %s", body)
	}
}

func TestSendsAuthorizationHeader(t *testing.T) {
	var got http.Header
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, textStream("ok"))
	})
	drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if got.Get("Authorization") != "Bearer test-key" {
		t.Errorf("authorization = %q", got.Get("Authorization"))
	}
}

func TestMissingAPIKeyFailsBeforeRequest(t *testing.T) {
	// New refuses a keyless configuration outright (pi: getClientApiKey
	// throws), so the failure happens at construction, not mid-request.
	model := testModel()
	if _, err := New(Config{Model: model, MaxRetries: 0}); err == nil {
		t.Fatal("a keyless client must be rejected at construction")
	}
	// A header-owned credential satisfies the check instead.
	headers := map[string]string{"Authorization": "Bearer proxy-token"}
	if _, err := New(Config{Model: model, Headers: headers, MaxRetries: 0}); err != nil {
		t.Errorf("header-owned auth must be accepted: %v", err)
	}
}

func TestConsumerBreakingEarlyDoesNotBlock(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, textStream("a response nobody finishes reading"))
	})
	s := client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{})

	consumed := 0
	for range s.Events() {
		consumed++
		if consumed == 2 {
			break
		}
	}
	if consumed != 2 {
		t.Fatalf("consumed %d events", consumed)
	}
	done := make(chan struct{})
	go func() {
		_, _ = s.Result()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Result hung after the consumer stopped reading")
	}
}

func readBody(r *http.Request) ([]byte, error) {
	var buf strings.Builder
	if r.Body == nil {
		return []byte(buf.String()), nil
	}
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(tmp)
		buf.Write(tmp[:n])
		if err != nil {
			break
		}
	}
	return []byte(buf.String()), nil
}
