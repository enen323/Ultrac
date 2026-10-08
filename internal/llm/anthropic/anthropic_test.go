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

	"ultrac/internal/contract/provider"
	"ultrac/internal/logging"
)

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
		`{"type":"message_start","message":{"id":"msg_1","model":"test","role":"assistant","usage":{"input_tokens":10,"output_tokens":0}}}`))
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

// collect drains a Seq2, returning the chunks and the first error.
func collect(seq func(func(provider.Chunk, error) bool)) ([]provider.Chunk, error) {
	var chunks []provider.Chunk
	var firstErr error
	for ch, err := range seq {
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// Keep draining: a caller must see what arrived before the
			// failure, and stopping here would hide it.
			continue
		}
		chunks = append(chunks, ch)
	}
	return chunks, firstErr
}

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := NewClient(provider.Config{
		BaseURL:   srv.URL,
		Model:     "test-model",
		AuthToken: "test-token",
	}, WithHTTPClient(srv.Client()),
		WithMaxRetries(3),
		WithBackoff(time.Millisecond, 2*time.Millisecond),
		// The logger writes to stderr; silence it so a passing test does not
		// print a retry warning.
		WithLogger(logging.New(logging.Options{Disabled: true})),
	)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	c.randFloat = func() float64 { return 0 }
	return c, srv
}

// TestNilOptionIsIgnored is a regression test.
//
// New is a provider.Factory, so it has no options parameter and used to call
// NewClient(cfg, nil) to satisfy the variadic. That does not mean "no
// options" — it means a one-element slice containing a nil Option, which
// panicked when applied. Every configured provider hit this on the first real
// request, while every test that went through NewClient directly passed.
func TestNilOptionIsIgnored(t *testing.T) {
	p, err := provider.New(provider.Config{
		Kind:      Kind,
		BaseURL:   "https://example.invalid",
		Model:     "m",
		AuthToken: "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != Kind {
		t.Errorf("provider name = %q", p.Name())
	}

	// The explicit nil form too, so the guard itself is covered.
	if _, err := NewClient(provider.Config{Model: "m", BaseURL: "https://x"}, nil); err != nil {
		t.Errorf("an explicit nil option must be ignored, got %v", err)
	}
}

// TestRegistryConstructionMatchesFactorySignature guards the seam the nil-option
// bug lived in.
func TestRegistryConstructionMatchesFactorySignature(t *testing.T) {
	var f provider.Factory = New
	p, err := f(provider.Config{Model: "m", BaseURL: "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind() != Kind {
		t.Errorf("kind = %q", p.Kind())
	}
}

func simpleRequest() provider.Request {
	return provider.Request{
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Kind: provider.BlockText, Text: "hi"}},
		}},
	}
}

// ---------------------------------------------------------------------------
// DoD: a real request streams text deltas
// ---------------------------------------------------------------------------

func TestStreamsTextDeltas(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textStream("PROVIDER-OK"))
	})

	chunks, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	var stop provider.StopReason
	var usage *provider.Usage
	for _, ch := range chunks {
		switch ch.Kind {
		case provider.ChunkText:
			text.WriteString(ch.Text)
		case provider.ChunkStop:
			stop = ch.Stop
		case provider.ChunkUsage:
			usage = ch.Usage
		}
	}

	if text.String() != "PROVIDER-OK" {
		t.Errorf("streamed text = %q", text.String())
	}
	if stop != provider.StopEndTurn {
		t.Errorf("stop reason = %q", stop)
	}
	if usage == nil || usage.OutputTokens != 25 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestSendsRequiredHeaders(t *testing.T) {
	var got http.Header
	var gotPath string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, textStream("ok"))
	})

	if _, err := collect(c.Stream(context.Background(), simpleRequest())); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if got.Get("anthropic-version") == "" {
		t.Error("anthropic-version is required and was not sent")
	}
	if got.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept = %q", got.Get("Accept"))
	}
	if got.Get("Authorization") != "Bearer test-token" {
		t.Errorf("Authorization = %q", got.Get("Authorization"))
	}
	// A proxy token and an API key are alternatives. Sending both makes some
	// proxies reject the request outright.
	if got.Get("x-api-key") != "" {
		t.Error("x-api-key must not be sent alongside a proxy token")
	}
}

func TestAPIKeyUsesXApiKeyHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, textStream("ok"))
	}))
	defer srv.Close()

	c, _ := NewClient(provider.Config{BaseURL: srv.URL, Model: "m", APIKey: "sk-test"},
		WithHTTPClient(srv.Client()),
		WithLogger(logging.New(logging.Options{Disabled: true})))
	if _, err := collect(c.Stream(context.Background(), simpleRequest())); err != nil {
		t.Fatal(err)
	}
	if got.Get("x-api-key") != "sk-test" {
		t.Errorf("x-api-key = %q", got.Get("x-api-key"))
	}
	if got.Get("Authorization") != "" {
		t.Error("Authorization must be absent when only an API key is configured")
	}
}

func TestRequestBodyMatchesTheAPI(t *testing.T) {
	var body map[string]any
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, textStream("ok"))
	})

	req := provider.Request{
		Model:     "test-model",
		System:    "be brief",
		MaxTokens: 1000,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Kind: provider.BlockText, Text: "read a.txt"}}},
			{Role: provider.RoleAssistant, Blocks: []provider.Block{
				{Kind: provider.BlockToolUse, ToolUseID: "toolu_1", ToolName: "Read", Input: json.RawMessage(`{"file_path":"a.txt"}`)},
			}},
			{Role: provider.RoleUser, Blocks: []provider.Block{
				{Kind: provider.BlockToolResult, ToolUseID: "toolu_1", Text: "contents", IsError: false},
			}},
		},
		Tools: []provider.ToolSchema{{
			Name:        "Read",
			Description: "read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}}}`),
		}},
	}
	if _, err := collect(c.Stream(context.Background(), req)); err != nil {
		t.Fatal(err)
	}

	if body["stream"] != true {
		t.Error(`"stream":true is required or the API returns a JSON blob`)
	}
	if body["max_tokens"] != float64(1000) {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}
	if body["model"] != "test-model" {
		t.Errorf("model = %v", body["model"])
	}
	if body["system"] != "be brief" {
		t.Errorf("system = %v", body["system"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("sent %d messages, want 3", len(msgs))
	}

	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("sent %d tools", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "Read" {
		t.Errorf("tool name = %v", tool["name"])
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Error("the API requires input_schema, not inputSchema")
	}

	// The tool_result block must carry tool_use_id, or the API cannot match it
	// to the call it answers.
	third := msgs[2].(map[string]any)
	blocks := third["content"].([]any)
	block := blocks[0].(map[string]any)
	if block["type"] != "tool_result" {
		t.Errorf("block type = %v", block["type"])
	}
	if block["tool_use_id"] != "toolu_1" {
		t.Errorf("tool_use_id = %v", block["tool_use_id"])
	}
}

// ---------------------------------------------------------------------------
// DoD: tool_use accumulates by index and is emitted only when complete
// ---------------------------------------------------------------------------

// interleavedToolStream has text at index 0, a tool call at index 1 whose
// input arrives in four fragments, and more text at index 2. That interleaving
// is the case a naive "accumulate into one buffer" implementation gets wrong.
func interleavedToolStream() string {
	var b strings.Builder
	b.WriteString(sse("message_start",
		`{"type":"message_start","message":{"id":"msg_2","usage":{"input_tokens":5,"output_tokens":0}}}`))

	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look."}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))

	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc","name":"Read","input":{}}}`))
	for _, frag := range []string{`{"file`, `_path":`, `"a.txt`, `","limit":50}`} {
		enc, _ := json.Marshal(frag)
		b.WriteString(sse("content_block_delta",
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+string(enc)+`}}`))
	}
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":1}`))

	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Done."}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":2}`))

	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

func TestToolUseAccumulatesByIndexAndEmitsWhole(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, interleavedToolStream())
	})

	chunks, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err != nil {
		t.Fatal(err)
	}

	var calls []provider.ToolCall
	var texts []string
	var stop provider.StopReason
	for _, ch := range chunks {
		switch ch.Kind {
		case provider.ChunkToolCall:
			calls = append(calls, *ch.ToolCall)
		case provider.ChunkText:
			texts = append(texts, ch.Text)
		case provider.ChunkStop:
			stop = ch.Stop
		}
	}

	if len(calls) != 1 {
		t.Fatalf("emitted %d tool calls, want exactly 1 (a partial call must never escape)", len(calls))
	}
	call := calls[0]
	if call.ID != "toolu_abc" || call.Name != "Read" {
		t.Errorf("call = %+v", call)
	}
	// The whole point: parseable JSON, assembled from four fragments.
	if !json.Valid(call.Input) {
		t.Fatalf("accumulated input is not valid JSON: %s", call.Input)
	}
	var in struct {
		FilePath string `json:"file_path"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		t.Fatal(err)
	}
	if in.FilePath != "a.txt" || in.Limit != 50 {
		t.Errorf("accumulated input = %s", call.Input)
	}

	// Text from both text blocks must arrive, and neither may be duplicated by
	// a re-emission at content_block_stop.
	if strings.Join(texts, "") != "Let me look.Done." {
		t.Errorf("text chunks = %v", texts)
	}
	if stop != provider.StopToolUse {
		t.Errorf("stop = %q, want tool_use", stop)
	}
}

// TestToolCallOnlyMaterialisesAtBlockStop asserts the ordering guarantee
// directly on the accumulator.
//
// This was originally a goroutine-and-channel test that held a live stream open
// mid tool-call. That was testing the timing of a race rather than the
// invariant, and it hung whenever the scheduling came out differently. The
// guarantee — no tool-shaped chunk before content_block_stop — is a property
// of consume(), so it is tested where it lives.
func TestToolCallOnlyMaterialisesAtBlockStop(t *testing.T) {
	acc := newAccumulator()

	feed := func(payload string) []provider.Chunk {
		t.Helper()
		res, err := acc.consume([]byte(payload))
		if err != nil {
			t.Fatalf("consume(%s): %v", payload, err)
		}
		return res.Chunks
	}

	feed(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)
	feed(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Write","input":{}}}`)

	for _, frag := range []string{`{"a":`, `1,`, `"b":`, `2}`} {
		chunks := feed(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + mustJSON(frag) + `}}`)
		// Every one of these arrives mid-arguments. Emitting any of them would
		// hand a consumer JSON that cannot decode.
		for _, ch := range chunks {
			if ch.Kind == provider.ChunkToolCall {
				t.Fatalf("a tool call was materialised mid-arguments, from %s", frag)
			}
		}
	}

	// The accumulator is holding the fragments, but has emitted nothing.
	blocks := acc.blocksInOrder()
	if len(blocks) != 1 {
		t.Fatalf("expected 1 accumulated block, got %d", len(blocks))
	}
	if !strings.Contains(blocks[0].input.String(), `"a":1`) {
		t.Errorf("fragments were not accumulated: %q", blocks[0].input.String())
	}

	chunks := feed(`{"type":"content_block_stop","index":0}`)
	if len(chunks) != 1 {
		t.Fatalf("content_block_stop produced %d chunks, want 1", len(chunks))
	}
	call := chunks[0].ToolCall
	if call == nil {
		t.Fatal("the tool call was not emitted at block stop")
	}
	if !json.Valid(call.Input) {
		t.Fatalf("emitted input is not valid JSON: %s", call.Input)
	}
	if string(call.Input) != `{"a":1,"b":2}` {
		t.Errorf("input = %s, want the fragments in order", call.Input)
	}
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestTextInContentBlockStartIsNotDuplicated(t *testing.T) {
	// The API repeats the block's text in the following delta. Honouring both
	// would double the first word of every message.
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Hello"}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))

	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, b.String()) })

	chunks, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, ch := range chunks {
		if ch.Kind == provider.ChunkText {
			text.WriteString(ch.Text)
		}
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q, want Hello exactly once", text.String())
	}
}

func TestThinkingDeltasStreamSeparately(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":1}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))

	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, b.String()) })

	chunks, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err != nil {
		t.Fatal(err)
	}
	var thinking, text strings.Builder
	for _, ch := range chunks {
		switch ch.Kind {
		case provider.ChunkThinking:
			thinking.WriteString(ch.Text)
		case provider.ChunkText:
			text.WriteString(ch.Text)
		}
	}
	if thinking.String() != "hmm" {
		t.Errorf("thinking = %q", thinking.String())
	}
	// Reasoning must not leak into the user-visible text.
	if text.String() != "answer" {
		t.Errorf("text = %q", text.String())
	}
}

// ---------------------------------------------------------------------------
// DoD: ctx cancellation interrupts an in-flight request, no goroutine leak
// ---------------------------------------------------------------------------

func TestCancellationInterruptsInFlightStream(t *testing.T) {
	started := make(chan struct{})
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, sse("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
		fmt.Fprint(w, sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		fmt.Fprint(w, sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`))
		flusher.Flush()
		close(started)
		// Block until the client goes away. If cancellation does not reach the
		// transport, this handler never returns and the server Close hangs.
		<-r.Context().Done()
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var firstErr error
	got := 0
	for _, err := range c.Stream(ctx, simpleRequest()) {
		if err != nil {
			firstErr = err
			break
		}
		got++
		if got == 1 {
			<-started
			cancel()
		}
	}

	if firstErr == nil {
		t.Fatal("expected the cancelled stream to report an error")
	}
	if !errorsIs(firstErr, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", firstErr)
	}
}

func TestCancellationBeforeRequestSendsNothing(t *testing.T) {
	var hits atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, textStream("ok"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := collect(c.Stream(ctx, simpleRequest()))
	if err == nil {
		t.Fatal("expected an error from an already-cancelled context")
	}
	if hits.Load() != 0 {
		t.Error("a request was sent despite the context being cancelled")
	}
}

func TestCancellationDuringBackoffAbortsImmediately(t *testing.T) {
	// A 30-second backoff must not outlive the user's decision to stop. This
	// asserts the sleep is interruptible rather than waiting it out.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()

	c, _ := NewClient(provider.Config{BaseURL: srv.URL, Model: "m", AuthToken: "t"},
		WithHTTPClient(srv.Client()),
		WithMaxRetries(10),
		WithBackoff(30*time.Second, 30*time.Second),
		WithLogger(logging.New(logging.Options{Disabled: true})))
	// The real sleepCtx, so cancellation is what ends the wait.
	c.sleep = sleepCtx
	c.randFloat = func() float64 { return 0 }

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := collect(c.Stream(ctx, simpleRequest()))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the rate limit to surface as an error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v; the backoff was not interrupted by cancellation", elapsed)
	}
	if attempts.Load() != 1 {
		t.Errorf("made %d attempts; a cancelled backoff must not retry", attempts.Load())
	}
}

// ---------------------------------------------------------------------------
// DoD: 429 and 5xx retry with backoff
// ---------------------------------------------------------------------------

func TestRetriesOnRateLimitThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		fmt.Fprint(w, textStream("recovered"))
	})

	chunks, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, ch := range chunks {
		if ch.Kind == provider.ChunkText {
			text.WriteString(ch.Text)
		}
	}
	if text.String() != "recovered" {
		t.Errorf("text = %q", text.String())
	}
	if attempts.Load() != 3 {
		t.Errorf("made %d attempts, want 3", attempts.Load())
	}
}

func TestRetriesOnServerError(t *testing.T) {
	var attempts atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"type":"api_error","message":"upstream"}}`)
			return
		}
		fmt.Fprint(w, textStream("ok"))
	})
	if _, err := collect(c.Stream(context.Background(), simpleRequest())); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Errorf("made %d attempts, want 2", attempts.Load())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	// A 400 is wrong the same way next time. Retrying burns quota to
	// reproduce it.
	var attempts atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"max_tokens too large"}}`)
	})

	_, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err == nil {
		t.Fatal("expected an error")
	}
	if attempts.Load() != 1 {
		t.Errorf("a 400 was retried %d times", attempts.Load()-1)
	}
	apiErr, ok := logging.AsAPIError(err)
	if !ok {
		t.Fatalf("error is not typed: %v", err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("status = %d", apiErr.StatusCode)
	}
	// The server's own code and message must survive, because both the retry
	// policy and the user-facing surface branch on them.
	if apiErr.Code != "invalid_request_error" {
		t.Errorf("code = %q", apiErr.Code)
	}
	if apiErr.Message != "max_tokens too large" {
		t.Errorf("message = %q", apiErr.Message)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"type":"overloaded_error","message":"busy"}}`)
	})

	_, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	// maxRetries is 3 in testClient, so 4 attempts total.
	if attempts.Load() != 4 {
		t.Errorf("made %d attempts, want 4 (1 initial + 3 retries)", attempts.Load())
	}
}

func TestRetryAfterHeaderWinsOverComputedBackoff(t *testing.T) {
	// A server that tells you when to come back should be believed. Backing
	// off on our own schedule anyway is how a rate limit becomes a ban.
	var delays []time.Duration
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 2 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, textStream("ok"))
	}))
	defer srv.Close()

	c, _ := NewClient(provider.Config{BaseURL: srv.URL, Model: "m", AuthToken: "t"},
		WithHTTPClient(srv.Client()),
		WithMaxRetries(2),
		WithBackoff(50*time.Millisecond, time.Second),
		WithLogger(logging.New(logging.Options{Disabled: true})))
	c.sleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return ctx.Err()
	}

	if _, err := collect(c.Stream(context.Background(), simpleRequest())); err != nil {
		t.Fatal(err)
	}
	if len(delays) != 1 {
		t.Fatalf("slept %d times, want 1", len(delays))
	}
	if delays[0] != 7*time.Second {
		t.Errorf("waited %v, want the 7s the server asked for", delays[0])
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {})
	c.retryBase = 100 * time.Millisecond
	c.retryMax = 800 * time.Millisecond
	c.randFloat = func() float64 { return 0 }

	var prev time.Duration
	for attempt := range 8 {
		d := c.retryDelay(attempt, &logging.APIError{StatusCode: 500})
		if d > c.retryMax {
			t.Errorf("attempt %d waited %v, over the %v ceiling", attempt, d, c.retryMax)
		}
		if d < prev {
			t.Errorf("attempt %d waited %v, less than the previous %v", attempt, d, prev)
		}
		prev = d
	}
}

func TestJitterVariesTheDelay(t *testing.T) {
	// Identical jitter across clients means they retry in lockstep and
	// reproduce the overload they are backing off from.
	seen := map[time.Duration]bool{}
	for range 50 {
		v, ok := jitterFloat()
		if !ok {
			t.Skip("crypto/rand unavailable")
		}
		if v < 0 || v >= 1 {
			t.Fatalf("jitter %v is outside [0,1)", v)
		}
		seen[time.Duration(v*1000)] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct jitter values in 50 draws; it looks constant", len(seen))
	}
}

// ---------------------------------------------------------------------------
// Error surfacing
// ---------------------------------------------------------------------------

func TestTruncatedStreamIsReportedNotSilentlyAccepted(t *testing.T) {
	// Without this, a stream cut off mid-answer looks like a complete one and
	// the user has no way to tell.
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half an ans"}}`))
	// no content_block_stop, no message_delta, no message_stop

	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, b.String()) })

	chunks, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err == nil {
		t.Fatal("a truncated stream must be reported")
	}
	// What arrived before the cut is still delivered.
	var text strings.Builder
	for _, ch := range chunks {
		if ch.Kind == provider.ChunkText {
			text.WriteString(ch.Text)
		}
	}
	if text.String() != "half an ans" {
		t.Errorf("partial output was lost: %q", text.String())
	}
}

func TestStreamErrorEventBecomesAPIError(t *testing.T) {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	b.WriteString(sse("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))

	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, b.String()) })

	_, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err == nil {
		t.Fatal("expected an error event to surface")
	}
	apiErr, ok := logging.AsAPIError(err)
	if !ok {
		t.Fatalf("not a typed API error: %v", err)
	}
	// The status is absent mid-stream but the body says overloaded, which is
	// how a 529 is recognised during a stream.
	if !apiErr.IsOverloaded() {
		t.Errorf("overloaded_error was not recognised: %+v", apiErr)
	}
}

func TestProtocolViolationIsReported(t *testing.T) {
	// A delta for a block that was never started means the stream contradicts
	// what we sent. Continuing would attach text to the wrong block.
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"orphan"}}`))

	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, b.String()) })

	_, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err == nil {
		t.Fatal("expected a protocol error")
	}
	if !strings.Contains(err.Error(), "never started") {
		t.Errorf("error should name the problem: %v", err)
	}
}

func TestInvalidToolInputIsReportedNotEmitted(t *testing.T) {
	// Handing the engine JSON that cannot decode fails somewhere further from
	// the cause. Failing here names the tool and the bad bytes.
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}`))
	b.WriteString(sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"unclosed\":"}}`))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))

	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, b.String()) })

	chunks, err := collect(c.Stream(context.Background(), simpleRequest()))
	if err == nil {
		t.Fatal("invalid accumulated JSON must be reported")
	}
	for _, ch := range chunks {
		if ch.Kind == provider.ChunkToolCall {
			t.Fatal("an invalid tool call was emitted")
		}
	}
	if !strings.Contains(err.Error(), "Read") {
		t.Errorf("the error should name the tool: %v", err)
	}
}

func TestRequestValidationFailsBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })

	cases := []struct {
		name string
		req  provider.Request
		want string
	}{
		{"no messages", provider.Request{Model: "m"}, "no messages"},
		{"tool without schema", provider.Request{
			Model:    "m",
			Messages: simpleRequest().Messages,
			Tools:    []provider.ToolSchema{{Name: "Read"}},
		}, "input schema"},
		{"tool without a name", provider.Request{
			Model:    "m",
			Messages: simpleRequest().Messages,
			Tools:    []provider.ToolSchema{{InputSchema: json.RawMessage(`{"type":"object"}`)}},
		}, "no name"},
		{"thinking budget exceeds max tokens", provider.Request{
			Model:     "m",
			Messages:  simpleRequest().Messages,
			MaxTokens: 100,
			Thinking:  &provider.Thinking{BudgetTokens: 200},
		}, "thinking budget"},
		{"unknown block kind", provider.Request{
			Model: "m",
			Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{
				{Kind: provider.BlockKind("video"), Text: "x"},
			}}},
		}, "unsupported block kind"},
		{"invalid tool_use JSON", provider.Request{
			Model: "m",
			Messages: []provider.Message{{Role: provider.RoleAssistant, Blocks: []provider.Block{
				{Kind: provider.BlockToolUse, ToolUseID: "t1", ToolName: "Read", Input: json.RawMessage(`{broken`)},
			}}},
		}, "invalid JSON input"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collect(c.Stream(context.Background(), tc.req))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests reached the server; validation must happen first", hits.Load())
	}
}

// TestRequestWithNoModelUsesTheConfiguredOne pins the fallback: a Request that
// omits the model gets the client's, because the provider instance was
// configured with one. Failing here instead would force every caller to repeat
// a value the client already holds.
func TestRequestWithNoModelUsesTheConfiguredOne(t *testing.T) {
	var body map[string]any
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, textStream("ok"))
	})

	if _, err := collect(c.Stream(context.Background(), simpleRequest())); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "test-model" {
		t.Errorf("model = %v, want the client's configured test-model", body["model"])
	}
}

// TestNoConfiguredModelIsRejectedAtConstruction pins where the failure lands.
// NewClient refuses rather than deferring, so a misconfigured provider is
// reported at wiring time instead of on the first user request.
func TestNoConfiguredModelIsRejectedAtConstruction(t *testing.T) {
	if _, err := NewClient(provider.Config{BaseURL: "http://x", AuthToken: "t"}); err == nil {
		t.Fatal("expected NewClient to reject a config with no model")
	}
}

func TestDefaultMaxTokensIsApplied(t *testing.T) {
	var body map[string]any
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, textStream("ok"))
	})
	req := simpleRequest()
	req.Model = "m"
	if _, err := collect(c.Stream(context.Background(), req)); err != nil {
		t.Fatal(err)
	}
	// max_tokens is required by the API; zero would be a 400.
	if body["max_tokens"] != float64(defaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", body["max_tokens"], defaultMaxTokens)
	}
}

func TestConsumerBreakingEarlyDoesNotBlock(t *testing.T) {
	// The reason Stream is an iterator rather than a channel: a consumer that
	// stops reading must not leave the producer blocked on a send.
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, textStream("a much longer response than anyone will read"))
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range c.Stream(context.Background(), simpleRequest()) {
			break
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("breaking out of the loop did not return; the producer is blocked")
	}
}

// --- helpers ---------------------------------------------------------------

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
