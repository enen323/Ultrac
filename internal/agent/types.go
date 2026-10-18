// Package agent freezes the agent-loop-facing contract of the pi harness in
// Go: the AgentTool shape, the result and event types, the control hooks, and
// the StreamFn the loop calls (contract C3 of
// docs/pi-core-harness-plan.md).
//
// Every exported type carries a comment naming the pi TypeScript file and
// line range it was transcribed from.
//
// # Layering
//
// agent depends on ai (messages, events, model) and on nothing else in
// ultrac: it must not import any concrete provider, the tool implementations,
// the permission layer, or the shell layer (the latter two are being deleted;
// docs/pi-core-harness-plan.md §4.2).
//
// # What is deliberately absent
//
// Permission policy. pi ships no permission system and neither does this
// package: AgentTool has no canUse callback and the loop has no policy hook.
// beforeToolCall exists as a generic block/allow hook, not as a permission
// engine.
package agent

import (
	"context"
	"encoding/json"

	"ultrac/internal/ai"
)

// AgentMessage is one message of the agent's transcript.
//
// pi allows apps to extend the union through declaration merging
// (packages/agent/src/types.ts:351-374). Go has no such mechanism, so this is
// an alias of the LLM message union; custom message roles would need a new
// mechanism and are out of scope for the harness port.
type AgentMessage = ai.Message

// AgentToolCall is one tool-call content block of an assistant message.
//
// pi: packages/agent/src/types.ts:57-58 (Extract<AssistantMessage["content"][number], { type: "toolCall" }>).
type AgentToolCall = ai.ContentBlock

// ToolExecutionMode configures how tool calls from one assistant message run.
//
//   - "sequential": each call is prepared, executed, and finalized before the
//     next one starts.
//   - "parallel": calls are prepared sequentially, then allowed tools execute
//     concurrently. tool_execution_end is emitted in completion order after
//     each tool is finalized, while tool-result message artifacts are emitted
//     later in assistant source order.
//
// pi: packages/agent/src/types.ts:39-47.
type ToolExecutionMode string

const (
	ExecSequential ToolExecutionMode = "sequential"
	ExecParallel   ToolExecutionMode = "parallel"
)

// QueueMode controls how many queued messages are injected at a queue drain
// point: "all" drains everything, "one-at-a-time" drains only the oldest.
//
// pi: packages/agent/src/types.ts:49-55.
type QueueMode string

const (
	QueueAll        QueueMode = "all"
	QueueOneAtATime QueueMode = "one-at-a-time"
)

// ThinkingLevel is a requested reasoning level for models that support it.
//
// pi: packages/agent/src/types.ts:344-349. "xhigh" and "max" are only
// supported by selected model families; use the model catalog's
// thinkingLevelMap to detect support for a concrete model.
type ThinkingLevel string

const (
	ThinkOff     ThinkingLevel = "off"
	ThinkMinimal ThinkingLevel = "minimal"
	ThinkLow     ThinkingLevel = "low"
	ThinkMedium  ThinkingLevel = "medium"
	ThinkHigh    ThinkingLevel = "high"
	ThinkXHigh   ThinkingLevel = "xhigh"
	ThinkMax     ThinkingLevel = "max"
)

// Result is the final or partial result produced by a tool
// (pi's AgentToolResult).
//
// Content is the model-facing result; Details is arbitrary structured data
// for logs or UI; StructuredContent matches the tool's outputSchema and is
// not sent to the model. IsError reports a failure without throwing — the
// model sees Content as an error result, like a thrown error, but Details and
// StructuredContent are kept for callers. Terminate hints that the agent
// should stop after the current batch; early termination only happens when
// every finalized result in the batch sets it.
//
// pi: packages/agent/src/types.ts:423-446.
type Result struct {
	Content           []ai.ContentBlock
	Details           any
	StructuredContent any
	Usage             *ai.Usage
	IsError           bool
	Terminate         bool
}

// NewTextResult returns a single-text-block result.
func NewTextResult(text string) Result {
	return Result{Content: []ai.ContentBlock{ai.TextBlock(text)}}
}

// ToolOutcome is the final outcome of a tool call after hooks ran.
//
// pi: packages/agent/src/types.ts:448-455.
type ToolOutcome struct {
	ToolCall   AgentToolCall
	Result     Result
	IsError    bool
	DurationMS *int64
}

// BeforeToolCallResult is what beforeToolCall returns. Block prevents
// execution; the loop emits an error tool result whose text is Reason (or a
// default when omitted). Terminate participates in the batch
// early-termination rule.
//
// pi: packages/agent/src/types.ts:60-74.
type BeforeToolCallResult struct {
	Block     bool
	Reason    string
	Terminate bool
}

// AfterToolCallResult is a partial override of an executed tool result.
//
// Merge semantics are field-by-field: a provided field replaces the original
// in full, omitted fields keep the executed values. There is no deep merge.
// IsError and Terminate are pointers so that `false` is distinguishable from
// "not provided"; Details and StructuredContent use nil for "not provided",
// which means an explicit null cannot be expressed — acceptable because pi's
// own override path treats null details as absent too.
//
// pi: packages/agent/src/types.ts:76-104.
type AfterToolCallResult struct {
	Content           []ai.ContentBlock
	Details           any
	StructuredContent any
	IsError           *bool
	Usage             *ai.Usage
	Terminate         *bool
}

// BeforeToolCallContext is passed to beforeToolCall.
//
// pi: packages/agent/src/types.ts:106-116.
type BeforeToolCallContext struct {
	AssistantMessage ai.Message
	ToolCall         AgentToolCall
	Args             any // validated tool arguments
	Context          AgentContext
}

// AfterToolCallContext is passed to afterToolCall.
//
// pi: packages/agent/src/types.ts:118-132.
type AfterToolCallContext struct {
	AssistantMessage ai.Message
	ToolCall         AgentToolCall
	Args             any
	Result           Result
	IsError          bool
	Context          AgentContext
}

// AgentTurnContext is passed to completed-turn callbacks.
//
// NewMessages are the messages this loop invocation will return if it exits
// at this point: prompt runs include the initial prompt messages,
// continuation runs do not include pre-existing context messages.
//
// pi: packages/agent/src/types.ts:134-144.
type AgentTurnContext struct {
	Message     ai.Message
	ToolResults []ai.Message
	Context     AgentContext
	NewMessages []ai.Message
}

// TurnDecision is what finishTurn returns. The zero value preserves normal
// scheduling, matching TS undefined.
//
// pi: packages/agent/src/types.ts:146-147.
type TurnDecision string

const (
	TurnDefault  TurnDecision = "" // undefined: preserve normal scheduling
	TurnContinue TurnDecision = "continue"
	TurnEnd      TurnDecision = "end"
)

// AgentContext is the context snapshot passed into the low-level loop.
//
// pi: packages/agent/src/types.ts:501-507.
type AgentContext struct {
	// Messages is the transcript visible to the model.
	Messages []AgentMessage
	// Tools are the tools available for execution in this run.
	Tools []Tool
}

// AgentLoopTurnUpdate is replacement runtime state used before another
// provider request.
//
// pi: packages/agent/src/types.ts:160-170.
type AgentLoopTurnUpdate struct {
	Context       *AgentContext
	Messages      []AgentMessage
	Model         *ai.Model
	ThinkingLevel *ThinkingLevel
}

// PrepareRequestContext is the runtime state available immediately before a
// conversational provider request.
//
// pi: packages/agent/src/types.ts:172-177.
type PrepareRequestContext struct {
	Context       AgentContext
	Model         ai.Model
	ThinkingLevel ThinkingLevel
}

// AgentRequestUpdate is the replacement state for the request being prepared
// (AgentLoopTurnUpdate without Messages).
//
// pi: packages/agent/src/types.ts:179-180.
type AgentRequestUpdate struct {
	Context       *AgentContext
	Model         *ai.Model
	ThinkingLevel *ThinkingLevel
}

// PrepareNextTurnContext extends AgentTurnContext for prepareNextTurn.
//
// pi: packages/agent/src/types.ts:191.
type PrepareNextTurnContext = AgentTurnContext

// Hook function types. Every hook mirrors its pi counterpart's contract:
// none of them may panic, and the getters return an empty slice rather than
// nil when nothing is queued.

// ConvertToLLMFunc converts AgentMessages to LLM-compatible messages before
// each call. Messages that cannot be converted should be filtered out; the
// function must not panic — return a safe fallback instead.
//
// pi: packages/agent/src/types.ts:193-222.
type ConvertToLLMFunc func(messages []AgentMessage) []ai.Message

// TransformContextFunc transforms the message list before ConvertToLLM,
// e.g. for context-window pruning.
//
// pi: packages/agent/src/types.ts:224-244.
type TransformContextFunc func(ctx context.Context, messages []AgentMessage) []AgentMessage

// GetAPIKeyFunc resolves an API key dynamically per call; returns "" when no
// key is available.
//
// pi: packages/agent/src/types.ts:246-254.
type GetAPIKeyFunc func(provider string) string

// FinishTurnFunc is called after a completed turn and its tool results,
// immediately before turn_end. Error and aborted responses are hard exits
// regardless of the decision.
//
// pi: packages/agent/src/types.ts:149-158, 256-264.
type FinishTurnFunc func(ctx context.Context, turn AgentTurnContext) TurnDecision

// PrepareRequestFunc is called immediately before every conversational
// provider request, including the first; pending messages have already been
// appended.
//
// pi: packages/agent/src/types.ts:182-189, 266-271.
type PrepareRequestFunc func(ctx context.Context, req PrepareRequestContext) AgentRequestUpdate

// PrepareNextTurnFunc is called after turn_end when the loop will continue.
//
// pi: packages/agent/src/types.ts:273-280.
type PrepareNextTurnFunc func(ctx context.Context, turn PrepareNextTurnContext) AgentLoopTurnUpdate

// GetSteeringMessagesFunc returns messages to inject mid-run, after the
// current turn's tool calls finish.
//
// pi: packages/agent/src/types.ts:282-293.
type GetSteeringMessagesFunc func(ctx context.Context) []AgentMessage

// GetFollowUpMessagesFunc returns messages to process after the agent would
// otherwise stop.
//
// pi: packages/agent/src/types.ts:295-306.
type GetFollowUpMessagesFunc func(ctx context.Context) []AgentMessage

// BeforeToolCallFunc is called before a tool executes, after arguments have
// been validated.
//
// pi: packages/agent/src/types.ts:308-326.
type BeforeToolCallFunc func(ctx context.Context, bcx BeforeToolCallContext) BeforeToolCallResult

// AfterToolCallFunc is called after a tool finishes, before
// tool_execution_end and the tool-result message events.
//
// pi: packages/agent/src/types.ts:328-341.
type AfterToolCallFunc func(ctx context.Context, acx AfterToolCallContext) AfterToolCallResult

// StreamFn is the stream function used by the agent loop; a provider's
// streamSimple satisfies this shape.
//
// The loop passes a normalized transcript: the system prompt and tool
// declarations are carried by the transcript's system messages, never by
// context.systemPrompt or context.tools.
//
// Contract (pi: packages/agent/src/types.ts:19-37):
//   - Must not panic for request/model/runtime failures.
//   - Must return a Stream.
//   - Failures must be encoded in the returned stream via protocol events
//     and a final message with stopReason "error" or "aborted" and an
//     errorMessage.
//
// It is an alias of ai.StreamFn, not a second definition: the contract lives
// in the ai layer (where the providers implement it), and the loop must not
// own a structurally-identical-but-distinct type that a provider signature
// would silently fail to satisfy.
type StreamFn = ai.StreamFn

// AgentLoopConfig configures one loop invocation. It embeds the stream
// options, mirroring pi's AgentLoopConfig extends SimpleStreamOptions.
//
// Every hook field is optional: a nil hook means "not installed" and the
// loop treats it as a no-op (return no decision, no update, nothing queued).
// The loop implementation (P2) must nil-check every hook before calling it.
//
// pi: packages/agent/src/types.ts:193-342.
type AgentLoopConfig struct {
	ai.StreamOptions

	Model ai.Model

	ConvertToLLM        ConvertToLLMFunc
	TransformContext    TransformContextFunc
	GetAPIKey           GetAPIKeyFunc
	FinishTurn          FinishTurnFunc
	PrepareRequest      PrepareRequestFunc
	PrepareNextTurn     PrepareNextTurnFunc
	GetSteeringMessages GetSteeringMessagesFunc
	GetFollowUpMessages GetFollowUpMessagesFunc

	// ToolExecution selects sequential or parallel tool execution; the empty
	// string means the default ("parallel").
	ToolExecution ToolExecutionMode

	BeforeToolCall BeforeToolCallFunc
	AfterToolCall  AfterToolCallFunc
}

// AgentEvent is one event the Agent emits for UI updates.
//
// agent_end is the last event of a run, but awaited subscribers for it are
// still part of run settlement: the agent becomes idle only after those
// listeners finish.
//
// pi: packages/agent/src/types.ts:509-539.
type AgentEvent interface {
	agentEventName() string
}

// EvAgentStart opens a run. pi: types.ts:518.
type EvAgentStart struct{}

// EvAgentEnd closes a run with the full message list. pi: types.ts:519.
type EvAgentEnd struct {
	Messages []AgentMessage
}

// EvAgentSettled marks the run fully settled — emitted after agent_end, once
// deferred cleanup has cleared (pi: agent-session.ts:203, _emitAgentSettled).
// Aborted reports whether the run ended by abort rather than naturally.
type EvAgentSettled struct {
	Aborted bool
}

// EvTurnStart opens a turn: one assistant response plus its tool calls and
// results. pi: types.ts:521.
type EvTurnStart struct{}

// EvTurnEnd closes a turn. pi: types.ts:522.
type EvTurnEnd struct {
	Message     ai.Message
	ToolResults []ai.Message
}

// EvMessageStart opens a message of any role. pi: types.ts:524.
type EvMessageStart struct {
	Message AgentMessage
}

// EvMessageUpdate carries a streaming assistant update: the partial agent
// message plus the underlying assistant event. pi: types.ts:526.
type EvMessageUpdate struct {
	Message        AgentMessage
	AssistantEvent ai.AssistantEvent
}

// EvMessageEnd closes a message. pi: types.ts:527.
type EvMessageEnd struct {
	Message AgentMessage
}

// EvToolExecutionStart announces a tool about to run. pi: types.ts:529.
type EvToolExecutionStart struct {
	ToolCallID string
	ToolName   string
	Args       any
}

// EvToolExecutionUpdate reports partial progress from a running tool.
// pi: types.ts:530.
type EvToolExecutionUpdate struct {
	ToolCallID    string
	ToolName      string
	Args          any
	PartialResult Result
}

// EvToolExecutionEnd reports a finished tool. DurationMS is absent when the
// tool did not run. pi: types.ts:531-539.
type EvToolExecutionEnd struct {
	ToolCallID string
	ToolName   string
	Result     Result
	IsError    bool
	DurationMS *int64
}

func (EvAgentStart) agentEventName() string          { return "agent_start" }
func (EvAgentEnd) agentEventName() string            { return "agent_end" }
func (EvAgentSettled) agentEventName() string        { return "agent_settled" }
func (EvTurnStart) agentEventName() string           { return "turn_start" }
func (EvTurnEnd) agentEventName() string             { return "turn_end" }
func (EvMessageStart) agentEventName() string        { return "message_start" }
func (EvMessageUpdate) agentEventName() string       { return "message_update" }
func (EvMessageEnd) agentEventName() string          { return "message_end" }
func (EvToolExecutionStart) agentEventName() string  { return "tool_execution_start" }
func (EvToolExecutionUpdate) agentEventName() string { return "tool_execution_update" }
func (EvToolExecutionEnd) agentEventName() string    { return "tool_execution_end" }

// EventName returns the wire discriminant of ev.
func EventName(ev AgentEvent) string { return ev.agentEventName() }

// AgentState is the public agent state.
//
// Tools and Messages are copied on assignment in pi (accessor properties,
// packages/agent/src/types.ts:382-421); in Go the Agent implementation is
// responsible for copying before storing.
//
// pi: packages/agent/src/types.ts:376-421.
type AgentState struct {
	// SystemPrompt is the current prompt, replayed from the transcript's
	// system messages. Read-only: to change it, append a system message.
	SystemPrompt string
	// Model is the active model for future turns.
	Model ai.Model
	// ThinkingLevel is the requested reasoning level for future turns.
	ThinkingLevel ThinkingLevel
	// Tools are the executable tools.
	Tools []Tool
	// Messages is the conversation transcript.
	Messages []AgentMessage
	// IsStreaming is true while the agent processes a prompt or
	// continuation, until awaited agent_end listeners settle.
	IsStreaming bool
	// StreamingMessage is the partial assistant message of the current
	// streamed response, if any.
	StreamingMessage *AgentMessage
	// PendingToolCalls holds the tool call ids currently executing.
	PendingToolCalls map[string]bool
	// ErrorMessage is from the most recent failed or aborted turn.
	ErrorMessage string
}

// AgentTool is the tool definition used by the agent runtime: pi's
// AgentTool<TParameters, TDetails> (packages/agent/src/types.ts:465-499).
//
// It is generic over the tool's input type In, so the compiler checks the
// Execute implementation against the struct the model's arguments decode
// into. The runtime never sees AgentTool[In] directly — it sees the erased
// Tool interface (see Erase), because a registry has to hold tools whose
// input types differ.
//
// # Contract
//
// Execute must throw (return an error) on failure, or return a Result with
// IsError set; it must not merely describe the failure in Content. A returned
// error means the loop converts it into an isError tool result (pi: types.ts
// :479-482 and the plan §1.1 item 10).
type AgentTool[In any] struct {
	// Tool is the declaration sent to the model.
	ai.Tool

	// Label is a human-readable name for UI display.
	Label string

	// ExecutionMode overrides the loop's mode for this tool. The empty
	// string means "use the loop default".
	ExecutionMode ToolExecutionMode

	// OutputSchema is the JSON Schema of StructuredContent in successful
	// results. Tools that declare it should always set StructuredContent.
	OutputSchema json.RawMessage

	// PrepareArguments is an optional compatibility shim for raw tool-call
	// arguments before schema validation. It must return a value matching
	// the tool's parameters schema.
	PrepareArguments func(raw map[string]any) (In, error)

	// Replay is the recovery policy for an effect whose durable intent
	// exists but whose outcome is unknown: "never" or "safe".
	Replay string

	// Execute runs the tool call. onUpdate may be nil; when non-nil, calls
	// after Execute returns are ignored (pi scopes the callback to the
	// invocation, types.ts:457-463).
	Execute func(ctx context.Context, toolCallID string, in In, onUpdate func(Result)) (Result, error)
}
