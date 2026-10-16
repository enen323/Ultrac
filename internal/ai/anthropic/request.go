package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"ultrac/internal/ai"
)

// wireRequest is the JSON body of POST /v1/messages.
//
// The field names are the API's, not ours. max_tokens and stop_reason are
// snake_case because the API says so, and they sit next to our camelCase
// internal types deliberately: renaming them to match house style would break
// every request.
type wireRequest struct {
	Model       string          `json:"model"`
	System      string          `json:"system,omitempty"`
	Messages    []wireMessage   `json:"messages"`
	Tools       []wireTool      `json:"tools,omitempty"`
	MaxTokens   int             `json:"max_tokens"`
	Stream      bool            `json:"stream"`
	Thinking    *wireThinking   `json:"thinking,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    *wireMetadata   `json:"metadata,omitempty"`
	Betas       []string        `json:"betas,omitempty"`
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
	Source    *wireImageSrc   `json:"source,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// cacheControl is emitted only when Options.SendCacheControl is set.
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireImageSrc struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type wireCacheControl struct {
	Type string `json:"type"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`

	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type wireMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// encodeRequest converts the normalized transcript into the wire body.
//
// Every role and block kind must map to something the API accepts. An
// unmapped value is an error rather than a skipped entry: silently dropping a
// block would delete a tool_result and leave the model with an unanswered
// tool call, which produces confident nonsense instead of a visible failure.
func (c *Client) encodeRequest(model ai.Model, tc ai.TranscriptContext, opts Options) ([]byte, error) {
	if model.ID == "" {
		return nil, fmt.Errorf("anthropic: request has no model")
	}
	if len(tc.Messages) == 0 {
		return nil, fmt.Errorf("anthropic: request has no messages")
	}

	// The transcript is collapsed first: Anthropic takes the system prompt
	// outside the message list and rejects system messages interleaved with
	// the conversation. Later system messages (tool-loading announcements)
	// fold into the leading one (pi: anthropic-messages.ts:1132-1135).
	messages := ai.ResolveTranscript(tc.Messages, false)
	head, hasHead := ai.GetInitialSystemMessage(messages)

	wire := wireRequest{
		Model:  model.ID,
		Stream: true,
	}
	if hasHead {
		wire.System = ai.SystemMessageText(head)
	}

	// Everything after the head is the conversation; consecutive
	// toolResult messages fold into one user message inside the encoder,
	// because the API requires tool_result blocks to immediately follow
	// their tool_use.
	conversation := messages
	if hasHead {
		conversation = messages[1:]
	}
	if len(conversation) == 0 {
		return nil, fmt.Errorf("anthropic: request has no conversation messages")
	}
	encoded, err := encodeToolResultGroups(conversation)
	if err != nil {
		return nil, err
	}
	wire.Messages = encoded

	tools := ai.GetCurrentTools(tc.Messages)
	for i, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("anthropic: tool %d has no name", i)
		}
		if len(t.Parameters) == 0 {
			// The API rejects a tool with no schema, and a tool with no
			// schema is a tool the model cannot call correctly. An empty
			// object schema ("accepts anything") is worse than useless: the
			// model will invent arguments.
			return nil, fmt.Errorf("anthropic: tool %q has no input schema", t.Name)
		}
		wt := wireTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters}
		if opts.SendCacheControl {
			wt.CacheControl = &wireCacheControl{Type: "ephemeral"}
		}
		wire.Tools = append(wire.Tools, wt)
	}

	maxTokens, thinking, err := resolveGeneration(model, tc, opts)
	if err != nil {
		return nil, err
	}
	wire.MaxTokens = maxTokens
	wire.Thinking = thinking
	// Temperature is incompatible with extended thinking on this API, so it
	// is only sent when thinking is off (pi: anthropic-messages.ts:1194-1202).
	if thinking == nil && opts.Temperature != nil {
		wire.Temperature = opts.Temperature
	}
	if choice, ok := encodeToolChoice(opts.ToolChoice); ok {
		wire.ToolChoice = choice
	}
	if opts.Metadata != nil {
		if uid, ok := opts.Metadata["userId"].(string); ok && uid != "" {
			wire.Metadata = &wireMetadata{UserID: uid}
		}
	}
	wire.Betas = c.resolveBetas(model, tc, opts, thinking != nil)

	if opts.SendCacheControl && len(wire.Messages) > 0 {
		markLastCacheable(&wire)
	}

	return json.Marshal(wire)
}

// resolveGeneration computes max_tokens and the optional thinking block.
//
// Ported from pi's buildParams + adjustMaxTokensForThinking
// (simple-options.ts:92-108): when thinking is requested, its budget is
// reserved on top of the caller's cap (or the model's), and the budget is
// clamped to leave answer room when the two would collide. The API requires
// max_tokens to exceed the budget, so an unresolved collision is an error
// rather than a 400 with a useless message.
func resolveGeneration(model ai.Model, tc ai.TranscriptContext, opts Options) (int, *wireThinking, error) {
	base := opts.MaxTokens
	if base <= 0 {
		base = model.MaxTokens
	}
	if base <= 0 {
		base = defaultMaxTokens
	}

	thinking := (*wireThinking)(nil)
	if opts.Reasoning != "" && model.Reasoning {
		budgets := defaultThinkingBudgets
		if opts.ThinkingBudgets != nil {
			budgets = mergeThinkingBudgets(budgets, *opts.ThinkingBudgets)
		}
		adjusted, budget, err := adjustMaxTokensForThinking(base, model.MaxTokens, opts.Reasoning, budgets)
		if err != nil {
			return 0, nil, err
		}
		base = adjusted
		thinking = &wireThinking{Type: "enabled", BudgetTokens: budget}
	}

	return ai.ClampMaxTokensToContext(model, tc.Messages, base), thinking, nil
}

var defaultThinkingBudgets = ai.ThinkingBudgets{
	Minimal: intPtr(1024),
	Low:     intPtr(2048),
	Medium:  intPtr(8192),
	High:    intPtr(16384),
}

func mergeThinkingBudgets(base, override ai.ThinkingBudgets) ai.ThinkingBudgets {
	if override.Minimal != nil {
		base.Minimal = override.Minimal
	}
	if override.Low != nil {
		base.Low = override.Low
	}
	if override.Medium != nil {
		base.Medium = override.Medium
	}
	if override.High != nil {
		base.High = override.High
	}
	return base
}

func thinkingBudgetForLevel(level ai.ThinkingLevel, budgets ai.ThinkingBudgets) int {
	switch clampReasoning(level) {
	case ai.ThinkMinimal:
		return derefOr(budgets.Minimal, 1024)
	case ai.ThinkLow:
		return derefOr(budgets.Low, 2048)
	case ai.ThinkMedium:
		return derefOr(budgets.Medium, 8192)
	default:
		return derefOr(budgets.High, 16384)
	}
}

// clampReasoning maps the adaptive-thinking efforts onto the budget ladder.
func clampReasoning(level ai.ThinkingLevel) ai.ThinkingLevel {
	switch level {
	case ai.ThinkXHigh, ai.ThinkMax:
		return ai.ThinkHigh
	default:
		return level
	}
}

// adjustMaxTokensForThinking reserves the thinking budget above the cap.
//
// pi: simple-options.ts:92-108. When no explicit cap exists the model's own
// ceiling applies; when one exists the budget stacks on top of it (a caller
// asking for 4096 with thinking gets 4096 + budget, not 4096 including it).
func adjustMaxTokensForThinking(base, modelMax int, level ai.ThinkingLevel, budgets ai.ThinkingBudgets) (maxTokens, budget int, err error) {
	budget = thinkingBudgetForLevel(level, budgets)
	maxTokens = base
	if base <= 0 {
		maxTokens = modelMax
	} else if modelMax > 0 {
		maxTokens = min(base+budget, modelMax)
	}
	if maxTokens <= 0 {
		return 0, 0, fmt.Errorf("anthropic: no max_tokens ceiling available for thinking")
	}
	if maxTokens <= budget {
		// Leave room for the answer (pi: MIN_ANSWER_TOKENS = 1024).
		budget = max(0, maxTokens-1024)
	}
	if budget <= 0 {
		return 0, 0, fmt.Errorf("anthropic: max_tokens (%d) leaves no room for a thinking budget", maxTokens)
	}
	return maxTokens, budget, nil
}

func encodeToolChoice(choice ai.ToolChoice) (json.RawMessage, bool) {
	switch choice {
	case ai.ToolChoiceAuto:
		return json.RawMessage(`{"type":"auto"}`), true
	case ai.ToolChoiceNone:
		return json.RawMessage(`{"type":"none"}`), true
	default:
		return nil, false
	}
}

// resolveBetas derives the anthropic-beta list: derived features plus
// Options.Betas, unless a header-owned anthropic-beta replaces them
// (pi: getBetaFeatures, anthropic-messages.ts:1080-1122).
func (c *Client) resolveBetas(model ai.Model, tc ai.TranscriptContext, opts Options, thinking bool) []string {
	for name, value := range opts.Headers {
		if strings.EqualFold(name, "anthropic-beta") {
			var feats []string
			for _, f := range strings.Split(value, ",") {
				if f = strings.TrimSpace(f); f != "" {
					feats = append(feats, f)
				}
			}
			return feats
		}
	}
	feats := append([]string(nil), c.betas...)
	if thinking {
		feats = append(feats, InterleavedThinkingBeta)
	}
	return dedupeStrings(feats)
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// markLastCacheable puts a cache_control marker on the last block of the
// last message, so the conversation prefix can be cached across turns
// (pi: anthropic-messages.ts:1483-1509 — marker on the last user/system
// message; ours also covers a trailing assistant message, which Anthropic
// accepts and which is the common shape after a tool round).
func markLastCacheable(wire *wireRequest) {
	if len(wire.Messages) == 0 {
		return
	}
	last := &wire.Messages[len(wire.Messages)-1]
	if len(last.Content) == 0 {
		return
	}
	last.Content[len(last.Content)-1].CacheControl = &wireCacheControl{Type: "ephemeral"}
}

func encodeMessage(msg ai.Message) (wireMessage, error) {
	role := string(msg.Role)
	if role != string(ai.RoleUser) && role != string(ai.RoleAssistant) {
		return wireMessage{}, fmt.Errorf("unknown role %q", msg.Role)
	}

	out := wireMessage{Role: role}
	for i, b := range msg.Content {
		block, err := encodeBlock(b)
		if err != nil {
			return wireMessage{}, fmt.Errorf("block %d: %w", i, err)
		}
		if block.Type == "" {
			continue // encodeBlock declined (empty text)
		}
		out.Content = append(out.Content, block)
	}
	if len(out.Content) == 0 {
		// A message whose only block was empty text would otherwise become
		// a message with an empty content array, which the API rejects with
		// a message that says nothing about the empty string that caused it.
		return wireMessage{}, fmt.Errorf("message has no content blocks")
	}
	return out, nil
}

func encodeBlock(b ai.ContentBlock) (wireBlock, error) {
	switch b.Type {
	case ai.BlockText:
		if strings.TrimSpace(b.Text) == "" {
			// An empty text block is rejected by the API. Dropping it is
			// correct: an assistant turn that produced no text carries no
			// information, and an error here would fail a whole turn over
			// one empty string.
			return wireBlock{}, nil
		}
		return wireBlock{Type: "text", Text: b.Text}, nil

	case ai.BlockImage:
		if b.Data == "" {
			return wireBlock{}, fmt.Errorf("image block has no data")
		}
		mime := b.MimeType
		if mime == "" {
			mime = "image/png"
		}
		return wireBlock{
			Type:   "image",
			Source: &wireImageSrc{Type: "base64", MediaType: mime, Data: b.Data},
		}, nil

	case ai.BlockThinking:
		// The signature is what makes a thinking block replayable: without
		// it Anthropic rejects the whole request. A block that lost its
		// signature (aborted stream, hand-built transcript) degrades to
		// plain text instead of poisoning the turn — pi does the same
		// (anthropic-messages.ts:1419-1431).
		//
		// The mirror case — a signature over EMPTY reasoning — is an
		// artifact, not content: it can only come from a stream that was
		// cut before the first thinking delta (the P5 live run hit exactly
		// that when DeepSeek's endpoint put the whole thinking text in
		// content_block_start). Sending it makes Anthropic-compatible
		// endpoints reject the request ("missing field `thinking`"),
		// and dropping it loses nothing, so it is skipped whatever the
		// signature says.
		if b.Redacted {
			if b.ThinkingSignature == "" {
				return wireBlock{}, fmt.Errorf("redacted thinking block has no payload")
			}
			return wireBlock{Type: "redacted_thinking", Data: b.ThinkingSignature}, nil
		}
		if strings.TrimSpace(b.Thinking) == "" {
			return wireBlock{}, nil
		}
		if b.ThinkingSignature == "" {
			return wireBlock{Type: "text", Text: b.Thinking}, nil
		}
		return wireBlock{Type: "thinking", Thinking: b.Thinking, Signature: b.ThinkingSignature}, nil

	case ai.BlockToolCall:
		input := b.Arguments
		if len(input) == 0 {
			input = map[string]any{}
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return wireBlock{}, fmt.Errorf("tool_use %s has unmarshalable input", b.ID)
		}
		return wireBlock{
			Type:      "tool_use",
			ID:        normalizeToolCallID(b.ID),
			Name:      b.Name,
			Input:     raw,
			ToolUseID: normalizeToolCallID(b.ID),
		}, nil

	default:
		return wireBlock{}, fmt.Errorf("unsupported block kind %q", b.Type)
	}
}

// toolResultContent renders a toolResult's blocks the way the API accepts:
// a bare string when it is all text, a block array when images are involved
// (pi: convertContentBlocks, anthropic-messages.ts:137-184).
func toolResultContent(blocks []ai.ContentBlock) any {
	hasImage := false
	for _, b := range blocks {
		if b.Type == ai.BlockImage {
			hasImage = true
			break
		}
	}
	if !hasImage {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == ai.BlockText {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	out := make([]wireBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ai.BlockText:
			out = append(out, wireBlock{Type: "text", Text: b.Text})
		case ai.BlockImage:
			mime := b.MimeType
			if mime == "" {
				mime = "image/png"
			}
			out = append(out, wireBlock{
				Type:   "image",
				Source: &wireImageSrc{Type: "base64", MediaType: mime, Data: b.Data},
			})
		}
	}
	return out
}

// encodeToolResultGroups folds consecutive toolResult messages into one user
// message. The API requires tool_result blocks to immediately follow their
// tool_use, and several results answering one assistant turn arrive as
// several messages; sending them separately would interleave roles the API
// rejects.
func encodeToolResultGroups(messages []ai.Message) ([]wireMessage, error) {
	var out []wireMessage
	for i := 0; i < len(messages); {
		if messages[i].Role != ai.RoleToolResult {
			wm, err := encodeMessage(messages[i])
			if err != nil {
				return nil, fmt.Errorf("anthropic: message %d: %w", i, err)
			}
			out = append(out, wm)
			i++
			continue
		}
		group := wireMessage{Role: "user"}
		for i < len(messages) && messages[i].Role == ai.RoleToolResult {
			m := messages[i]
			if m.ToolCallID == "" {
				return nil, fmt.Errorf("anthropic: toolResult message %d has no toolCallId", i)
			}
			group.Content = append(group.Content, wireBlock{
				Type:      "tool_result",
				ToolUseID: normalizeToolCallID(m.ToolCallID),
				Content:   toolResultContent(m.Content),
				IsError:   m.IsError,
			})
			i++
		}
		out = append(out, group)
	}
	return out, nil
}

// normalizeToolCallID maps ids onto Anthropic's accepted pattern
// (pi: anthropic-messages.ts:1289-1292): [a-zA-Z0-9_-], at most 64 chars.
// Providers invent their own id shapes; a rejected id fails the request with
// a message that does not mention the id.
func normalizeToolCallID(id string) string {
	if id == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func intPtr(v int) *int { return &v }
func derefOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}
