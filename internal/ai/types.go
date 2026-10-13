// Package ai freezes the LLM-facing contract of the pi harness in Go.
//
// This is contract C1 (message types) + C2 (streaming event protocol) of
// docs/pi-core-harness-plan.md. Every exported type carries a comment naming
// the pi TypeScript file and line range it was transcribed from — that mapping
// is the navigation aid for anyone diffing the two implementations later.
//
// # Shape decisions (Go vs TypeScript)
//
// pi models the message union with four interfaces and content blocks with
// four interfaces. Go has no declaration merging and no cheap sum types, so
// both become one struct with a discriminator field (Role / BlockType), as
// prescribed by the plan's TS→Go mapping table. Custom MarshalJSON /
// UnmarshalJSON implementations keep the wire shape byte-compatible with pi:
// only the fields belonging to the active variant are emitted, and the
// string-or-array `content` polymorphism of system/user messages is handled
// explicitly.
//
// # Layering
//
// This package imports nothing from ultrac. Providers (internal/ai/anthropic,
// internal/ai/openai) and the agent loop (internal/agent) depend on it; it
// depends on neither.
package ai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Role is a conversation participant.
//
// pi: packages/ai/src/types.ts:541-639 (SystemMessage / UserMessage /
// AssistantMessage / ToolResultMessage).
type Role string

const (
	RoleSystem     Role = "system"
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "toolResult"
)

// BlockType discriminates a ContentBlock.
//
// pi: packages/ai/src/types.ts:414-444 (TextContent / ThinkingContent /
// ImageContent / ToolCall).
type BlockType string

const (
	BlockText     BlockType = "text"
	BlockThinking BlockType = "thinking"
	BlockImage    BlockType = "image"
	BlockToolCall BlockType = "toolCall"
)

// ContentBlock is one piece of a message.
//
// The struct carries every field of all four pi block interfaces; MarshalJSON
// and UnmarshalJSON emit and accept only the fields of the active Type, so the
// JSON on the wire is exactly pi's shape:
//
//	{"type":"text","text":"hello"}
//	{"type":"thinking","thinking":"...","thinkingSignature":"...","redacted":true}
//	{"type":"image","data":"<base64>","mimeType":"image/png"}
//	{"type":"toolCall","id":"...","name":"...","arguments":{...}}
//
// pi: packages/ai/src/types.ts:408-444.
type ContentBlock struct {
	Type BlockType `json:"type"`

	// Text blocks (TextContent, types.ts:414-418). TextSignature carries
	// provider message metadata, e.g. an OpenAI responses id or the JSON of a
	// TextSignatureV1.
	Text          string `json:"text,omitempty"`
	TextSignature string `json:"textSignature,omitempty"`

	// Thinking blocks (ThinkingContent, types.ts:420-428). ThinkingSignature
	// is the opaque provider payload that MUST be replayed on the next turn;
	// dropping it makes Anthropic reject the entire request. Redacted marks
	// content suppressed by safety filters, whose encrypted payload lives in
	// ThinkingSignature.
	Thinking          string `json:"thinking,omitempty"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	Redacted          bool   `json:"redacted,omitempty"`

	// Image blocks (ImageContent, types.ts:430-434). Data is base64.
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`

	// Tool-call blocks (ToolCall, types.ts:436-444). Arguments is the parsed
	// JSON object; providers accumulate streamed fragments and only a complete
	// call leaves the adapter. ThoughtSignature is Google-specific replay
	// data; Namespace is the OpenAI Responses namespace for dynamically
	// loaded tools.
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name,omitempty"`
	Arguments        map[string]any `json:"arguments,omitempty"`
	ThoughtSignature string         `json:"thoughtSignature,omitempty"`
	Namespace        string         `json:"namespace,omitempty"`
}

// TextBlock returns a text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockText, Text: text}
}

// ThinkingBlock returns a thinking content block with its replay signature.
func ThinkingBlock(thinking, signature string, redacted bool) ContentBlock {
	return ContentBlock{Type: BlockThinking, Thinking: thinking, ThinkingSignature: signature, Redacted: redacted}
}

// ImageBlock returns a base64 image content block.
func ImageBlock(data, mimeType string) ContentBlock {
	return ContentBlock{Type: BlockImage, Data: data, MimeType: mimeType}
}

// ToolCallBlock returns a complete tool-call block.
func ToolCallBlock(id, name string, arguments map[string]any) ContentBlock {
	return ContentBlock{Type: BlockToolCall, ID: id, Name: name, Arguments: arguments}
}

// wire shapes — only the fields of the active block type appear in JSON.

type textWire struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	TextSignature string `json:"textSignature,omitempty"`
}

type thinkingWire struct {
	Type              string `json:"type"`
	Thinking          string `json:"thinking"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	Redacted          bool   `json:"redacted,omitempty"`
}

type imageWire struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

type toolCallWire struct {
	Type             string         `json:"type"`
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Arguments        map[string]any `json:"arguments"`
	ThoughtSignature string         `json:"thoughtSignature,omitempty"`
	Namespace        string         `json:"namespace,omitempty"`
}

// MarshalJSON emits only the fields belonging to the block's Type, matching
// pi's per-interface JSON shape.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case BlockText:
		return json.Marshal(textWire{Type: string(BlockText), Text: b.Text, TextSignature: b.TextSignature})
	case BlockThinking:
		return json.Marshal(thinkingWire{
			Type: string(BlockThinking), Thinking: b.Thinking,
			ThinkingSignature: b.ThinkingSignature, Redacted: b.Redacted,
		})
	case BlockImage:
		return json.Marshal(imageWire{Type: string(BlockImage), Data: b.Data, MimeType: b.MimeType})
	case BlockToolCall:
		args := b.Arguments
		if args == nil {
			args = map[string]any{}
		}
		return json.Marshal(toolCallWire{
			Type: string(BlockImage), ID: b.ID, Name: b.Name, Arguments: args,
			ThoughtSignature: b.ThoughtSignature, Namespace: b.Namespace,
		})
	default:
		return nil, fmt.Errorf("ai: unknown content block type %q", b.Type)
	}
}

// UnmarshalJSON accepts pi's per-interface JSON shape into the shared struct.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type BlockType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	switch probe.Type {
	case BlockText:
		var w textWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		*b = ContentBlock{Type: BlockText, Text: w.Text, TextSignature: w.TextSignature}
	case BlockThinking:
		var w thinkingWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		*b = ContentBlock{Type: BlockThinking, Thinking: w.Thinking, ThinkingSignature: w.ThinkingSignature, Redacted: w.Redacted}
	case BlockImage:
		var w imageWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		*b = ContentBlock{Type: BlockImage, Data: w.Data, MimeType: w.MimeType}
	case BlockToolCall:
		var w toolCallWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		*b = ContentBlock{Type: BlockToolCall, ID: w.ID, Name: w.Name, Arguments: w.Arguments, ThoughtSignature: w.ThoughtSignature, Namespace: w.Namespace}
	default:
		return fmt.Errorf("ai: unknown content block type %q", probe.Type)
	}
	return nil
}

// Usage is token accounting for one request.
//
// pi: packages/ai/src/types.ts:446-467. Reasoning and CacheWrite1h are
// pointers because providers that do not report them leave them absent, which
// is distinct from reporting zero.
type Usage struct {
	Input        int  `json:"input"`
	Output       int  `json:"output"`
	CacheRead    int  `json:"cacheRead"`
	CacheWrite   int  `json:"cacheWrite"`
	CacheWrite1h *int `json:"cacheWrite1h,omitempty"`
	Reasoning    *int `json:"reasoning,omitempty"`
	TotalTokens  int  `json:"totalTokens"`
	Cost         Cost `json:"cost"`
}

// Cost is the dollar accounting of one request at catalog prices.
//
// pi: packages/ai/src/types.ts:460-466 (the inline `cost` object) with rates
// from providers/model-schema.ts:25-46.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// StopReason explains why generation ended.
//
// pi: packages/ai/src/types.ts:469. The plan's draft struct list omitted
// "deferred"; the pi source has it, and the `done` event's reason enum
// (types.ts:812-817) includes it, so it is frozen here.
type StopReason string

const (
	StopPending  StopReason = "pending"
	StopStop     StopReason = "stop"
	StopLength   StopReason = "length"
	StopToolUse  StopReason = "toolUse"
	StopError    StopReason = "error"
	StopAborted  StopReason = "aborted"
	StopDeferred StopReason = "deferred"
)

// DeferredHandle is a provider handle for an asynchronously continued request.
//
// pi: packages/ai/src/types.ts:519-529.
type DeferredHandle struct {
	Provider    string `json:"provider"`
	ModelID     string `json:"modelId"`
	API         string `json:"api"`
	ID          string `json:"id"`
	ExpiresAt   *int64 `json:"expiresAt,omitempty"`
	PollAfterMs *int64 `json:"pollAfterMs,omitempty"`
	Data        any    `json:"data,omitempty"`
}

// DiagnosticErrorInfo is the error payload of a diagnostic.
//
// pi: packages/ai/src/utils/diagnostics.ts:3-8.
type DiagnosticErrorInfo struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	Code    any    `json:"code,omitempty"`
}

// Diagnostic is a redacted provider/runtime diagnostic attached to an
// assistant message.
//
// pi: packages/ai/src/utils/diagnostics.ts:10-15.
type Diagnostic struct {
	Type      string               `json:"type"`
	Timestamp int64                `json:"timestamp"`
	Error     *DiagnosticErrorInfo `json:"error,omitempty"`
	Details   map[string]any       `json:"details,omitempty"`
}

// NestedToolCallRecord records a call another tool made while it ran.
//
// pi: packages/ai/src/types.ts:599-611.
type NestedToolCallRecord struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Arguments      map[string]any `json:"arguments,omitempty"`
	ArgumentsBytes *int           `json:"argumentsBytes,omitempty"`
	Status         string         `json:"status"` // "ok" | "error" | "unfinished"
	DurationMs     *int64         `json:"durationMs,omitempty"`
	Error          string         `json:"error,omitempty"`
}

// NestedToolCalls is the bounded record of a tool's nested calls.
//
// pi: packages/ai/src/types.ts:613-618.
type NestedToolCalls struct {
	Calls    []NestedToolCallRecord `json:"calls"`
	Complete bool                   `json:"complete"`
}

// Message is one turn of the conversation: the Go union of pi's
// SystemMessage / UserMessage / AssistantMessage / ToolResultMessage.
//
// Fields are grouped by owning role; MarshalJSON emits only the fields of the
// active Role. Sections maps a section name to its replacement text; a nil
// *string value deletes the section (pi: types.ts:551).
//
// pi: packages/ai/src/types.ts:541-639.
type Message struct {
	Role Role `json:"role"`

	// Content blocks for every role. System and user messages may carry a
	// bare string on the wire; see MarshalJSON.
	Content []ContentBlock `json:"content,omitempty"`

	// System-only patch fields (types.ts:551-555).
	Sections     map[string]*string `json:"sections,omitempty"`
	ToolsAdded   []Tool             `json:"toolsAdded,omitempty"`
	ToolsRemoved []ToolReference    `json:"toolsRemoved,omitempty"`

	// ToolResult-only fields (types.ts:620-637).
	ToolCallID  string           `json:"toolCallId,omitempty"`
	ToolName    string           `json:"toolName,omitempty"`
	Details     any              `json:"details,omitempty"`
	NestedCalls *NestedToolCalls `json:"nestedCalls,omitempty"`
	IsError     bool             `json:"isError,omitempty"`

	// Assistant-only fields (types.ts:565-596).
	API                   string          `json:"api,omitempty"`
	Provider              string          `json:"provider,omitempty"`
	Model                 string          `json:"model,omitempty"`
	ResponseModel         string          `json:"responseModel,omitempty"`
	ResponseID            string          `json:"responseId,omitempty"`
	ProviderThinkingLevel string          `json:"providerThinkingLevel,omitempty"`
	ThinkingLevel         string          `json:"thinkingLevel,omitempty"`
	Diagnostics           []Diagnostic    `json:"diagnostics,omitempty"`
	Usage                 *Usage          `json:"usage,omitempty"`
	StopReason            StopReason      `json:"stopReason,omitempty"`
	Deferred              *DeferredHandle `json:"deferred,omitempty"`
	ErrorMessage          string          `json:"errorMessage,omitempty"`
	RawStopReason         string          `json:"rawStopReason,omitempty"`
	EndTurn               bool            `json:"endTurn,omitempty"`
	DurationMS            int64           `json:"durationMs,omitempty"`

	// TimestampMS is the Unix timestamp in milliseconds every role carries
	// (types.ts:556/562/589/633).
	TimestampMS int64 `json:"timestamp"`
}

// NewTextMessage returns a single-text-block message of the given role.
func NewTextMessage(role Role, text string) Message {
	return Message{Role: role, Content: []ContentBlock{TextBlock(text)}}
}

// NewToolResultMessage returns a toolResult message answering one call.
func NewToolResultMessage(toolCallID, toolName, text string, isError bool) Message {
	return Message{
		Role:       RoleToolResult,
		ToolCallID: toolCallID,
		ToolName:   toolName,
		Content:    []ContentBlock{TextBlock(text)},
		IsError:    isError,
	}
}

// wire shapes — one per role, so only the active role's keys are emitted.

type systemWire struct {
	Role         string             `json:"role"`
	Content      any                `json:"content"` // string | []ContentBlock
	Sections     map[string]*string `json:"sections,omitempty"`
	ToolsAdded   []Tool             `json:"toolsAdded,omitempty"`
	ToolsRemoved []ToolReference    `json:"toolsRemoved,omitempty"`
	Timestamp    int64              `json:"timestamp"`
}

type userWire struct {
	Role      string `json:"role"`
	Content   any    `json:"content"` // string | []ContentBlock
	Timestamp int64  `json:"timestamp"`
}

type assistantWire struct {
	Role                  string          `json:"role"`
	Content               []ContentBlock  `json:"content"`
	API                   string          `json:"api"`
	Provider              string          `json:"provider"`
	Model                 string          `json:"model"`
	ResponseModel         string          `json:"responseModel,omitempty"`
	ResponseID            string          `json:"responseId,omitempty"`
	ProviderThinkingLevel string          `json:"providerThinkingLevel,omitempty"`
	ThinkingLevel         string          `json:"thinkingLevel,omitempty"`
	Diagnostics           []Diagnostic    `json:"diagnostics,omitempty"`
	Usage                 *Usage          `json:"usage"`
	StopReason            StopReason      `json:"stopReason"`
	Deferred              *DeferredHandle `json:"deferred,omitempty"`
	ErrorMessage          string          `json:"errorMessage,omitempty"`
	RawStopReason         string          `json:"rawStopReason,omitempty"`
	EndTurn               bool            `json:"endTurn,omitempty"`
	Timestamp             int64           `json:"timestamp"`
	DurationMS            int64           `json:"durationMs,omitempty"`
}

type toolResultWire struct {
	Role        string           `json:"role"`
	ToolCallID  string           `json:"toolCallId"`
	ToolName    string           `json:"toolName"`
	Content     []ContentBlock   `json:"content"`
	Details     any              `json:"details,omitempty"`
	NestedCalls *NestedToolCalls `json:"nestedCalls,omitempty"`
	Usage       *Usage           `json:"usage,omitempty"`
	IsError     bool             `json:"isError"`
	Timestamp   int64            `json:"timestamp"`
	DurationMS  int64            `json:"durationMs,omitempty"`
}

// contentValue renders Content the way pi does for roles that allow the
// string-or-array polymorphism: a lone plain text block becomes a bare
// string, anything else stays an array. A text block carrying a signature
// cannot collapse, because the signature would be lost.
func contentValue(blocks []ContentBlock) any {
	if len(blocks) == 1 && blocks[0].Type == BlockText && blocks[0].TextSignature == "" {
		return blocks[0].Text
	}
	return blocks
}

// MarshalJSON emits only the active role's fields, with the string-or-array
// content polymorphism for system and user messages (pi: types.ts:544/561).
func (m Message) MarshalJSON() ([]byte, error) {
	switch m.Role {
	case RoleSystem:
		return json.Marshal(systemWire{
			Role: "system", Content: contentValue(m.Content),
			Sections: m.Sections, ToolsAdded: m.ToolsAdded, ToolsRemoved: m.ToolsRemoved,
			Timestamp: m.TimestampMS,
		})
	case RoleUser:
		return json.Marshal(userWire{Role: "user", Content: contentValue(m.Content), Timestamp: m.TimestampMS})
	case RoleAssistant:
		usage := m.Usage
		if usage == nil {
			usage = &Usage{}
		}
		// content is a required array in pi's AssistantMessage; a nil slice
		// must marshal as [], not null.
		content := m.Content
		if content == nil {
			content = []ContentBlock{}
		}
		return json.Marshal(assistantWire{
			Role: "assistant", Content: content,
			API: m.API, Provider: m.Provider, Model: m.Model,
			ResponseModel: m.ResponseModel, ResponseID: m.ResponseID,
			ProviderThinkingLevel: m.ProviderThinkingLevel, ThinkingLevel: m.ThinkingLevel,
			Diagnostics: m.Diagnostics, Usage: usage, StopReason: m.StopReason,
			Deferred: m.Deferred, ErrorMessage: m.ErrorMessage, RawStopReason: m.RawStopReason,
			EndTurn: m.EndTurn, Timestamp: m.TimestampMS, DurationMS: m.DurationMS,
		})
	case RoleToolResult:
		content := m.Content
		if content == nil {
			content = []ContentBlock{}
		}
		return json.Marshal(toolResultWire{
			Role: "toolResult", ToolCallID: m.ToolCallID, ToolName: m.ToolName,
			Content: content, Details: m.Details, NestedCalls: m.NestedCalls,
			Usage: m.Usage, IsError: m.IsError, Timestamp: m.TimestampMS, DurationMS: m.DurationMS,
		})
	default:
		return nil, fmt.Errorf("ai: unknown message role %q", m.Role)
	}
}

// UnmarshalJSON accepts both content forms (bare string and block array) for
// system and user messages, and only the array form for assistant and
// toolResult messages, exactly as pi's types allow.
func (m *Message) UnmarshalJSON(data []byte) error {
	var probe struct {
		Role Role `json:"role"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	blocks, err := decodeContent(data)
	if err != nil {
		return err
	}
	switch probe.Role {
	case RoleSystem:
		var w systemWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		*m = Message{
			Role: RoleSystem, Content: blocks, Sections: w.Sections,
			ToolsAdded: w.ToolsAdded, ToolsRemoved: w.ToolsRemoved, TimestampMS: w.Timestamp,
		}
	case RoleUser:
		var w userWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		*m = Message{Role: RoleUser, Content: blocks, TimestampMS: w.Timestamp}
	case RoleAssistant:
		var w assistantWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		*m = Message{
			Role: RoleAssistant, Content: blocks,
			API: w.API, Provider: w.Provider, Model: w.Model,
			ResponseModel: w.ResponseModel, ResponseID: w.ResponseID,
			ProviderThinkingLevel: w.ProviderThinkingLevel, ThinkingLevel: w.ThinkingLevel,
			Diagnostics: w.Diagnostics, Usage: w.Usage, StopReason: w.StopReason,
			Deferred: w.Deferred, ErrorMessage: w.ErrorMessage, RawStopReason: w.RawStopReason,
			EndTurn: w.EndTurn, TimestampMS: w.Timestamp, DurationMS: w.DurationMS,
		}
	case RoleToolResult:
		var w toolResultWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		*m = Message{
			Role: RoleToolResult, ToolCallID: w.ToolCallID, ToolName: w.ToolName,
			Content: blocks, Details: w.Details, NestedCalls: w.NestedCalls,
			Usage: w.Usage, IsError: w.IsError, TimestampMS: w.Timestamp, DurationMS: w.DurationMS,
		}
	default:
		return fmt.Errorf("ai: unknown message role %q", probe.Role)
	}
	return nil
}

// decodeContent reads the `content` field as either a bare string (system and
// user messages) or an array of blocks (every role).
func decodeContent(data []byte) ([]ContentBlock, error) {
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	if len(probe.Content) == 0 {
		return nil, nil
	}
	if probe.Content[0] == '"' {
		var text string
		if err := json.Unmarshal(probe.Content, &text); err != nil {
			return nil, err
		}
		return []ContentBlock{TextBlock(text)}, nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(probe.Content, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// MessageText returns the message's text blocks joined with "\n".
//
// Mirrors pi's contentText() (packages/ai/src/utils/text.ts:6-12): the
// separator is "\n" by default, not "".
func MessageText(m Message) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ConstrainedSampling mirrors pi's Tool.constrainedSampling field, whose type
// is `false | ConstrainedSamplingConfig | undefined` (types.ts:739-754).
//
// nil pointer  → field absent (undefined)
// Disabled     → the literal false
// otherwise    → the config object (Kind is "json_schema" or "grammar")
type ConstrainedSampling struct {
	Disabled bool
	Kind     string            // "json_schema" | "grammar"
	Strict   string            // "prefer" | "require" (json_schema only)
	Variants map[string]string // grammar format → grammar (grammar only)
}

// MarshalJSON implements the three-state field described above.
func (c *ConstrainedSampling) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	if c.Disabled {
		return []byte("false"), nil
	}
	return json.Marshal(struct {
		Type     string            `json:"type"`
		Strict   string            `json:"strict,omitempty"`
		Variants map[string]string `json:"variants,omitempty"`
	}{Type: c.Kind, Strict: c.Strict, Variants: c.Variants})
}

// Tool is a tool declaration sent to the model.
//
// pi: packages/ai/src/types.ts:749-754.
type Tool struct {
	Name                string               `json:"name"`
	Description         string               `json:"description"`
	Parameters          json.RawMessage      `json:"parameters"` // JSON Schema
	ConstrainedSampling *ConstrainedSampling `json:"constrainedSampling,omitempty"`
}

// ToolReference names a tool without declaring it.
//
// pi: packages/ai/src/types.ts:756-758.
type ToolReference struct {
	Name string `json:"name"`
}
