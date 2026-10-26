package mcp

// client_test.go — the session over the transport: the handshake, tools/list
// and tools/call (docs/开发计划/新功能计划/mcp-plan.md P2).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ultrac/internal/ai"
)

func TestHandshakeRecordsWhatTheServerSaid(t *testing.T) {
	c := mustClient(t, fakeSpec("hs", ""))
	defer c.Close()

	// The version is what the server answered with, not what was asked for:
	// a session is spoken in the negotiated revision.
	if got := c.NegotiatedVersion(); got != protocolVersion {
		t.Errorf("negotiated = %q, want %q", got, protocolVersion)
	}
	// instructions is the only thing in the protocol that says what a server
	// is for, and it is recorded for the status surface.
	if got := c.Instructions(); got != "a fake server that exercises the transport" {
		t.Errorf("instructions = %q", got)
	}
	if !c.HasTools() {
		t.Error("the server advertised tools and it was not recorded")
	}
}

func TestListToolsReturnsEveryDeclaredTool(t *testing.T) {
	c := mustClient(t, fakeSpec("list", ""))
	defer c.Close()

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 7 {
		t.Fatalf("tools = %d, want 7", len(tools))
	}
	byName := map[string]ToolDef{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	for _, want := range []string{"echo", "sleep", "boom", "image", "structured", "resource", "fail"} {
		tl, ok := byName[want]
		if !ok {
			t.Errorf("tool %q missing", want)
			continue
		}
		if tl.Description == "" {
			t.Errorf("tool %q has no description", want)
		}
	}
	// The schema is kept raw: re-encoding a remote schema would churn its
	// bytes, and the provider's prompt-prefix cache is keyed on them.
	if !strings.Contains(string(byName["echo"].InputSchema), `"text"`) {
		t.Errorf("echo schema = %s", byName["echo"].InputSchema)
	}
}

func TestCallToolTextResult(t *testing.T) {
	c := mustClient(t, fakeSpec("call", ""))
	defer c.Close()

	res, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Error("IsError = true for a successful call")
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %d blocks, want 1", len(res.Content))
	}
	if res.Content[0].Type != ai.BlockText || res.Content[0].Text != "echo: hello" {
		t.Errorf("content = %+v", res.Content[0])
	}
}

// An image must reach the transcript as an image, not as a data URL inside a
// text block: the provider can see a base64 image block, and stringifying it
// would throw away the one thing the model could do with the result.
func TestCallToolImageBecomesAnImageBlock(t *testing.T) {
	c := mustClient(t, fakeSpec("img", ""))
	defer c.Close()

	res, err := c.CallTool(context.Background(), "image", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 3 {
		t.Fatalf("content = %d blocks, want text+image+text", len(res.Content))
	}
	if res.Content[0].Type != ai.BlockText || res.Content[0].Text != "before" {
		t.Errorf("block 0 = %+v, want the leading text", res.Content[0])
	}
	img := res.Content[1]
	if img.Type != ai.BlockImage {
		t.Fatalf("block 1 = %+v, want an image block", img)
	}
	if img.MimeType != "image/png" {
		t.Errorf("mimeType = %q", img.MimeType)
	}
	if img.Data == "" {
		t.Error("the image carries no data")
	}
	if res.Content[2].Type != ai.BlockText || res.Content[2].Text != "after" {
		t.Errorf("block 2 = %+v, want the trailing text", res.Content[2])
	}
}

// A result with no text but a structured payload is rendered as its JSON,
// because the protocol's own guidance is that a server would have put it in a
// text block anyway.
func TestCallToolStructuredContentFallback(t *testing.T) {
	c := mustClient(t, fakeSpec("structured", ""))
	defer c.Close()

	res, err := c.CallTool(context.Background(), "structured", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %d blocks, want 1", len(res.Content))
	}
	if !strings.Contains(res.Content[0].Text, "42") {
		t.Errorf("content = %q, want the structured payload", res.Content[0].Text)
	}
}

func TestCallToolResourceBecomesText(t *testing.T) {
	c := mustClient(t, fakeSpec("res", ""))
	defer c.Close()

	res, err := c.CallTool(context.Background(), "resource", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := resultText(res)
	if !strings.Contains(got, "file:///tmp/notes.txt") || !strings.Contains(got, "the contents") {
		t.Errorf("content = %q, want the uri and the body", got)
	}
}

// A tool the server reports as failed comes back as a result the model can
// read, not as a Go error that the loop would rewrite into a generic message.
func TestCallToolIsErrorBecomesAnErrorResult(t *testing.T) {
	c := mustClient(t, fakeSpec("fail", ""))
	defer c.Close()

	res, err := c.CallTool(context.Background(), "fail", nil)
	if err != nil {
		t.Fatalf("a reported tool failure came back as a Go error: %v", err)
	}
	if !res.IsError {
		t.Error("IsError = false for a call the server reported as failed")
	}
	if got := resultText(res); got != "the tool failed on purpose" {
		t.Errorf("content = %q, want the server's own message", got)
	}
}

// A call the server refuses is a real error: there is no result to hand the
// model and no way for it to correct anything.
func TestCallToolUnknownToolIsAnError(t *testing.T) {
	c := mustClient(t, fakeSpec("unknown", ""))
	defer c.Close()

	_, err := c.CallTool(context.Background(), "nope", nil)
	if err == nil {
		t.Fatal("an unknown tool was accepted")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %q, want it to name the tool", err)
	}
}

// A per-call timeout bounds a tool that never answers, without disturbing a
// caller that already set its own deadline.
func TestCallToolTimeout(t *testing.T) {
	spec := fakeSpec("timeout", "")
	spec.CallTimeout = 250 * time.Millisecond
	c := mustClient(t, spec)
	defer c.Close()

	_, err := c.CallTool(context.Background(), "sleep", map[string]any{"ms": 5000})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}
