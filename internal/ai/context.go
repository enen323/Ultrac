package ai

import (
	"encoding/json"
	"sort"
	"strings"
)

// StreamOptions is the per-request option set shared by the stream entry
// points and carried by the agent loop config (pi's SimpleStreamOptions,
// packages/ai/src/types.ts:369-377, over StreamOptions at 212-262).
//
// It is the contract-level subset: the fields the loop and every provider
// agree on. Provider-specific options (transport selection, custom fetch,
// headers, env overrides, the onPayload/onResponse callbacks, retry-delay
// caps) live in the provider packages and are not part of this frozen
// contract; P1 lands them with the anthropic adapter.
//
// Pointer fields keep "unset" distinguishable from a zero value, because
// providers differ on which of those means "use your default".
type StreamOptions struct {
	// APIKey is the resolved credential for this request.
	APIKey string `json:"apiKey,omitempty"`
	// Temperature is the sampling temperature.
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens caps generation length.
	MaxTokens int `json:"maxTokens,omitempty"`
	// Reasoning is the requested thinking level.
	Reasoning ThinkingLevel `json:"reasoning,omitempty"`
	// ToolChoice is provider-neutral tool selection.
	ToolChoice ToolChoice `json:"toolChoice,omitempty"`
	// ThinkingBudgets overrides token budgets per level (token-based
	// providers only).
	ThinkingBudgets *ThinkingBudgets `json:"thinkingBudgets,omitempty"`
	// SamplingParams are merged into the request body after the named
	// fields; only OpenAI-compatible adapters apply them.
	SamplingParams SamplingParams `json:"samplingParams,omitempty"`
	// CacheRetention is the prompt cache retention preference.
	CacheRetention CacheRetention `json:"cacheRetention,omitempty"`
	// SessionID enables provider session-aware features where supported.
	SessionID string `json:"sessionId,omitempty"`
	// Metadata is request metadata providers may extract fields from.
	Metadata map[string]any `json:"metadata,omitempty"`
	// TimeoutMS is the HTTP timeout for providers that support it.
	TimeoutMS int `json:"timeoutMs,omitempty"`
	// MaxRetries caps provider client-side retries.
	MaxRetries int `json:"maxRetries,omitempty"`
}

// Context is the request input accepted by stream entry points.
//
// systemPrompt and tools are shorthand for a leading system message;
// NormalizeContext folds them into one before the request reaches a provider.
//
// pi: packages/ai/src/types.ts:760-770.
type Context struct {
	SystemPrompt string
	Messages     []Message
	Tools        []Tool
}

// TranscriptContext is the normalized request context passed to providers.
//
// The prompt and tool declarations are carried by the transcript's system
// messages, never by context fields. NormalizeContext is the only intended
// producer, so a raw Context cannot reach provider code by accident.
//
// On the brand: pi marks the type with a unique symbol, which makes it
// genuinely unconstructible outside its module. Go has no equivalent — an
// unexported field does NOT stop another package from writing
// ai.TranscriptContext{Messages: ...} with a keyed literal. The field below
// is therefore documentation, not enforcement; the enforcement is that
// providers are only ever handed the value NormalizeContext returned, and
// StreamFn's signature takes the type by value.
//
// pi: packages/ai/src/types.ts:772-783.
type TranscriptContext struct {
	Messages []Message

	// normalized records that this value came out of NormalizeContext. It is
	// informational: see the brand note above.
	normalized bool
}

// Normalize folds SystemPrompt and Tools into a leading system message and
// returns the branded TranscriptContext.
//
// pi: packages/ai/src/utils/transcript.ts:10-34 (createInitialSystemMessage +
// normalizeContext).
func NormalizeContext(c Context) TranscriptContext {
	msg, ok := CreateInitialSystemMessage(c.SystemPrompt, c.Tools)
	if !ok {
		return TranscriptContext{Messages: c.Messages, normalized: true}
	}
	messages := make([]Message, 0, len(c.Messages)+1)
	messages = append(messages, msg)
	messages = append(messages, c.Messages...)
	return TranscriptContext{Messages: messages, normalized: true}
}

// CreateInitialSystemMessage builds the leading system message for a prompt
// and tool set. It reports false when both are empty, so an empty transcript
// stays empty.
//
// pi: packages/ai/src/utils/transcript.ts:10-23.
func CreateInitialSystemMessage(systemPrompt string, tools []Tool) (Message, bool) {
	hasPrompt := systemPrompt != ""
	hasTools := len(tools) > 0
	if !hasPrompt && !hasTools {
		return Message{}, false
	}
	m := Message{Role: RoleSystem, Content: []ContentBlock{TextBlock(systemPrompt)}}
	if hasTools {
		m.ToolsAdded = tools
	}
	return m, true
}

// GetInitialSystemMessage returns the leading system message, if the
// transcript starts with one.
//
// pi: packages/ai/src/utils/transcript.ts:44-49.
func GetInitialSystemMessage(messages []Message) (Message, bool) {
	if len(messages) == 0 || messages[0].Role != RoleSystem {
		return Message{}, false
	}
	return messages[0], true
}

// WithoutInitialSystemMessage drops the leading system message for APIs that
// carry the prompt outside the message list.
//
// pi: packages/ai/src/utils/transcript.ts:54-55.
func WithoutInitialSystemMessage(messages []Message) []Message {
	if _, ok := GetInitialSystemMessage(messages); ok {
		return messages[1:]
	}
	return messages
}

// GetCurrentTools resolves the tools available after applying every
// transcript system-message delta in order: removals delete by name, later
// additions replace earlier ones.
//
// pi: packages/ai/src/utils/transcript.ts:57-66.
func GetCurrentTools(messages []Message) []Tool {
	byName := make(map[string]Tool)
	order := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.Role != RoleSystem {
			continue
		}
		for _, ref := range m.ToolsRemoved {
			if _, ok := byName[ref.Name]; ok {
				delete(byName, ref.Name)
				order = removeString(order, ref.Name)
			}
		}
		for _, t := range m.ToolsAdded {
			if _, ok := byName[t.Name]; !ok {
				order = append(order, t.Name)
			}
			byName[t.Name] = t
		}
	}
	out := make([]Tool, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// removeString returns s without v. It allocates a fresh slice: compacting in
// place (s[:0]) would mutate the caller's backing array, and the replay passes
// slices it keeps using.
func removeString(s []string, v string) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// replaySystemMessage replays every system message into one holding the
// current prompt and tools: later `content` is appended to the base prompt,
// `sections` are patched by name (nil removes), tools resolve through
// GetCurrentTools. It also returns the section names in first-declaration
// order — pi gets that order from its Map (transcript.ts:73-96); Go maps are
// unordered, so the order is tracked explicitly here because the rendered
// prompt is order-sensitive.
//
// Within one message the section order is the sorted key order: a decoded Go
// map carries no insertion order, and sorted is the only deterministic
// choice. Cross-message order (the one that matters for replay) is exact.
func replaySystemMessage(messages []Message) (msg Message, sectionNames []string, ok bool) {
	var contents []string
	sections := make(map[string]*string)
	var order []string
	timestamp := int64(0)
	haveTimestamp := false
	for _, m := range messages {
		if m.Role != RoleSystem {
			continue
		}
		if !haveTimestamp {
			timestamp, haveTimestamp = m.TimestampMS, true
		}
		if text := MessageText(m); text != "" {
			contents = append(contents, text)
		}
		for _, name := range sortedKeys(m.Sections) {
			value := m.Sections[name]
			if value == nil {
				if _, present := sections[name]; present {
					delete(sections, name)
					order = removeString(order, name)
				}
				continue
			}
			if _, present := sections[name]; !present {
				order = append(order, name)
			}
			sections[name] = value
		}
	}
	tools := GetCurrentTools(messages)
	if !haveTimestamp && len(tools) == 0 {
		return Message{}, nil, false
	}
	out := Message{Role: RoleSystem, TimestampMS: timestamp}
	if len(contents) > 0 {
		out.Content = []ContentBlock{TextBlock(joinParagraphs(contents))}
	}
	if len(sections) > 0 {
		out.Sections = sections
	}
	if len(tools) > 0 {
		out.ToolsAdded = tools
	}
	return out, order, true
}

func sortedKeys(m map[string]*string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// GetCurrentSystemMessage replays every system message into one holding the
// current prompt and tools.
//
// pi: packages/ai/src/utils/transcript.ts:73-96.
func GetCurrentSystemMessage(messages []Message) (Message, bool) {
	msg, _, ok := replaySystemMessage(messages)
	return msg, ok
}

func joinParagraphs(parts []string) string {
	return strings.Join(parts, "\n\n")
}

// SystemMessageText renders a system message as a complete prompt: its
// content followed by its non-nil sections, joined with blank lines. Section
// order is the sorted key order — a decoded map carries no insertion order;
// use GetCurrentSystemPrompt for the replay order.
//
// pi: packages/ai/src/utils/text.ts:14-21 (getSystemMessageText).
func SystemMessageText(m Message) string {
	parts := []string{MessageText(m)}
	for _, name := range sortedKeys(m.Sections) {
		if v := m.Sections[name]; v != nil {
			parts = append(parts, *v)
		}
	}
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

// GetCurrentSystemPrompt renders the current system prompt text after
// replaying every system message, sections included, in replay order.
//
// pi: packages/ai/src/utils/transcript.ts:99-102, which composes
// getCurrentSystemMessage with getSystemMessageText (text.ts:14-21).
func GetCurrentSystemPrompt(messages []Message) string {
	msg, order, ok := replaySystemMessage(messages)
	if !ok {
		return ""
	}
	parts := []string{MessageText(msg)}
	for _, name := range order {
		if v := msg.Sections[name]; v != nil {
			parts = append(parts, *v)
		}
	}
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

// CollapseSystemMessages rebuilds the transcript for APIs without
// mid-conversation system messages: the replayed system message leads and
// every later system message is dropped.
//
// pi: packages/ai/src/utils/transcript.ts:108-112.
func CollapseSystemMessages(messages []Message) []Message {
	head, ok := GetCurrentSystemMessage(messages)
	rest := make([]Message, 0, len(messages)+1)
	for _, m := range messages {
		if m.Role != RoleSystem {
			rest = append(rest, m)
		}
	}
	if !ok {
		return rest
	}
	return append([]Message{head}, rest...)
}

// ResolveTranscript keeps later system messages in place when the model
// accepts them; otherwise it collapses them.
//
// pi: packages/ai/src/utils/transcript.ts:115-120.
func ResolveTranscript(messages []Message, supportsMidConvoSystemMessages bool) []Message {
	if supportsMidConvoSystemMessages {
		return messages
	}
	return CollapseSystemMessages(messages)
}

// GetToolStateChanges diffs two complete tool states. A changed definition is
// a removal followed by an addition (pi: transcript.ts:150-167). This is the
// machinery behind the agent loop's "tool loading changed" announcement
// (docs/pi-core-harness-plan.md §1.1 item 2).
func GetToolStateChanges(previous, current []Tool) (added []Tool, removed []ToolReference) {
	prevByLen := make(map[string]Tool, len(previous))
	for _, t := range previous {
		prevByLen[t.Name] = t
	}
	curByName := make(map[string]Tool, len(current))
	for _, t := range current {
		curByName[t.Name] = t
	}
	for _, t := range current {
		prev, ok := prevByLen[t.Name]
		if !ok || !DeclarationsEqual(prev, t) {
			added = append(added, t)
		}
	}
	for _, t := range previous {
		cur, ok := curByName[t.Name]
		if !ok || !DeclarationsEqual(t, cur) {
			removed = append(removed, ToolReference{Name: t.Name})
		}
	}
	return added, removed
}

// DeclarationsEqual reports whether two tools declare the same interface to
// the model. Both sides go through a JSON round-trip first, which drops
// in-memory-only fields and normalizes key order, so the comparison is exact.
//
// pi: packages/ai/src/utils/transcript.ts:123-142 (toToolDeclaration +
// declarationsEqual).
func DeclarationsEqual(left, right Tool) bool {
	lj, err := json.Marshal(Tool{
		Name: left.Name, Description: left.Description,
		Parameters: left.Parameters, ConstrainedSampling: left.ConstrainedSampling,
	})
	if err != nil {
		return false
	}
	rj, err := json.Marshal(Tool{
		Name: right.Name, Description: right.Description,
		Parameters: right.Parameters, ConstrainedSampling: right.ConstrainedSampling,
	})
	if err != nil {
		return false
	}
	return string(lj) == string(rj)
}
