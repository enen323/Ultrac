package mcp

// client.go — one MCP session: the initialize handshake, tools/list and
// tools/call (docs/开发计划/新功能计划/mcp-plan.md §1.3, §1.4).
//
// # The handshake
//
//	initialize { protocolVersion, capabilities, clientInfo }
//	    → records capabilities + instructions, and the version the server chose
//	notifications/initialized
//	tools/list → the tool set
//
// The version is negotiated, not asserted: a server may answer with a
// revision other than the one asked for, and that answer is what the rest of
// the session is spoken in.
//
// # instructions
//
// The free-form "what this server is for" field the server can send with its
// initialize result. It is recorded because nothing else in the protocol
// answers that question — a user looking at a configured-but-unconnected
// server has no other way to learn what they installed.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// maxListPages caps tools/list pagination. A server that always returns a
// nextCursor would otherwise make a handshake loop forever.
const maxListPages = 64

// ToolDef is one entry of a tools/list result — the model-visible declaration
// of a remote tool, before it becomes a tool name.
type ToolDef struct {
	// Name is the server-local name, what tools/call is spoken with.
	Name string `json:"name"`
	// Description is what the model is told the tool does.
	Description string `json:"description"`
	// InputSchema is the raw JSON Schema of the arguments. Raw, because a
	// remote schema is data: it is validated, cached and handed to the
	// provider verbatim, and re-encoding it would churn its bytes (and with
	// them the provider's prompt-prefix cache).
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client is one session with one server: a transport plus what the handshake
// learned.
//
// A Client is bound to one transport for its life. There is no reconnect:
// Ultrac spawns servers itself, over stdio, and a dead stdio connection means
// the child is gone — the host's answer to that is a fresh spawn, not a
// repair (see host.go).
type Client struct {
	spec Spec
	t    transport

	// hasTools is whether the server advertised the tools capability. A
	// server that did not has nothing to list, and asking anyway is a round
	// trip that can only fail.
	hasTools bool
	// instructions is the server's own description of what it is for.
	instructions string
	// negotiated is the protocol version the server answered with.
	negotiated string
}

// newClient opens a connection for spec and completes the handshake.
//
// ctx bounds the whole thing — spawn included — because that is the cost the
// caller is waiting on: a server that spawns instantly and then never answers
// is indistinguishable, from the outside, from one that took 29 seconds to
// spawn. The caller (host.go) derives it from spec.StartupTimeout.
func newClient(ctx context.Context, spec Spec) (*Client, error) {
	t, err := openStdio(spec)
	if err != nil {
		return nil, err
	}
	c := &Client{spec: spec, t: t}
	if err := c.handshake(ctx); err != nil {
		// The teardown is part of the cost too: it has its own budget, so a
		// server that is already wedged cannot hold the caller past it.
		t.close()
		return nil, err
	}
	return c, nil
}

func (c *Client) handshake(ctx context.Context) error {
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		// Ultrac advertises nothing: no roots, no sampling, no elicitation.
		// Every capability a client claims is a promise to answer requests,
		// and this build answers none.
		"capabilities": map[string]any{},
		"clientInfo":   map[string]any{"name": "ultrac", "version": "dev"},
	})
	if err != nil {
		return err
	}

	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
		Capabilities    struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		return fmt.Errorf("mcp server %q: decode initialize result: %w", c.spec.Name, err)
	}
	// A server that says nothing about its version is spoken to in the one
	// we asked for: the 2025-06-18 revision made protocolVersion optional in
	// the result, and refusing to talk would break servers that took it at
	// its word.
	c.negotiated = res.ProtocolVersion
	if c.negotiated == "" {
		c.negotiated = protocolVersion
	}
	// Presence of the key signals support, even when its value is an empty
	// object — that is the protocol's convention and servers rely on it.
	c.hasTools = len(res.Capabilities.Tools) > 0
	c.instructions = strings.TrimSpace(res.Instructions)

	if err := c.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	return nil
}

// ListTools asks the server for its tools.
//
// It is not gated on the advertised tools capability. Plenty of servers omit
// capabilities entirely and list their tools anyway, and the failure mode of
// asking is a single refused call, while the failure mode of not asking is a
// server that silently has no tools — which presents to the user as "the
// model cannot use my server" with nothing to point at.
//
// Pagination is followed, because a server with a large tool set is allowed
// to split the answer and a truncated list would look like a server with
// only some of its tools.
func (c *Client) ListTools(ctx context.Context) ([]ToolDef, error) {
	var all []ToolDef
	var cursor string
	for page := 0; page < maxListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page0 struct {
			Tools      []ToolDef `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(result, &page0); err != nil {
			return nil, fmt.Errorf("mcp server %q: decode tools/list: %w", c.spec.Name, err)
		}
		all = append(all, page0.Tools...)
		if page0.NextCursor == "" {
			return all, nil
		}
		cursor = page0.NextCursor
	}
	return all, fmt.Errorf("mcp server %q: tools/list paginated past %d pages", c.spec.Name, maxListPages)
}

// CallTool invokes one tool and flattens its result.
//
// A tool the server reports as failed comes back as a Result with IsError
// set rather than as a Go error: the model is supposed to read the server's
// own message and adjust, and an error would be rewritten by the loop into a
// generic one. (internal/tool does the same for its failures.)
func (c *Client) CallTool(ctx context.Context, rawName string, args map[string]any) (agent.Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	// The per-call timeout applies only when the caller did not already set
	// a deadline: an agent-loop cancellation and a tool timeout are two
	// different things and the shorter one must win without the longer one
	// being discarded.
	if _, ok := ctx.Deadline(); !ok && c.spec.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.spec.CallTimeout)
		defer cancel()
	}

	result, err := c.call(ctx, "tools/call", map[string]any{
		"name":      rawName,
		"arguments": args,
	})
	if err != nil {
		return agent.Result{}, err
	}
	return parseToolResult(result)
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.t.call(ctx, method, params)
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	return c.t.notify(ctx, method, params)
}

// Close tears the connection down within a budget.
func (c *Client) Close() { c.t.close() }

// ---------------------------------------------------------------------------
// Result flattening
// ---------------------------------------------------------------------------

// toolContent is one content item of a tools/call result, across the content
// types the protocol defines.
type toolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Resource *struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
		Blob     string `json:"blob"`
	} `json:"resource"`
}

// parseToolResult flattens a tools/call result into the content blocks the
// model sees.
//
// Two things differ from the way a client that only handles text would do it:
//
//   - An image becomes a real ai.ImageBlock, not a data URL pasted into a
//     text block. The provider can see it as an image because the transcript
//     carries base64 images natively (internal/ai's ContentBlock), so
//     stringifying it would throw away the one thing the model could do with
//     it.
//   - A result with no text but a structuredContent is rendered as its JSON,
//     because the protocol's own guidance is that a server would have put
//     that in a text block anyway.
//
// Content the model cannot take directly leaves a marker at its position, so
// a text-only reader still learns something was returned.
func parseToolResult(res json.RawMessage) (agent.Result, error) {
	var out struct {
		Content           []toolContent   `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return agent.Result{}, fmt.Errorf("decode tool result: %w", err)
	}

	blocks := make([]ai.ContentBlock, 0, len(out.Content))
	var text strings.Builder
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "image":
			// Images cannot be merged into the surrounding text, so the
			// accumulated text is flushed as its own block first and the
			// image goes out as an image.
			if text.Len() > 0 {
				blocks = append(blocks, ai.TextBlock(text.String()))
				text.Reset()
			}
			blocks = append(blocks, ai.ImageBlock(c.Data, orDefault(c.MimeType, "image/png")))
		case "audio":
			fmt.Fprintf(&text, "[audio: %s, not shown]", orDefault(c.MimeType, "audio"))
		case "resource_link":
			fmt.Fprintf(&text, "[resource: %s <%s>]",
				orDefault(c.Title, orDefault(c.Name, c.URI)), c.URI)
		case "resource":
			if c.Resource == nil {
				continue
			}
			switch {
			case c.Resource.Text != "":
				fmt.Fprintf(&text, "[resource <%s>]\n%s", c.Resource.URI, c.Resource.Text)
			case c.Resource.Blob != "":
				fmt.Fprintf(&text, "[resource: <%s> %s, base64, not shown]",
					c.Resource.URI, orDefault(c.Resource.MimeType, "binary"))
			default:
				fmt.Fprintf(&text, "[resource: <%s>, empty]", c.Resource.URI)
			}
		}
	}
	if text.Len() > 0 {
		blocks = append(blocks, ai.TextBlock(text.String()))
	}

	// Nothing textual and a structured payload: render the payload, which is
	// what the server meant the caller to read.
	if len(blocks) == 0 && hasJSON(out.StructuredContent) {
		blocks = append(blocks, ai.TextBlock(strings.TrimSpace(string(out.StructuredContent))))
	}
	if len(blocks) == 0 {
		blocks = append(blocks, ai.TextBlock("(no content)"))
	}

	result := agent.Result{Content: blocks, IsError: out.IsError}
	return result, nil
}

func hasJSON(raw json.RawMessage) bool {
	trimmed := []byte(strings.TrimSpace(string(raw)))
	return len(trimmed) > 0 && string(trimmed) != "null"
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// ---------------------------------------------------------------------------
// Status surface
// ---------------------------------------------------------------------------

// Instructions is the server's own description of what it is for, or "".
func (c *Client) Instructions() string { return c.instructions }

// NegotiatedVersion is the protocol revision this session is spoken in.
func (c *Client) NegotiatedVersion() string { return c.negotiated }

// HasTools reports whether the server advertised the tools capability.
func (c *Client) HasTools() bool { return c.hasTools }

// MalformedLines counts the non-JSON lines the server wrote to stdout. A
// server logging to stdout is a real and common misconfiguration, and the
// count is what makes it diagnosable rather than mysterious (R6).
func (c *Client) MalformedLines() int64 {
	type counter interface{ MalformedLines() int64 }
	if m, ok := c.t.(counter); ok {
		return m.MalformedLines()
	}
	return 0
}
