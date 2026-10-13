package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"ultrac/internal/ai"
)

// wireRequest is the JSON body of POST /chat/completions.
//
// max_tokens and max_completion_tokens are both carried as pointers because
// the dialect disagrees on which one an endpoint accepts; exactly one is
// ever set (see compat.maxTokensField).
type wireRequest struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	MaxTokens           *int       `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int       `json:"max_completion_tokens,omitempty"`
	Temperature         *float64   `json:"temperature,omitempty"`
	Tools               []wireTool `json:"tools,omitempty"`
	ToolChoice          any        `json:"tool_choice,omitempty"`
	ReasoningEffort     string     `json:"reasoning_effort,omitempty"`
	Store               *bool      `json:"store,omitempty"`
}

type wireMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"` // string | []contentPart
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	Name             string         `json:"name,omitempty"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string          `json:"type"`
	Function wireFunctionDef `json:"function"`
}

type wireFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// compat is the compressed version of pi's ResolvedOpenAICompletionsCompat
// (openai-completions.ts:170-185, detectCompat at 1593-1690): only the fields
// that change request encoding or stream termination for the endpoints this
// project targets. model.compat (the free-form object) can override the
// detected values through resolveCompat.
type compat struct {
	// maxTokensField selects the token-cap field name.
	maxTokensField string
	// supportsReasoningEffort enables the reasoning_effort parameter.
	supportsReasoningEffort bool
	// requiresReasoningContentOnAssistantMessages replays thinking as
	// reasoning_content on assistant messages (DeepSeek).
	requiresReasoningContentOnAssistantMessages bool
	// supportsDeveloperRole prefers the developer role for the instruction
	// message on reasoning models.
	supportsDeveloperRole bool
	// supportsStore enables the store parameter.
	supportsStore bool
	// supportsUsageInStreaming requests the usage-only final chunk.
	supportsUsageInStreaming bool
	// supportsFinishReason expects finish_reason; without it the stop
	// reason is inferred from the content.
	supportsFinishReason bool
}

// detectCompat derives the defaults from provider id and base URL, following
// pi's detectCompat (openai-completions.ts:1593-1690) for the cases this
// project targets. Unknown endpoints get the conservative OpenAI-shaped
// defaults, which the majority of compatible servers accept.
func detectCompat(model ai.Model) compat {
	provider := string(model.Provider)
	baseURL := strings.ToLower(model.BaseURL)

	isDeepSeek := provider == string(ai.ProviderDeepseek) || strings.Contains(baseURL, "deepseek.com")
	isOpenRouter := provider == string(ai.ProviderOpenRouter) || strings.Contains(baseURL, "openrouter.ai")
	isMoonshot := provider == string(ai.ProviderMoonshotAI) || provider == string(ai.ProviderMoonshotAICN) || strings.Contains(baseURL, "api.moonshot.")
	isZai := provider == string(ai.ProviderZai) || provider == string(ai.ProviderZaiCodingCN) ||
		strings.Contains(baseURL, "api.z.ai") || strings.Contains(baseURL, "open.bigmodel.cn")
	isXAI := provider == string(ai.ProviderXAI) || strings.Contains(baseURL, "api.x.ai")
	isTogether := provider == string(ai.ProviderTogether) || strings.Contains(baseURL, "api.together.")
	isNonStandard := isDeepSeek || isOpenRouter || isMoonshot || isZai || isXAI || isTogether

	c := compat{
		maxTokensField:           "max_completion_tokens",
		supportsReasoningEffort:  true,
		supportsDeveloperRole:    !isNonStandard && !isOpenRouter,
		supportsStore:            !isNonStandard,
		supportsUsageInStreaming: true,
		supportsFinishReason:     true,
	}
	if isDeepSeek || isMoonshot || isZai || isTogether || isXAI {
		// These endpoints only accept the legacy field name.
		c.maxTokensField = "max_tokens"
	}
	if isDeepSeek {
		// DeepSeek streams reasoning as reasoning_content and requires it
		// replayed on assistant messages (openai-completions.ts:1655-1657).
		c.requiresReasoningContentOnAssistantMessages = true
	}
	if isXAI || isZai || isMoonshot || isTogether {
		c.supportsReasoningEffort = false
	}
	return c
}

// resolveCompat applies model.compat overrides on top of the detected
// defaults. The override object is free-form (pi: OpenAICompletionsCompat);
// unknown keys are ignored rather than rejected, because the catalog is
// hand-maintained and a typo must not disable the endpoint.
func resolveCompat(model ai.Model) compat {
	c := detectCompat(model)
	obj, ok := model.Compat.(map[string]any)
	if !ok {
		return c
	}
	if v, ok := obj["maxTokensField"].(string); ok && v != "" {
		c.maxTokensField = v
	}
	if v, ok := obj["supportsReasoningEffort"].(bool); ok {
		c.supportsReasoningEffort = v
	}
	if v, ok := obj["requiresReasoningContentOnAssistantMessages"].(bool); ok {
		c.requiresReasoningContentOnAssistantMessages = v
	}
	if v, ok := obj["supportsDeveloperRole"].(bool); ok {
		c.supportsDeveloperRole = v
	}
	if v, ok := obj["supportsStore"].(bool); ok {
		c.supportsStore = v
	}
	if v, ok := obj["supportsUsageInStreaming"].(bool); ok {
		c.supportsUsageInStreaming = v
	}
	if v, ok := obj["supportsFinishReason"].(bool); ok {
		c.supportsFinishReason = v
	}
	return c
}

// encodeRequest converts the normalized transcript into the wire body.
func (c *Client) encodeRequest(model ai.Model, tc ai.TranscriptContext, opts Options) ([]byte, error) {
	if model.ID == "" {
		return nil, fmt.Errorf("openai: request has no model")
	}
	if len(tc.Messages) == 0 {
		return nil, fmt.Errorf("openai: request has no messages")
	}

	comp := resolveCompat(model)
	messages, err := encodeMessages(model, tc, comp)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("openai: request has no conversation messages")
	}

	wire := wireRequest{
		Model:    model.ID,
		Messages: messages,
		Stream:   true,
	}
	if comp.supportsUsageInStreaming {
		// The usage-only final chunk is the only place this dialect reports
		// token counts; without it the turn has no accounting.
		wire.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	if comp.supportsStore {
		store := false
		wire.Store = &store
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = model.MaxTokens
	}
	if maxTokens > 0 {
		clamped := ai.ClampMaxTokensToContext(model, tc.Messages, maxTokens)
		if comp.maxTokensField == "max_tokens" {
			wire.MaxTokens = &clamped
		} else {
			wire.MaxCompletionTokens = &clamped
		}
	}
	if opts.Temperature != nil {
		wire.Temperature = opts.Temperature
	}

	tools := ai.GetCurrentTools(tc.Messages)
	for i, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("openai: tool %d has no name", i)
		}
		if len(t.Parameters) == 0 {
			return nil, fmt.Errorf("openai: tool %q has no parameters", t.Name)
		}
		wire.Tools = append(wire.Tools, wireTool{
			Type: "function",
			Function: wireFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	if len(wire.Tools) == 0 && hasToolHistory(tc.Messages) {
		// Some proxies reject a conversation containing tool_calls when the
		// request carries no tools param; an empty list satisfies them
		// (pi: openai-completions.ts:860-863).
		wire.Tools = []wireTool{}
	}

	if opts.ToolChoice != "" {
		switch opts.ToolChoice {
		case ai.ToolChoiceAuto:
			wire.ToolChoice = "auto"
		case ai.ToolChoiceNone:
			wire.ToolChoice = "none"
		}
	}

	if opts.Reasoning != "" && model.Reasoning && comp.supportsReasoningEffort {
		effort := string(opts.Reasoning)
		if mapped, ok := model.ThinkingLevelMap[ai.ModelThinkingLevel(effort)]; ok && mapped != nil {
			effort = *mapped
		}
		wire.ReasoningEffort = effort
	}

	// Provider sampling params merge last, so a named field always wins.
	if len(opts.SamplingParams) > 0 {
		merged, err := mergeSamplingParams(wire, opts.SamplingParams)
		if err != nil {
			return nil, err
		}
		return json.Marshal(merged)
	}
	return json.Marshal(wire)
}

// encodeMessages converts the transcript. The instruction message is the
// collapsed system prompt (the dialect takes it as a message, not a field);
// assistant tool calls and tool results use the function-calling shape.
func encodeMessages(model ai.Model, tc ai.TranscriptContext, comp compat) ([]wireMessage, error) {
	messages := ai.ResolveTranscript(tc.Messages, false)
	head, hasHead := ai.GetInitialSystemMessage(messages)

	instructionRole := "system"
	if model.Reasoning && comp.supportsDeveloperRole {
		instructionRole = "developer"
	}

	var out []wireMessage
	if hasHead {
		if text := ai.SystemMessageText(head); text != "" {
			out = append(out, wireMessage{Role: instructionRole, Content: text})
		}
	}

	for i, msg := range messages {
		if i == 0 && hasHead {
			continue
		}
		if msg.Role == ai.RoleSystem {
			return nil, fmt.Errorf("openai: unexpected system message at index %d", i)
		}
		wm, err := encodeMessage(msg, comp)
		if err != nil {
			return nil, fmt.Errorf("openai: message %d: %w", i, err)
		}
		if wm == nil {
			continue // nothing encodable in this message
		}
		out = append(out, *wm)
	}
	return out, nil
}

// encodeMessage converts one transcript message. It returns nil for messages
// with no encodable content (an assistant turn that was only empty text),
// which the dialect would otherwise reject.
func encodeMessage(msg ai.Message, comp compat) (*wireMessage, error) {
	switch msg.Role {
	case ai.RoleUser:
		parts, text := encodeUserContent(msg.Content)
		if text != "" {
			return &wireMessage{Role: "user", Content: text}, nil
		}
		if len(parts) == 0 {
			return nil, nil
		}
		return &wireMessage{Role: "user", Content: parts}, nil

	case ai.RoleAssistant:
		out := &wireMessage{Role: "assistant"}
		var texts []string
		var reasoning []string
		for _, b := range msg.Content {
			switch b.Type {
			case ai.BlockText:
				if strings.TrimSpace(b.Text) != "" {
					texts = append(texts, b.Text)
				}
			case ai.BlockThinking:
				if strings.TrimSpace(b.Thinking) != "" {
					reasoning = append(reasoning, b.Thinking)
				}
			case ai.BlockToolCall:
				call := wireToolCall{ID: b.ID, Type: "function"}
				call.Function.Name = b.Name
				args, err := json.Marshal(b.Arguments)
				if err != nil {
					return nil, fmt.Errorf("tool_call %s has unmarshalable arguments", b.ID)
				}
				call.Function.Arguments = string(args)
				out.ToolCalls = append(out.ToolCalls, call)
			case ai.BlockImage:
				// Images in assistant history have no wire shape here; the
				// dialect only accepts them on user messages.
			}
		}
		if len(texts) > 0 {
			out.Content = strings.Join(texts, "")
		}
		if len(reasoning) > 0 && comp.requiresReasoningContentOnAssistantMessages {
			// DeepSeek requires the reasoning replayed, or it refuses the
			// next turn with "thinking content missing".
			out.ReasoningContent = strings.Join(reasoning, "\n\n")
		}
		if out.Content == nil && len(out.ToolCalls) == 0 && out.ReasoningContent == "" {
			return nil, nil
		}
		return out, nil

	case ai.RoleToolResult:
		if msg.ToolCallID == "" {
			return nil, fmt.Errorf("toolResult message has no toolCallId")
		}
		var texts []string
		for _, b := range msg.Content {
			if b.Type == ai.BlockText {
				texts = append(texts, b.Text)
			}
		}
		return &wireMessage{
			Role:       "tool",
			ToolCallID: msg.ToolCallID,
			Name:       msg.ToolName,
			Content:    strings.Join(texts, "\n"),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported role %q", msg.Role)
	}
}

func encodeUserContent(blocks []ai.ContentBlock) ([]contentPart, string) {
	var parts []contentPart
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case ai.BlockText:
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			texts = append(texts, b.Text)
			parts = append(parts, contentPart{Type: "text", Text: b.Text})
		case ai.BlockImage:
			mime := b.MimeType
			if mime == "" {
				mime = "image/png"
			}
			parts = append(parts, contentPart{
				Type: "image_url",
				ImageURL: &struct {
					URL string `json:"url"`
				}{URL: "data:" + mime + ";base64," + b.Data},
			})
		}
	}
	// A text-only message goes as a bare string: every endpoint accepts
	// that, and some mishandle the parts array.
	if len(parts) > 0 {
		onlyText := true
		for _, p := range parts {
			if p.Type != "text" {
				onlyText = false
			}
		}
		if onlyText {
			return nil, strings.Join(texts, "")
		}
	}
	return parts, ""
}

// hasToolHistory reports whether the transcript contains tool calls or
// results, which some proxies require the tools param for.
func hasToolHistory(messages []ai.Message) bool {
	for _, msg := range messages {
		if msg.Role == ai.RoleToolResult {
			return true
		}
		if msg.Role == ai.RoleAssistant {
			for _, b := range msg.Content {
				if b.Type == ai.BlockToolCall {
					return true
				}
			}
		}
	}
	return false
}

// mergeSamplingParams re-marshals the request with provider sampling params
// folded in, so the named fields keep precedence.
func mergeSamplingParams(wire wireRequest, params ai.SamplingParams) (map[string]any, error) {
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	for k, v := range params {
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out, nil
}
