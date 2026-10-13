package anthropic

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

// sse builds one SSE frame. The API frames every event as `event:` plus
// `data:`, and reproducing that exactly is the point: a parser that only
// works because a test omitted the event line proves nothing.
func sse(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// textStream is a complete, valid single-text-block response.
func textStream(text string) string {
	var b strings.Builder
	b.WriteString(sse("message_start",
		`{"type":"message_start","message":{"id":"msg_1","model":"test-model","role":"assistant","usage":{"input_tokens":10,"output_tokens":0}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	for _, piece := range splitEvery(text, 4) {
		enc, _ := json.Marshal(piece)
		b.WriteString(sse("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(enc)+`}}`))
	}
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("message_delta",
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))
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

// collected is one drained stream: every event plus the final message.
type collected struct {
	events []ai.AssistantEvent
	names  []string
	final  ai.Message
	err    error
}

// drain consumes a stream to completion. The second slot of the Seq2 is
// asserted empty: the contract puts failures inside the stream, so an error
// there means the adapter broke the protocol.
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
		API:           ai.APIAnthropicMessages,
		Provider:      ai.ProviderAnthropic,
		BaseURL:       "", // filled per test
		ContextWindow: 200000,
		MaxTokens:     8192,
		Cost: ai.ModelCost{
			Input: 3.0, Output: 15.0, CacheRead: 0.3, CacheWrite: 3.75,
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
		AuthToken:  "test-token",
		MaxRetries: 3,
		HTTPClient: srv.Client(),
		Logger:     logging.New(logging.Options{Disabled: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Backoff shrunk to nothing so retry tests do not sleep; the ctx-aware
	// sleep itself is covered separately.
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
					ai.ToolCallBlock("toolu_1", "read", map[string]any{"path": "go.mod"}),
				},
			},
			ai.NewToolResultMessage("toolu_1", "read", "module ultrac", false),
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

	s := client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{})
	got := drain(t, s)

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
		t.Errorf("final message text = %q", ai.MessageText(done.Message))
	}
	if done.Message.Usage == nil || done.Message.Usage.Input != 10 || done.Message.Usage.Output != 25 {
		t.Errorf("final usage = %+v", done.Message.Usage)
	}
	if done.Message.ResponseID != "msg_1" {
		t.Errorf("responseId = %q", done.Message.ResponseID)
	}
	if done.Message.Usage.TotalTokens != 35 {
		t.Errorf("totalTokens = %d, want 35 (input+output)", done.Message.Usage.TotalTokens)
	}
	// Cost is computed from the model catalog, not reported by the provider.
	if done.Message.Usage.Cost.Total <= 0 {
		t.Errorf("cost total = %v, want > 0", done.Message.Usage.Cost.Total)
	}
}

// TestPartialSnapshotsGrowMonotonic checks the property that makes partials
// useful: each event sees the response so far, and the text only ever grows.
func TestPartialSnapshotsGrowMonotonic(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textStream("abcdefgh"))
	})

	s := client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{})
	got := drain(t, s)

	last := ""
	for _, ev := range got.events {
		var partial ai.Message
		switch e := ev.(type) {
		case ai.EvStart:
			partial = e.Partial
		case ai.EvTextStart:
			partial = e.Partial
		case ai.EvTextDelta:
			partial = e.Partial
		case ai.EvTextEnd:
			partial = e.Partial
		default:
			continue // done/error carry the final message, not a partial
		}
		text := ai.MessageText(partial)
		if len(text) < len(last) {
			t.Fatalf("partial shrank: %q after %q", text, last)
		}
		if !strings.HasPrefix(text, last) {
			t.Fatalf("partial %q is not an extension of %q", text, last)
		}
		last = text
	}
	if last != "abcdefgh" {
		t.Errorf("last partial = %q", last)
	}
}

// ---------------------------------------------------------------------------
// DoD: tool_use accumulated by content block index, emitted whole at stop
// ---------------------------------------------------------------------------

func TestToolUseAccumulatesByIndexAndEmitsWhole(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start",
		`{"type":"message_start","message":{"id":"msg_2","model":"test-model","role":"assistant","usage":{"input_tokens":5,"output_tokens":1}}}`))
	// text block at index 0, tool call at index 1, text again at 2 — the
	// interleaving is the point: a map keyed by index must not reorder them.
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"let me look"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"read","input":{}}}`))
	for _, frag := range []string{`{"pa`, `th":"go.`, `mod"}`} {
		enc, _ := json.Marshal(frag)
		b.WriteString(sse("content_block_delta",
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+string(enc)+`}}`))
	}
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":1}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"done"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":2}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, b.String())
	})

	s := client.stream(context.Background(), testModel(), simpleTranscript("read go.mod"), Options{})
	got := drain(t, s)

	want := []string{
		"start",
		"text_start", "text_delta", "text_end",
		"toolcall_start", "toolcall_delta", "toolcall_delta", "toolcall_delta", "toolcall_end",
		"text_start", "text_delta", "text_end",
		"done",
	}
	if strings.Join(got.names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", got.names, want)
	}

	// contentIndex is the block's position in the message, not the API index.
	start := got.events[4].(ai.EvToolCallStart)
	if start.ContentIndex != 1 {
		t.Errorf("toolcall_start contentIndex = %d, want 1", start.ContentIndex)
	}
	secondTextStart := got.events[9].(ai.EvTextStart)
	if secondTextStart.ContentIndex != 2 {
		t.Errorf("second text_start contentIndex = %d, want 2", secondTextStart.ContentIndex)
	}

	end := got.events[8].(ai.EvToolCallEnd)
	if end.ToolCall.Name != "read" || end.ToolCall.ID != "toolu_9" {
		t.Errorf("tool call = %+v", end.ToolCall)
	}
	if end.ToolCall.Arguments["path"] != "go.mod" {
		t.Errorf("arguments = %+v, want path=go.mod", end.ToolCall.Arguments)
	}

	done := got.events[len(got.events)-1].(ai.EvDone)
	if done.Reason != ai.StopToolUse {
		t.Errorf("done reason = %q, want toolUse", done.Reason)
	}
	if len(done.Message.Content) != 3 {
		t.Fatalf("final content has %d blocks, want 3", len(done.Message.Content))
	}
	if done.Message.Content[1].Type != ai.BlockToolCall {
		t.Errorf("block 1 type = %q", done.Message.Content[1].Type)
	}
}

// TestToolCallOnlyMaterialisesAtBlockStop: fragments are not calls. A
// toolcall_delta carries a JSON fragment that does not parse on its own, and
// the event's ToolCall (on end) is the only complete call.
func TestToolCallOnlyMaterialisesAtBlockStop(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"write","input":{}}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"content\":\"hel"}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"lo\"}"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))

	for _, ev := range got.events {
		if d, ok := ev.(ai.EvToolCallDelta); ok {
			if json.Valid([]byte(d.Delta)) {
				t.Errorf("toolcall_delta carried a complete JSON object %q; fragments only", d.Delta)
			}
		}
	}
	end := got.events[len(got.events)-2].(ai.EvToolCallEnd)
	if end.ToolCall.Arguments["content"] != "hello" {
		t.Errorf("arguments = %+v", end.ToolCall.Arguments)
	}
}

func TestTextInContentBlockStartIsNotDuplicated(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`))
	// A proxy that fills the text at block start and also sends the delta
	// would double the first word if both were honoured.
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello world"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	done := got.events[len(got.events)-1].(ai.EvDone)
	if ai.MessageText(done.Message) != "hello world" {
		t.Errorf("text = %q, want the delta's text once", ai.MessageText(done.Message))
	}
}

// ---------------------------------------------------------------------------
// DoD: thinking signature replay (R3 — the old engine dropped it)
// ---------------------------------------------------------------------------

func TestThinkingBlockCarriesSignatureForReplay(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one "}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step two"}}`))
	// The signature arrives in its own delta type, possibly in pieces.
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sigAAA"}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"BBB"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":1}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))

	want := []string{
		"start",
		"thinking_start", "thinking_delta", "thinking_delta", "thinking_end",
		"text_start", "text_delta", "text_end",
		"done",
	}
	if strings.Join(got.names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", got.names, want)
	}
	end := got.events[4].(ai.EvThinkingEnd)
	if end.Content != "step one step two" {
		t.Errorf("thinking_end content = %q", end.Content)
	}

	done := got.events[len(got.events)-1].(ai.EvDone)
	thinking := done.Message.Content[0]
	if thinking.Type != ai.BlockThinking {
		t.Fatalf("block 0 type = %q", thinking.Type)
	}
	if thinking.ThinkingSignature != "sigAAABBB" {
		t.Errorf("signature = %q, want the reassembled sigAAABBB", thinking.ThinkingSignature)
	}

	// The signature must survive the round trip into the next request: a
	// thinking block without it makes Anthropic reject the whole request.
	tc := ai.NormalizeContext(ai.Context{Messages: []ai.Message{done.Message}})
	body, err := client.encodeRequest(testModel(), tc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) == 0 || len(wire.Messages[0].Content) == 0 {
		t.Fatalf("no assistant message in request: %s", body)
	}
	block := wire.Messages[0].Content[0]
	if block.Type != "thinking" || block.Signature != "sigAAABBB" {
		t.Errorf("replayed thinking block = %+v, want type=thinking signature=sigAAABBB", block)
	}
}

// TestThinkingWithoutSignatureDegradesToText: a block that lost its
// signature (aborted stream, hand-built transcript) must not poison the
// next request.
func TestThinkingWithoutSignatureDegradesToText(t *testing.T) {
	tc := ai.NormalizeContext(ai.Context{Messages: []ai.Message{{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			{Type: ai.BlockThinking, Thinking: "orphan thoughts"},
		},
	}}})
	client, err := New(Config{Model: testModel()})
	if err != nil {
		t.Fatal(err)
	}
	body, err := client.encodeRequest(testModel(), tc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"orphan thoughts"`) {
		t.Errorf("orphan thinking text missing from request: %s", body)
	}
	if strings.Contains(string(body), `"type":"thinking"`) {
		t.Errorf("signature-less thinking block was sent as thinking: %s", body)
	}
}

// ---------------------------------------------------------------------------
// DoD: truncated streams are reported, never silently accepted
// ---------------------------------------------------------------------------

func TestMaxTokensMapsToLength(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut off"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	done := got.events[len(got.events)-1].(ai.EvDone)
	if done.Reason != ai.StopLength {
		t.Errorf("reason = %q, want length (the loop refuses to run tools on a truncated batch)", done.Reason)
	}
}

func TestTruncatedStreamIsReportedNotSilentlyAccepted(t *testing.T) {
	cases := map[string]string{
		"no stop reason": strings.Join([]string{
			sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`),
			sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half a"}}`),
			sse("message_stop", `{"type":"message_stop"}`),
		}, ""),
		"no message_start": strings.Join([]string{
			sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half a"}}`),
			sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
			sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`),
			sse("message_stop", `{"type":"message_stop"}`),
		}, ""),
		"unclosed block": strings.Join([]string{
			sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`),
			sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half a"}}`),
			sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`),
			sse("message_stop", `{"type":"message_stop"}`),
		}, ""),
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			})
			got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
			last := got.events[len(got.events)-1]
			errEv, ok := last.(ai.EvError)
			if !ok {
				t.Fatalf("last event = %s, want error", ai.Name(last))
			}
			if errEv.Reason != ai.StopError {
				t.Errorf("reason = %q, want error", errEv.Reason)
			}
			if errEv.Error.ErrorMessage == "" {
				t.Error("error event carries no message")
			}
			// The partial text that did arrive is still visible on the
			// error message, so a consumer can show what it got.
			if ai.MessageText(errEv.Error) != "half a" {
				t.Errorf("error message text = %q", ai.MessageText(errEv.Error))
			}
		})
	}
}

func TestStreamErrorEventBecomesErrorEvent(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`))
	b.WriteString(sse("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	last := got.events[len(got.events)-1].(ai.EvError)
	if !strings.Contains(last.Error.ErrorMessage, "Overloaded") {
		t.Errorf("error message = %q", last.Error.ErrorMessage)
	}
}

func TestInvalidToolInputIsReportedNotEmitted(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"read","input":{}}}`))
	// Never closed: the JSON is still half an object at block stop.
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	for _, ev := range got.events {
		if _, ok := ev.(ai.EvToolCallEnd); ok {
			t.Error("a tool call with invalid JSON was emitted")
		}
	}
	last := got.events[len(got.events)-1].(ai.EvError)
	if !strings.Contains(last.Error.ErrorMessage, "invalid JSON") {
		t.Errorf("error = %q, want the invalid-JSON report", last.Error.ErrorMessage)
	}
}

func TestProtocolViolationIsReported(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"ghost"}}`))

	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, b.String())
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("x"), Options{}))
	last := got.events[len(got.events)-1].(ai.EvError)
	if !strings.Contains(last.Error.ErrorMessage, "never started") {
		t.Errorf("error = %q", last.Error.ErrorMessage)
	}
}

// ---------------------------------------------------------------------------
// DoD: ctx cancellation interrupts in-flight requests
// ---------------------------------------------------------------------------

func TestCancellationInterruptsInFlightStream(t *testing.T) {
	release := make(chan struct{})
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test lets go
	})

	ctx, cancel := context.WithCancel(context.Background())
	s := client.stream(ctx, testModel(), simpleTranscript("hi"), Options{})

	// Read in a goroutine so the test can cancel while nothing has arrived.
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
				if last == nil {
					t.Fatal("stream produced no events")
				}
				errEv, ok := last.(ai.EvError)
				if !ok {
					t.Fatalf("last event = %s, want error", ai.Name(last))
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

func TestCancellationBeforeRequestSendsNothing(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, textStream("never seen"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := drain(t, client.stream(ctx, testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 0 {
		t.Errorf("handler was hit %d times on a cancelled context", hits.Load())
	}
	last := got.events[len(got.events)-1].(ai.EvError)
	if last.Reason != ai.StopAborted {
		t.Errorf("reason = %q, want aborted", last.Reason)
	}
}

// ---------------------------------------------------------------------------
// DoD: retries only before the first event
// ---------------------------------------------------------------------------

func TestRetriesOnRateLimitThenSucceeds(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textStream("recovered"))
	})

	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 3 {
		t.Fatalf("requests = %d, want 3 (two retries then success)", hits.Load())
	}
	done := got.events[len(got.events)-1].(ai.EvDone)
	if ai.MessageText(done.Message) != "recovered" {
		t.Errorf("text = %q", ai.MessageText(done.Message))
	}
}

func TestRetriesOnServerError(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"type":"api_error","message":"boom"}}`)
			return
		}
		fmt.Fprint(w, textStream("ok"))
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 2 {
		t.Fatalf("requests = %d, want 2", hits.Load())
	}
	if _, ok := got.events[len(got.events)-1].(ai.EvDone); !ok {
		t.Fatalf("last event = %s, want done", got.names[len(got.names)-1])
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"bad"}}`)
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (a 400 is wrong the same way next time)", hits.Load())
	}
	last := got.events[len(got.events)-1].(ai.EvError)
	if !strings.Contains(last.Error.ErrorMessage, "bad") {
		t.Errorf("error = %q", last.Error.ErrorMessage)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 4 { // 1 initial + 3 retries
		t.Fatalf("requests = %d, want 4", hits.Load())
	}
	if _, ok := got.events[len(got.events)-1].(ai.EvError); !ok {
		t.Error("a exhausted-retry stream must end in error")
	}
}

// TestMidStreamFailureIsNotRetried: once an event has been pushed, replaying
// would show the consumer the same text twice.
func TestMidStreamFailureIsNotRetried(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 200, some text, then a hard close mid-stream.
		fmt.Fprint(w, sse("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":1,"output_tokens":0}}}`))
		fmt.Fprint(w, sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		fmt.Fprint(w, sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // abort the connection after the flush
	})
	got := drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (no replay after events)", hits.Load())
	}
	if _, ok := got.events[len(got.events)-1].(ai.EvError); !ok {
		t.Fatalf("last event = %s, want error", got.names[len(got.names)-1])
	}
}

func TestRetryAfterHeaderWinsOverComputedBackoff(t *testing.T) {
	var slept time.Duration
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"slow"}}`)
	})
	client.sleep = func(ctx context.Context, d time.Duration) error {
		slept = d
		return nil
	}

	drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if slept != 7*time.Second {
		t.Errorf("slept %v, want the server's 7s", slept)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	client, err := New(Config{Model: testModel(), MaxRetries: 10})
	if err != nil {
		t.Fatal(err)
	}
	client.randFloat = func() float64 { return 0 }

	apiErr := &logging.APIError{StatusCode: 500}
	prev := time.Duration(0)
	for attempt := 0; attempt < 6; attempt++ {
		d := client.retryDelay(attempt, apiErr)
		if d <= prev {
			t.Fatalf("delay did not grow at attempt %d: %v after %v", attempt, d, prev)
		}
		prev = d
	}
	if prev > client.retryMax {
		t.Errorf("delay %v exceeds the cap %v", prev, client.retryMax)
	}
}

// ---------------------------------------------------------------------------
// request encoding
// ---------------------------------------------------------------------------

func TestRequestBodyMatchesTheAPI(t *testing.T) {
	var body []byte
	client, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAllBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textStream("ok"))
	})
	model := testModel()
	model.BaseURL = srv.URL

	drain(t, client.stream(context.Background(), model, toolTranscript(), Options{}))

	var wire struct {
		Model     string `json:"model"`
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ToolUseID string          `json:"tool_use_id"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				IsError   bool            `json:"is_error"`
			} `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("request body is not valid JSON: %v: %s", err, body)
	}
	if wire.Model != "test-model" {
		t.Errorf("model = %q", wire.Model)
	}
	if wire.System != "You are a test." {
		t.Errorf("system = %q", wire.System)
	}
	if !wire.Stream {
		t.Error("stream must be true")
	}
	if wire.MaxTokens <= 0 {
		t.Errorf("max_tokens = %d", wire.MaxTokens)
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Name != "read" {
		t.Fatalf("tools = %+v", wire.Tools)
	}
	if len(wire.Tools[0].InputSchema) == 0 {
		t.Error("tool has no input_schema")
	}

	// The transcript is user → assistant(tool_use) → toolResult; on the wire
	// the toolResult folds into one user message right after the assistant.
	if len(wire.Messages) != 3 {
		t.Fatalf("messages = %d (%s), want 3", len(wire.Messages), body)
	}
	if wire.Messages[0].Role != "user" || wire.Messages[0].Content[0].Text != "read go.mod" {
		t.Errorf("message 0 = %+v", wire.Messages[0])
	}
	if wire.Messages[1].Role != "assistant" || wire.Messages[1].Content[0].Type != "tool_use" {
		t.Errorf("message 1 = %+v", wire.Messages[1])
	}
	if wire.Messages[2].Role != "user" || wire.Messages[2].Content[0].Type != "tool_result" {
		t.Errorf("message 2 = %+v", wire.Messages[2])
	}
	if wire.Messages[2].Content[0].ToolUseID != "toolu_1" {
		t.Errorf("tool_result id = %q", wire.Messages[2].Content[0].ToolUseID)
	}
}

func TestSendsRequiredHeaders(t *testing.T) {
	var got http.Header
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, textStream("ok"))
	})
	drain(t, client.stream(context.Background(), testModel(), simpleTranscript("hi"), Options{}))
	if got.Get("anthropic-version") != defaultAPIVersion {
		t.Errorf("anthropic-version = %q", got.Get("anthropic-version"))
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q", got.Get("Content-Type"))
	}
	if got.Get("Accept") != "text/event-stream" {
		t.Errorf("accept = %q", got.Get("Accept"))
	}
	if got.Get("Authorization") != "Bearer test-token" {
		t.Errorf("authorization = %q", got.Get("Authorization"))
	}
}

func TestAPIKeyUsesXApiKeyHeader(t *testing.T) {
	var got http.Header
	model := testModel()
	client, err := New(Config{
		Model:      model,
		APIKey:     "sk-first-party",
		MaxRetries: 0,
		HTTPClient: http.DefaultClient,
		Logger:     logging.New(logging.Options{Disabled: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, textStream("ok"))
	}))
	defer srv.Close()
	client.baseURL = srv.URL

	drain(t, client.stream(context.Background(), model, simpleTranscript("hi"), Options{}))
	if got.Get("x-api-key") != "sk-first-party" {
		t.Errorf("x-api-key = %q", got.Get("x-api-key"))
	}
	if got.Get("Authorization") != "" {
		t.Errorf("authorization must be absent for a first-party key, got %q", got.Get("Authorization"))
	}
}

func TestThinkingRequestAddsBetaAndBudget(t *testing.T) {
	var body []byte
	client, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAllBody(r)
		fmt.Fprint(w, textStream("ok"))
	})
	model := testModel()
	model.BaseURL = srv.URL
	model.Reasoning = true
	model.MaxTokens = 8192

	drain(t, client.stream(context.Background(), model, simpleTranscript("hi"), Options{
		StreamOptions: ai.StreamOptions{Reasoning: ai.ThinkMedium},
	}))

	var wire struct {
		MaxTokens int `json:"max_tokens"`
		Thinking  *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		Betas []string `json:"betas"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Thinking == nil || wire.Thinking.Type != "enabled" {
		t.Fatalf("thinking = %+v", wire.Thinking)
	}
	// medium's default budget (8192) collides with the model ceiling (8192),
	// so it clamps to leave answer room: 8192 - 1024.
	if wire.Thinking.BudgetTokens != 7168 {
		t.Errorf("budget = %d, want 7168", wire.Thinking.BudgetTokens)
	}
	if wire.MaxTokens != 8192 {
		t.Errorf("max_tokens = %d, want the model ceiling 8192", wire.MaxTokens)
	}
	found := false
	for _, b := range wire.Betas {
		if b == InterleavedThinkingBeta {
			found = true
		}
	}
	if !found {
		t.Errorf("betas = %v, want the interleaved-thinking beta", wire.Betas)
	}
}

func TestTemperatureOmittedWhenThinking(t *testing.T) {
	var body []byte
	client, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAllBody(r)
		fmt.Fprint(w, textStream("ok"))
	})
	model := testModel()
	model.BaseURL = srv.URL
	model.Reasoning = true
	temp := 0.5
	drain(t, client.stream(context.Background(), model, simpleTranscript("hi"), Options{
		StreamOptions: ai.StreamOptions{Reasoning: ai.ThinkLow, Temperature: &temp},
	}))
	if strings.Contains(string(body), "temperature") {
		t.Errorf("temperature must be omitted with thinking: %s", body)
	}
}

func TestToolCallIDIsNormalized(t *testing.T) {
	tc := ai.NormalizeContext(ai.Context{Messages: []ai.Message{{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			ai.ToolCallBlock("call abc/123+xyz", "read", map[string]any{"path": "x"}),
		},
	}}})
	client, err := New(Config{Model: testModel()})
	if err != nil {
		t.Fatal(err)
	}
	body, err := client.encodeRequest(testModel(), tc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"id":"call_abc_123_xyz"`) {
		t.Errorf("id not normalized: %s", body)
	}
}

func TestEmptyTextBlocksAreStripped(t *testing.T) {
	tc := ai.NormalizeContext(ai.Context{Messages: []ai.Message{{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			ai.TextBlock(""),
			ai.TextBlock("real"),
		},
	}}})
	client, err := New(Config{Model: testModel()})
	if err != nil {
		t.Fatal(err)
	}
	body, err := client.encodeRequest(testModel(), tc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), `"type":"text"`) != 1 {
		t.Errorf("empty text block not stripped: %s", body)
	}
}

func TestMessageWithOnlyEmptyTextIsRejected(t *testing.T) {
	tc := ai.NormalizeContext(ai.Context{Messages: []ai.Message{{
		Role:    ai.RoleUser,
		Content: []ai.ContentBlock{ai.TextBlock("   ")},
	}}})
	client, err := New(Config{Model: testModel()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.encodeRequest(testModel(), tc, Options{}); err == nil {
		t.Error("a message with no usable content must be rejected, not sent empty")
	}
}

func TestRequestValidationFailsBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, textStream("ok"))
	})
	// A tool with no schema cannot be called correctly; the request must
	// fail locally rather than at the API.
	tc := ai.NormalizeContext(ai.Context{
		Messages: []ai.Message{ai.NewTextMessage(ai.RoleUser, "hi")},
		Tools:    []ai.Tool{{Name: "broken"}},
	})
	got := drain(t, client.stream(context.Background(), testModel(), tc, Options{}))
	if hits.Load() != 0 {
		t.Errorf("handler hit %d times", hits.Load())
	}
	last := got.events[len(got.events)-1].(ai.EvError)
	if !strings.Contains(last.Error.ErrorMessage, "input schema") {
		t.Errorf("error = %q", last.Error.ErrorMessage)
	}
}

func TestDefaultMaxTokensIsApplied(t *testing.T) {
	model := testModel()
	model.MaxTokens = 0
	var body []byte
	client, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAllBody(r)
		fmt.Fprint(w, textStream("ok"))
	})
	model.BaseURL = srv.URL
	drain(t, client.stream(context.Background(), model, simpleTranscript("hi"), Options{}))
	if !strings.Contains(string(body), `"max_tokens":4096`) {
		t.Errorf("max_tokens default not applied: %s", body)
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
			break // stop reading mid-stream
		}
	}
	if consumed != 2 {
		t.Fatalf("consumed %d events", consumed)
	}
	// If the producer were blocked on a send to a dead consumer, the test
	// binary would deadlock at exit; reaching here with a clean close is
	// the assertion. Result must also not hang forever.
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

func readAllBody(r *http.Request) ([]byte, error) {
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
