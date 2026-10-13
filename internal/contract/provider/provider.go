// Package provider defines the model-facing contract.
//
// # Design rule
//
// A provider is resolved by kind from a registry, never selected by a switch.
// Adding a vendor is either a config entry (for any OpenAI-compatible or
// Anthropic-compatible endpoint) or a new package that self-registers in
// init(). Nothing in the engine changes either way.
//
// # Layering
//
// Part of the contract layer. It imports nothing from ultrac. Concrete
// implementations live in internal/llm/<vendor>, which import this package and
// never the other way round.
//
// Deprecated: this package is the Claude Code-shaped contract (Request.System,
// coarse Chunk events, end_turn-style stop reasons). It is replaced by
// internal/ai, which freezes the pi harness contract (C1 message types, C2
// streaming events) — see docs/pi-core-harness-plan.md §2.4. It still
// compiles and serves the existing engine during the migration; it is
// scheduled for deletion in phase P7. New code must not import it.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sort"
	"sync"
)

// Role is a conversation participant.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockKind discriminates a content block.
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockThinking   BlockKind = "thinking"
	BlockToolUse    BlockKind = "tool_use"
	BlockToolResult BlockKind = "tool_result"
)

// Block is one piece of a message.
//
// It is the wire-level shape, so the same struct carries an assistant's tool
// call and the result the engine sends back for it. Keeping one struct makes
// the transcript round-trip without a translation step on resume.
type Block struct {
	Kind BlockKind `json:"kind"`

	Text string `json:"text,omitempty"`

	// Signature authenticates an extended-thinking block. It is sent back
	// verbatim on the next turn; dropping it makes the API reject the whole
	// request, so it is modelled rather than treated as an opaque extra.
	Signature string `json:"signature,omitempty"`

	ToolUseID string          `json:"toolUseId,omitempty"`
	ToolName  string          `json:"toolName,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	IsError   bool            `json:"isError,omitempty"`
}

// Message is one turn of the conversation.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// ToolSchema is a tool declaration sent to the model.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Thinking requests extended reasoning. A nil pointer means "do not ask for it".
type Thinking struct {
	BudgetTokens int `json:"budgetTokens"`
}

// Request is a single completion request.
type Request struct {
	Model     string       `json:"model"`
	System    string       `json:"system,omitempty"`
	Messages  []Message    `json:"messages"`
	Tools     []ToolSchema `json:"tools,omitempty"`
	MaxTokens int          `json:"maxTokens"`

	// Temperature is a pointer so that "unset" and "0" stay distinguishable.
	// Providers differ on which of those means "use your default".
	Temperature *float64 `json:"temperature,omitempty"`

	Thinking *Thinking `json:"thinking,omitempty"`
}

// StopReason explains why generation ended.
//
// Values are wire identifiers shared with the TypeScript shell.
type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"
	StopToolUse      StopReason = "tool_use"
	StopMaxTokens    StopReason = "max_tokens"
	StopStopSequence StopReason = "stop_sequence"
	StopRefusal      StopReason = "refusal"
)

// Usage is token accounting for one request.
type Usage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}

// ToolCall is a complete tool invocation.
//
// Streaming deltas are accumulated by index inside the provider, so by the
// time a ToolCall leaves this package its Input is whole and parseable. That
// invariant is the reason callers can treat a partial call as a bug rather
// than a case to handle.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ChunkKind discriminates a Chunk.
type ChunkKind string

const (
	ChunkText     ChunkKind = "text"
	ChunkThinking ChunkKind = "thinking"
	ChunkToolCall ChunkKind = "tool_call"
	ChunkUsage    ChunkKind = "usage"
	ChunkStop     ChunkKind = "stop"
)

// Chunk is one step of a streamed response.
type Chunk struct {
	Kind     ChunkKind  `json:"kind"`
	Text     string     `json:"text,omitempty"`
	ToolCall *ToolCall  `json:"toolCall,omitempty"`
	Usage    *Usage     `json:"usage,omitempty"`
	Stop     StopReason `json:"stop,omitempty"`
}

// Provider is a model endpoint.
//
// Stream returns an iterator rather than a channel on purpose: the consumer
// stopping the loop is the cancellation signal, so a frontend that stops
// reading cannot leave a goroutine blocked on a send. Errors travel in the
// second slot, which makes ignoring them a visible choice rather than an
// easy accident.
type Provider interface {
	// Name is the configured instance name, e.g. "deepseek".
	Name() string
	// Kind is the registered implementation, e.g. "anthropic" or "openai".
	Kind() string
	// Stream runs one request. Setup failures are reported as the first
	// error yielded, never as a panic.
	Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error]
}

// Config is a resolved provider instance.
//
// Name identifies the instance; Kind selects the implementation. The
// credential fields mirror the names the TypeScript implementation read from
// settings.json, so one file configures both during the migration.
type Config struct {
	Name      string
	Kind      string
	BaseURL   string
	Model     string
	APIKey    string
	AuthToken string
	// Extra carries kind-specific options that don't deserve a field yet.
	Extra map[string]any
}

// Factory builds a Provider from a resolved Config.
type Factory func(cfg Config) (Provider, error)

var (
	mu       sync.RWMutex
	registry = make(map[string]Factory)
)

// Register adds a factory under a kind. Call it from a provider package's
// init(), so the only wiring cmd/ultrac needs is a blank import.
//
// It panics on a duplicate kind or a nil factory, following
// database/sql.Register: both are programming errors that would otherwise
// surface as a confusing runtime failure much later.
func Register(kind string, f Factory) {
	if kind == "" {
		panic("provider: Register called with an empty kind")
	}
	if f == nil {
		panic("provider: Register called with a nil factory for kind " + kind)
	}

	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[kind]; dup {
		panic("provider: duplicate registration for kind " + kind)
	}
	registry[kind] = f
}

// New instantiates the provider for cfg.Kind.
func New(cfg Config) (Provider, error) {
	mu.RLock()
	f, ok := registry[cfg.Kind]
	mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("provider: unknown kind %q (registered: %v)", cfg.Kind, Kinds())
	}
	return f(cfg)
}

// Kinds lists registered kinds in lexical order.
func Kinds() []string {
	mu.RLock()
	defer mu.RUnlock()

	kinds := make([]string, 0, len(registry))
	for k := range registry {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// Registered reports whether a kind is registered.
func Registered(kind string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := registry[kind]
	return ok
}
