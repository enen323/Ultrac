package anthropic

import (
	"encoding/json"
	"fmt"

	"ultrac/internal/contract/provider"
)

// wireRequest is the JSON body of POST /v1/messages.
//
// The field names are the API's, not ours. max_tokens and stop_reason are
// snake_case because the API says so, and they sit next to our camelCase
// internal types deliberately: renaming them to match house style would break
// every request.
type wireRequest struct {
	Model     string        `json:"model"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
	Thinking  *wireThinking `json:"thinking,omitempty"`
	Metadata  *wireMetadata `json:"metadata,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type wireMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// encodeRequest converts the internal Request into the wire body.
//
// Every role and block kind must map to something the API accepts. An
// unmapped value is an error rather than a skipped entry: silently dropping a
// block would delete a tool_result and leave the model with an unanswered tool
// call, which produces confident nonsense instead of a visible failure.
func (c *Client) encodeRequest(req provider.Request) ([]byte, error) {
	if req.Model == "" {
		req.Model = c.defaultModel(req)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("anthropic: no model in request")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("anthropic: request has no messages")
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	wire := wireRequest{
		Model:     req.Model,
		System:    req.System,
		MaxTokens: maxTokens,
		Stream:    true,
	}

	for i, msg := range req.Messages {
		wm, err := encodeMessage(msg)
		if err != nil {
			return nil, fmt.Errorf("anthropic: message %d: %w", i, err)
		}
		wire.Messages = append(wire.Messages, wm)
	}

	for i, t := range req.Tools {
		if t.Name == "" {
			return nil, fmt.Errorf("anthropic: tool %d has no name", i)
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			// The API rejects a tool with no schema, and a tool with no
			// schema is a tool the model cannot call correctly. An empty
			// object schema ("accepts anything") is worse than useless: the
			// model will invent arguments.
			return nil, fmt.Errorf("anthropic: tool %q has no input schema", t.Name)
		}
		wire.Tools = append(wire.Tools, wireTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}

	if req.Thinking != nil {
		if req.Thinking.BudgetTokens <= 0 {
			return nil, fmt.Errorf("anthropic: thinking budget must be positive")
		}
		// The API requires max_tokens to exceed the thinking budget. Without
		// this check the request fails with a 400 that says only
		// "max_tokens must be greater than thinking.budget_tokens", which is
		// accurate and useless.
		if maxTokens <= req.Thinking.BudgetTokens {
			return nil, fmt.Errorf(
				"anthropic: max_tokens (%d) must exceed the thinking budget (%d)",
				maxTokens, req.Thinking.BudgetTokens)
		}
		wire.Thinking = &wireThinking{
			Type:         "enabled",
			BudgetTokens: req.Thinking.BudgetTokens,
		}
	}

	return json.Marshal(wire)
}

// defaultModel returns the client's configured model, when the Request omits
// one. The model lives on the Client because a resolved provider instance is
// configured with a model; a Request that names a different one overrides it.
func (c *Client) defaultModel(req provider.Request) string {
	if req.Model != "" {
		return req.Model
	}
	return c.model
}

func encodeMessage(msg provider.Message) (wireMessage, error) {
	role := string(msg.Role)
	if role != string(provider.RoleUser) && role != string(provider.RoleAssistant) {
		return wireMessage{}, fmt.Errorf("unknown role %q", msg.Role)
	}

	out := wireMessage{Role: role}
	for i, b := range msg.Blocks {
		block, err := encodeBlock(b)
		if err != nil {
			return wireMessage{}, fmt.Errorf("block %d: %w", i, err)
		}
		out.Content = append(out.Content, block)
	}
	// Empty-text blocks are dropped before the check below, not after: a
	// message whose only block was an empty assistant text would otherwise
	// become `{"type":""}`, which the API rejects with a message that says
	// nothing about the empty string that caused it.
	out = stripEmptyBlocks(out)
	if len(out.Content) == 0 {
		return wireMessage{}, fmt.Errorf("message has no content blocks")
	}
	return out, nil
}

func encodeBlock(b provider.Block) (wireBlock, error) {
	switch b.Kind {
	case provider.BlockText:
		if b.Text == "" {
			// An empty text block is rejected by the API. Dropping it is
			// correct: an assistant turn that produced no text carries no
			// information, and an error here would fail a whole turn over
			// one empty string.
			return wireBlock{}, nil
		}
		return wireBlock{Type: "text", Text: b.Text}, nil

	case provider.BlockThinking:
		return wireBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature}, nil

	case provider.BlockToolUse:
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		if !json.Valid(input) {
			return wireBlock{}, fmt.Errorf("tool_use %s has invalid JSON input", b.ToolUseID)
		}
		return wireBlock{
			Type:  "tool_use",
			ID:    b.ToolUseID,
			Name:  b.ToolName,
			Input: input,
		}, nil

	case provider.BlockToolResult:
		return wireBlock{
			Type:      "tool_result",
			ToolUseID: b.ToolUseID,
			Content:   b.Text,
			IsError:   b.IsError,
		}, nil

	default:
		return wireBlock{}, fmt.Errorf("unsupported block kind %q", b.Kind)
	}
}

// stripEmptyBlocks drops the blocks encodeBlock declined to produce, so a
// message whose only block was empty text does not become a message with an
// empty content array.
func stripEmptyBlocks(msg wireMessage) wireMessage {
	kept := msg.Content[:0]
	for _, b := range msg.Content {
		if b.Type == "" {
			continue
		}
		kept = append(kept, b)
	}
	msg.Content = kept
	return msg
}
