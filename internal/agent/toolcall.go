package agent

// Tool-call execution: the Go transcription of the prepare/execute/finalize
// machinery in pi's packages/agent/src/agent-loop.ts (prepareToolCall at
// :708-777, executePreparedToolCall at :821-857, finalizeExecutedToolCall at
// :859-910) plus the two batch executors (:530-660).
//
// # The two-phase batch
//
// Preflight is serial and in source order: every call is looked up, its
// arguments prepared and validated, and beforeToolCall consulted, before any
// call executes. Only calls that pass preflight become workers. This is what
// makes `tool_execution_start` ordering deterministic and what keeps a
// validation failure from racing an execution.
//
// Execution then runs in one of two modes:
//
//   - sequential: prepare → execute → finalize → emit, one call at a time.
//   - parallel: workers run concurrently under a bounded semaphore;
//     `tool_execution_end` is emitted in completion order, while the
//     tool-result messages are emitted afterwards in source order.
//
// # Why worker events go through a queue
//
// The loop's event sink (and any subscriber behind it) is not required to be
// goroutine-safe, and the Agent reduces its state on every event. Workers
// therefore never call the sink: they push onto an unbounded queue that the
// loop goroutine drains and emits from. The queue is FIFO and every push
// happens before the worker exits, so the emit order of the end events is the
// completion order of the tools — the same guarantee pi gets from its
// single-threaded event loop, without requiring consumers to be thread-safe.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"ultrac/internal/ai"
)

// maxParallelTools bounds concurrent tool execution.
//
// Unbounded parallelism is how twenty parallel reads become twenty thousand
// open file handles. Twelve is the same order the TypeScript side used and is
// well above the batch sizes that actually occur. (Kept from the pre-port
// engine, internal/engine/tools.go:20-25.)
const maxParallelTools = 12

// Tool is the type-erased view of an AgentTool that the runtime stores.
//
// Inputs arrive as JSON from the model and a registry has to hold tools whose
// input types differ, so the loop only ever sees this interface. Erase
// produces it from a typed AgentTool[In].
//
// The split mirrors pi's boundary: AgentTool<TParameters> is the authoring
// shape; the loop works with the untyped dispatch shape.
type Tool interface {
	// Name is the identifier the model calls.
	Name() string
	// Label is the human-readable display name.
	Label() string
	// Description is what the model is told the tool does.
	Description() string
	// Parameters is the tool's JSON Schema.
	Parameters() json.RawMessage
	// ExecutionMode is this tool's per-call mode override; the empty string
	// means "use the loop default".
	ExecutionMode() ToolExecutionMode

	// Prepare turns raw model arguments into the tool's validated input,
	// boxed as any. It runs the tool's PrepareArguments shim (if any) and
	// then validates against the tool's schema. It returns an error when
	// either step fails; the loop converts that into an isError tool result.
	Prepare(raw map[string]any) (any, error)

	// Run executes one call whose arguments Prepare already validated. The
	// boxed value is the tool's In. A returned error is a tool-level
	// failure the loop converts into an isError tool result.
	Run(ctx context.Context, toolCallID string, in any, onUpdate func(Result)) (Result, error)
}

// Erase adapts a typed AgentTool into the runtime Tool.
//
// The schema is compiled once here; a schema that fails to compile is not a
// registration panic but a per-call error, so one broken tool cannot take the
// process down at init.
func Erase[In any](t *AgentTool[In]) Tool {
	e := &erasedTool[In]{tool: t}
	if len(t.Parameters) > 0 {
		schema, err := compileSchema(t.Parameters)
		if err != nil {
			e.compileErr = fmt.Errorf("agent: tool %q: invalid parameters schema: %w", t.Name, err)
		} else {
			e.schema = schema
		}
	}
	return e
}

type erasedTool[In any] struct {
	tool       *AgentTool[In]
	schema     *jsonschema.Schema
	compileErr error
}

func (e *erasedTool[In]) Name() string                     { return e.tool.Name }
func (e *erasedTool[In]) Label() string                    { return e.tool.Label }
func (e *erasedTool[In]) Description() string              { return e.tool.Description }
func (e *erasedTool[In]) Parameters() json.RawMessage      { return e.tool.Parameters }
func (e *erasedTool[In]) ExecutionMode() ToolExecutionMode { return e.tool.ExecutionMode }

// Prepare runs the shim, decodes, and schema-validates, in pi's preflight
// order (packages/agent/src/agent-loop.ts): lookup → prepareArguments →
// schema validation → beforeToolCall.
//
// What gets validated depends on the shim, mirroring pi: with no shim, the
// raw arguments the model sent are validated, so unknown fields, wrong types
// and missing required keys are all caught. With a shim, the shim's output is
// validated — a typed Go struct cannot carry unknown fields, so the shim
// author owns the final shape (pi's contract for prepareArguments is the
// same: "must return an object that matches TParameters", types.ts:469-473).
func (e *erasedTool[In]) Prepare(raw map[string]any) (any, error) {
	if e.compileErr != nil {
		return nil, e.compileErr
	}
	if e.tool.PrepareArguments == nil {
		if e.schema != nil {
			if err := e.validate(raw); err != nil {
				return nil, err
			}
		}
		return e.decode(raw)
	}
	in, err := e.tool.PrepareArguments(raw)
	if err != nil {
		return nil, err
	}
	if e.schema != nil {
		if err := e.validate(in); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// validate checks a JSON-serializable value against the tool's schema.
func (e *erasedTool[In]) validate(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("agent: tool %s: cannot encode arguments: %w", e.tool.Name, err)
	}
	decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("agent: tool %s: cannot decode arguments: %w", e.tool.Name, err)
	}
	if err := e.schema.Validate(decoded); err != nil {
		return fmt.Errorf("agent: tool %s: invalid arguments: %w", e.tool.Name, err)
	}
	return nil
}

// decode turns raw model arguments into the tool's input type. There is no
// empty-map shortcut: an empty object must still go through validation
// upstream, where `required` rejects it.
func (e *erasedTool[In]) decode(raw map[string]any) (In, error) {
	var zero In
	encoded, err := json.Marshal(raw)
	if err != nil {
		return zero, fmt.Errorf("agent: tool %s: cannot encode arguments: %w", e.tool.Name, err)
	}
	var in In
	if err := json.Unmarshal(encoded, &in); err != nil {
		return zero, fmt.Errorf("agent: tool %s: invalid input: %w", e.tool.Name, err)
	}
	return in, nil
}

func (e *erasedTool[In]) Run(ctx context.Context, toolCallID string, in any, onUpdate func(Result)) (Result, error) {
	typed, ok := in.(In)
	if !ok {
		return Result{}, fmt.Errorf("agent: tool %s: prepared arguments have type %T, want %T", e.tool.Name, in, zeroOf[In]())
	}
	return e.tool.Execute(ctx, toolCallID, typed, onUpdate)
}

func zeroOf[In any]() In { var zero In; return zero }

// compileSchema compiles a JSON Schema document. Compilation is done once per
// tool, at Erase time, so per-call cost is validation only.
func compileSchema(doc json.RawMessage) (*jsonschema.Schema, error) {
	// v6's AddResource takes the decoded JSON value, not a reader.
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	const resource = "urn:ultrac:agent:tool-parameters"
	if err := compiler.AddResource(resource, value); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

// ValidateArguments checks raw arguments against a JSON Schema document. It
// is exported for callers that validate outside the tool-call path (tests,
// the probe command).
//
// It recompiles on every call, which is right for a one-off check and wrong
// for a hot path: a tool whose schema is only known at runtime (internal/mcp's
// remote tools) must use CompileSchema once and hold the result.
func ValidateArguments(schema json.RawMessage, args map[string]any) error {
	compiled, err := compileSchema(schema)
	if err != nil {
		return err
	}
	return compiled.Validate(args)
}

// CompiledSchema is a JSON Schema compiled once and validated against many
// times.
//
// It exists because a tool's parameters are not always a compile-time
// constant. An MCP tool's schema arrives over the wire, and compiling it per
// call — the only thing ValidateArguments offers — would put a compiler in
// the path of every tool call.
type CompiledSchema struct {
	schema *jsonschema.Schema
}

// CompileSchema compiles a JSON Schema document.
//
// A document that does not compile is an error, not a panic. That is the
// contract every caller in this package keeps: a broken tool's schema must
// cost the tool its calls, not the process its life.
func CompileSchema(doc json.RawMessage) (*CompiledSchema, error) {
	schema, err := compileSchema(doc)
	if err != nil {
		return nil, err
	}
	return &CompiledSchema{schema: schema}, nil
}

// Validate checks a value against the compiled schema. The value is encoded
// to JSON first because the validator works on decoded JSON, not on Go maps —
// a map[string]any holding a float64 and a JSON number are two different
// things to a schema that says "integer".
func (c *CompiledSchema) Validate(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	return c.schema.Validate(decoded)
}

// Registry is the set of tools available for one run, assembled from
// built-ins plus whatever the caller contributes. The loop only ever sees a
// Registry, so a plugin tool and a built-in are indistinguishable from its
// side.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Tool
}

// NewRegistry returns an empty Registry, optionally seeded with tools.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool)}
	for _, t := range tools {
		_ = r.Add(t)
	}
	return r
}

// Add registers a tool under its name. A collision is an error rather than a
// silent overwrite: the caller has to say which one wins.
func (r *Registry) Add(t Tool) error {
	if t == nil {
		return fmt.Errorf("agent: cannot register a nil tool")
	}
	if t.Name() == "" {
		return fmt.Errorf("agent: cannot register a tool with an empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[t.Name()]; dup {
		return fmt.Errorf("agent: tool %q is already registered", t.Name())
	}
	r.byName[t.Name()] = t
	return nil
}

// Lookup finds a tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// All returns every registered tool, sorted by name.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.byName))
	for _, t := range r.byName {
		out = append(out, t)
	}
	sortTools(out)
	return out
}

// Names returns the registered tool names, sorted.
func (r *Registry) Names() []string {
	all := r.All()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name())
	}
	return out
}

func sortTools(tools []Tool) {
	for i := 1; i < len(tools); i++ {
		for j := i; j > 0 && tools[j].Name() < tools[j-1].Name(); j-- {
			tools[j], tools[j-1] = tools[j-1], tools[j]
		}
	}
}

// --- tool-call pipeline ----------------------------------------------------
//
// pi's agent-loop.ts models the pipeline as a small union: a call is either
// "prepared" (ready to execute) or "immediate" (already finished, usually as
// an error). Go has no cheap sum type, so the union is a tagged struct with
// exactly one non-nil field.

// preparedToolCall is a call that passed preflight and is ready to execute.
//
// pi: agent-loop.ts:662-667.
type preparedToolCall struct {
	// ToolCall is the original call from the assistant message — arguments
	// as the model sent them, even when a shim rewrote them for execution.
	ToolCall ai.ContentBlock
	Tool     Tool
	// Args is the validated input, boxed: the tool's In.
	Args any
}

// immediateToolCallOutcome is a call that finished during preflight: unknown
// tool, invalid arguments, blocked by beforeToolCall, or aborted.
//
// pi: agent-loop.ts:669-673.
type immediateToolCallOutcome struct {
	Result  Result
	IsError bool
}

// executedToolCallOutcome is what execute produced, before hooks.
//
// pi: agent-loop.ts:675-679.
type executedToolCallOutcome struct {
	Result     Result
	IsError    bool
	DurationMS int64
}

// executedToolBatch is one assistant message's worth of tool results.
//
// pi: agent-loop.ts:525-528.
type executedToolBatch struct {
	// messages are the tool-result messages in source order.
	messages []ai.Message
	// terminate stops the loop after this batch when every finalized result
	// in it set Terminate.
	terminate bool
}

// ToolCallHooks is the before/after pair the pipeline consults.
//
// pi: agent-loop.ts:683-684 (ToolCallHooks = Pick<AgentLoopConfig, …>).
type ToolCallHooks struct {
	BeforeToolCall BeforeToolCallFunc
	AfterToolCall  AfterToolCallFunc
}

// hooks projects the loop config onto the pipeline's hook pair.
func (c AgentLoopConfig) hooks() ToolCallHooks {
	return ToolCallHooks{BeforeToolCall: c.BeforeToolCall, AfterToolCall: c.AfterToolCall}
}

// prepareToolCall runs one call's preflight: lookup → prepareArguments →
// schema validation → beforeToolCall. It never executes the tool.
//
// Every failure becomes an immediate error outcome — including unknown tools,
// which must still produce a tool_result (the API requires one result per
// tool_use, so skipping an unknown call would make the next request invalid
// rather than merely less useful).
//
// pi: agent-loop.ts:708-777.
func prepareToolCall(
	ctx context.Context,
	currentContext AgentContext,
	assistantMessage ai.Message,
	toolCall ai.ContentBlock,
	hooks ToolCallHooks,
	tools []Tool,
) toolCallPreparation {
	if tools == nil {
		tools = currentContext.Tools
	}
	tool := findTool(tools, toolCall.Name)
	if tool == nil {
		return immediateOutcome(immediateToolCallOutcome{
			Result:  createErrorToolResult(fmt.Sprintf("Tool %s not found", toolCall.Name)),
			IsError: true,
		})
	}

	// PrepareArguments shim + schema validation + typed decode.
	args, err := tool.Prepare(toolCall.Arguments)
	if err != nil {
		return immediateOutcome(immediateToolCallOutcome{
			Result:  createErrorToolResult(err.Error()),
			IsError: true,
		})
	}

	if hooks.BeforeToolCall != nil {
		before := hooks.BeforeToolCall(ctx, BeforeToolCallContext{
			AssistantMessage: assistantMessage,
			ToolCall:         toolCall,
			Args:             args,
			Context:          currentContext,
		})
		if ctx.Err() != nil {
			return immediateOutcome(immediateToolCallOutcome{
				Result:  createErrorToolResult("Operation aborted"),
				IsError: true,
			})
		}
		if before.Block {
			reason := before.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			result := createErrorToolResult(reason)
			if before.Terminate {
				result.Terminate = true
			}
			return immediateOutcome(immediateToolCallOutcome{Result: result, IsError: true})
		}
	}
	if ctx.Err() != nil {
		return immediateOutcome(immediateToolCallOutcome{
			Result:  createErrorToolResult("Operation aborted"),
			IsError: true,
		})
	}

	return toolCallPreparation{prepared: &preparedToolCall{
		ToolCall: toolCall,
		Tool:     tool,
		Args:     args,
	}}
}

// toolCallPreparation is the tagged union prepareToolCall returns: exactly one
// of prepared/immediate is non-nil.
type toolCallPreparation struct {
	prepared  *preparedToolCall
	immediate *immediateToolCallOutcome
}

func immediateOutcome(o immediateToolCallOutcome) toolCallPreparation {
	return toolCallPreparation{immediate: &o}
}

// executePreparedToolCall runs one prepared call and measures its duration
// with a monotonic clock. A thrown tool error (a returned error) becomes an
// isError result — the loop converts it, the model reads it, the run
// continues.
//
// pi: agent-loop.ts:821-857.
func executePreparedToolCall(
	ctx context.Context,
	prepared *preparedToolCall,
	onUpdate func(Result),
) executedToolCallOutcome {
	// Updates are scoped to the invocation: a callback that fires after
	// Execute returns (an async tool holding onto the closure) is ignored,
	// exactly as pi's acceptingUpdates flag does (agent-loop.ts:826-856).
	var accepting atomic.Bool
	accepting.Store(true)
	scoped := func(partial Result) {
		if accepting.Load() {
			onUpdate(partial)
		}
	}

	startedAt := time.Now()
	result, err := prepared.Tool.Run(ctx, prepared.ToolCall.ID, prepared.Args, scoped)
	elapsed := time.Since(startedAt).Milliseconds()
	accepting.Store(false)

	if err != nil {
		return executedToolCallOutcome{
			Result:     createErrorToolResult(err.Error()),
			IsError:    true,
			DurationMS: elapsed,
		}
	}
	return executedToolCallOutcome{
		Result:     result,
		IsError:    result.IsError,
		DurationMS: elapsed,
	}
}

// finalizeExecutedToolCall applies afterToolCall's field-by-field override.
//
// Merge semantics (pi: agent-loop.ts:883-897): a provided field replaces the
// original in full, omitted fields keep the executed values. Content and
// structuredContent move together — replacing the content without the
// structured payload would leave a mismatch — and an afterToolCall that
// throws replaces the whole result with an error.
//
// pi: agent-loop.ts:859-910.
func finalizeExecutedToolCall(
	ctx context.Context,
	currentContext AgentContext,
	assistantMessage ai.Message,
	prepared *preparedToolCall,
	executed executedToolCallOutcome,
	hooks ToolCallHooks,
) ToolOutcome {
	result := executed.Result
	isError := executed.IsError

	if hooks.AfterToolCall != nil {
		after := hooks.AfterToolCall(ctx, AfterToolCallContext{
			AssistantMessage: assistantMessage,
			ToolCall:         prepared.ToolCall,
			Args:             prepared.Args,
			Result:           result,
			IsError:          isError,
			Context:          currentContext,
		})
		// Structured content not replaced along with the content may no
		// longer match it.
		structured := result.StructuredContent
		switch {
		case after.StructuredContent != nil:
			structured = after.StructuredContent
		case after.Content != nil:
			structured = nil
		}
		if after.Content != nil {
			result.Content = after.Content
		}
		if after.Details != nil {
			result.Details = after.Details
		}
		if after.Usage != nil {
			result.Usage = after.Usage
		}
		if after.Terminate != nil {
			result.Terminate = *after.Terminate
		}
		result.StructuredContent = structured
		if after.IsError != nil {
			isError = *after.IsError
		}
	}

	duration := executed.DurationMS
	return ToolOutcome{
		ToolCall:   prepared.ToolCall,
		Result:     result,
		IsError:    isError,
		DurationMS: &duration,
	}
}

// RunToolCallOptions configures RunToolCall.
//
// pi: agent-loop.ts:790-800.
type RunToolCallOptions struct {
	// Tools the call resolves against; nil means opts.Context.Tools.
	Tools []Tool
	// AssistantMessage is passed to the hooks as the message that issued
	// the call.
	AssistantMessage ai.Message
	// Context is passed to the hooks as the current agent context.
	Context AgentContext
	// Hooks apply to this call too, so a tool calling another tool goes
	// through the same policy path.
	BeforeToolCall BeforeToolCallFunc
	AfterToolCall  AfterToolCallFunc
	// OnUpdate receives partial results while the tool runs.
	OnUpdate func(Result)
}

// RunToolCall runs one tool call through the same steps as a model-issued
// call — argument preparation, schema validation, beforeToolCall, execution,
// afterToolCall — emitting no events and adding no messages. Tools that call
// other tools use this so the hooks apply to those calls too.
//
// It never fails for tool-level problems: unknown tools, validation errors,
// blocked calls, and thrown errors all come back as isError outcomes.
//
// pi: agent-loop.ts:811-819.
func RunToolCall(ctx context.Context, toolCall ai.ContentBlock, opts RunToolCallOptions) ToolOutcome {
	hooks := ToolCallHooks{BeforeToolCall: opts.BeforeToolCall, AfterToolCall: opts.AfterToolCall}
	prep := prepareToolCall(ctx, opts.Context, opts.AssistantMessage, toolCall, hooks, opts.Tools)
	if prep.immediate != nil {
		return ToolOutcome{
			ToolCall: toolCall,
			Result:   prep.immediate.Result,
			IsError:  prep.immediate.IsError,
		}
	}
	onUpdate := opts.OnUpdate
	if onUpdate == nil {
		onUpdate = func(Result) {}
	}
	executed := executePreparedToolCall(ctx, prep.prepared, onUpdate)
	return finalizeExecutedToolCall(ctx, opts.Context, opts.AssistantMessage, prep.prepared, executed, hooks)
}

// executeToolCalls runs an assistant message's tool calls, choosing the mode.
//
// The batch degrades to sequential when the loop says so or when any single
// call names a sequential tool — one order-dependent call in the batch makes
// the whole batch order-dependent.
//
// pi: agent-loop.ts:508-523.
func executeToolCalls(
	ctx context.Context,
	currentContext AgentContext,
	assistantMessage ai.Message,
	config AgentLoopConfig,
	emit EventSink,
) (executedToolBatch, error) {
	toolCalls := toolCallsOf(assistantMessage)
	hasSequentialToolCall := false
	for _, tc := range toolCalls {
		if t := findTool(currentContext.Tools, tc.Name); t != nil && t.ExecutionMode() == ExecSequential {
			hasSequentialToolCall = true
			break
		}
	}
	if config.ToolExecution == ExecSequential || hasSequentialToolCall {
		return executeToolCallsSequential(ctx, currentContext, assistantMessage, toolCalls, config, emit)
	}
	return executeToolCallsParallel(ctx, currentContext, assistantMessage, toolCalls, config, emit)
}

// executeToolCallsSequential runs the calls one at a time, in source order.
//
// pi: agent-loop.ts:530-584.
func executeToolCallsSequential(
	ctx context.Context,
	currentContext AgentContext,
	assistantMessage ai.Message,
	toolCalls []ai.ContentBlock,
	config AgentLoopConfig,
	emit EventSink,
) (executedToolBatch, error) {
	var outcomes []ToolOutcome
	var messages []ai.Message

	for _, toolCall := range toolCalls {
		if err := emit(EvToolExecutionStart{
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Name,
			Args:       toolCall.Arguments,
		}); err != nil {
			return executedToolBatch{messages: messages}, err
		}

		prep := prepareToolCall(ctx, currentContext, assistantMessage, toolCall, config.hooks(), nil)
		var finalized ToolOutcome
		if prep.immediate != nil {
			finalized = ToolOutcome{
				ToolCall: toolCall,
				Result:   prep.immediate.Result,
				IsError:  prep.immediate.IsError,
			}
		} else {
			// Sequential execution runs on the loop goroutine, so progress
			// updates can be emitted directly; a sink error still aborts the
			// run, mirroring pi's awaited update promises.
			var updateErr error
			executed := executePreparedToolCall(ctx, prep.prepared, func(partial Result) {
				if updateErr != nil {
					return
				}
				updateErr = emit(EvToolExecutionUpdate{
					ToolCallID:    toolCall.ID,
					ToolName:      toolCall.Name,
					Args:          toolCall.Arguments,
					PartialResult: partial,
				})
			})
			if updateErr != nil {
				return executedToolBatch{messages: messages}, updateErr
			}
			finalized = finalizeExecutedToolCall(ctx, currentContext, assistantMessage, prep.prepared, executed, config.hooks())
		}

		if err := emitToolExecutionEnd(finalized, emit); err != nil {
			return executedToolBatch{messages: messages}, err
		}
		msg := createToolResultMessage(finalized)
		if err := emit(EvMessageStart{Message: msg}); err != nil {
			return executedToolBatch{messages: messages}, err
		}
		if err := emit(EvMessageEnd{Message: msg}); err != nil {
			return executedToolBatch{messages: messages}, err
		}
		outcomes = append(outcomes, finalized)
		messages = append(messages, msg)

		if ctx.Err() != nil {
			break
		}
	}

	return executedToolBatch{
		messages:  messages,
		terminate: shouldTerminateToolBatch(outcomes),
	}, nil
}

// executeToolCallsParallel runs the calls concurrently after a serial
// preflight.
//
// Ordering contract (pi: agent-loop.ts:586-660):
//
//   - tool_execution_start: source order, during preflight, before any
//     execution begins.
//   - tool_execution_end: completion order — each worker's end event enters
//     the queue when that worker finishes, and the queue is FIFO.
//   - tool-result messages (message_start/message_end): source order, after
//     every call in the batch has finished.
//
// pi: agent-loop.ts:586-660.
func executeToolCallsParallel(
	ctx context.Context,
	currentContext AgentContext,
	assistantMessage ai.Message,
	toolCalls []ai.ContentBlock,
	config AgentLoopConfig,
	emit EventSink,
) (executedToolBatch, error) {
	queue := newAgentEventQueue()
	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup

	// One entry per source-order call. Immediate outcomes are filled in
	// during preflight; worker outcomes are filled in by the goroutines.
	// The loop goroutine owns the slice until a worker is spawned for an
	// entry, and reads an entry's outcome only after wg.Wait(), so the
	// happens-before edge is the WaitGroup itself.
	type entry struct {
		finalized *ToolOutcome
	}
	entries := make([]*entry, 0, len(toolCalls))

	// inFlight counts spawned workers; the last one to finish closes the
	// queue, which unblocks the drain loop.
	var inFlight atomic.Int32

	preflightErr := error(nil)
	for _, toolCall := range toolCalls {
		if preflightErr != nil {
			break
		}
		if err := emit(EvToolExecutionStart{
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Name,
			Args:       toolCall.Arguments,
		}); err != nil {
			preflightErr = err
			break
		}

		prep := prepareToolCall(ctx, currentContext, assistantMessage, toolCall, config.hooks(), nil)
		if prep.immediate != nil {
			finalized := ToolOutcome{
				ToolCall: toolCall,
				Result:   prep.immediate.Result,
				IsError:  prep.immediate.IsError,
			}
			if err := emitToolExecutionEnd(finalized, emit); err != nil {
				preflightErr = err
				break
			}
			entries = append(entries, &entry{finalized: &finalized})
			if ctx.Err() != nil {
				break
			}
			continue
		}

		e := &entry{}
		entries = append(entries, e)
		prepared := prep.prepared
		inFlight.Add(1)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			outcome := runPreparedToolCall(ctx, currentContext, assistantMessage, prepared, config, queue)
			e.finalized = &outcome
			if inFlight.Add(-1) == 0 {
				queue.close()
			}
		}()

		if ctx.Err() != nil {
			break
		}
	}

	// Drain worker-originated events on the loop goroutine while the
	// workers run. The queue is unbounded, so workers never block on a slow
	// sink and never need to be abandoned.
	//
	// When preflight produced no workers at all (every call immediate —
	// unknown tools, validation failures, a blocked batch), nobody is left
	// to close the queue, so close it here. inFlight only grows inside the
	// preflight loop above, so this check is race-free.
	if preflightErr == nil && inFlight.Load() == 0 {
		queue.close()
	}
	if preflightErr == nil {
		for {
			ev, ok := queue.pop()
			if !ok {
				break
			}
			if err := emit(ev); err != nil {
				preflightErr = err
				queue.close()
				break
			}
		}
	} else {
		// Preflight or an earlier emit failed: stop collecting worker
		// events and just wait for the in-flight calls to finish.
		queue.close()
	}
	wg.Wait()

	if preflightErr != nil {
		return executedToolBatch{}, preflightErr
	}

	// Tool-result messages in source order, after the whole batch.
	messages := make([]ai.Message, 0, len(entries))
	outcomes := make([]ToolOutcome, 0, len(entries))
	for _, e := range entries {
		if e.finalized == nil {
			continue
		}
		outcomes = append(outcomes, *e.finalized)
		msg := createToolResultMessage(*e.finalized)
		if err := emit(EvMessageStart{Message: msg}); err != nil {
			return executedToolBatch{messages: messages}, err
		}
		if err := emit(EvMessageEnd{Message: msg}); err != nil {
			return executedToolBatch{messages: messages}, err
		}
		messages = append(messages, msg)
	}

	return executedToolBatch{
		messages:  messages,
		terminate: shouldTerminateToolBatch(outcomes),
	}, nil
}

// runPreparedToolCall is the parallel worker body: abort check, execute,
// finalize, and push the end event onto the queue in completion order.
//
// pi: agent-loop.ts:619-640 (the closure pushed into finalizedCalls).
func runPreparedToolCall(
	ctx context.Context,
	currentContext AgentContext,
	assistantMessage ai.Message,
	prepared *preparedToolCall,
	config AgentLoopConfig,
	queue *agentEventQueue,
) ToolOutcome {
	if ctx.Err() != nil {
		finalized := ToolOutcome{
			ToolCall: prepared.ToolCall,
			Result:   createErrorToolResult("Operation aborted"),
			IsError:  true,
		}
		queue.push(toolExecutionEndEvent(finalized))
		return finalized
	}

	executed := executePreparedToolCall(ctx, prepared, func(partial Result) {
		queue.push(EvToolExecutionUpdate{
			ToolCallID:    prepared.ToolCall.ID,
			ToolName:      prepared.ToolCall.Name,
			Args:          prepared.ToolCall.Arguments,
			PartialResult: partial,
		})
	})
	finalized := finalizeExecutedToolCall(ctx, currentContext, assistantMessage, prepared, executed, config.hooks())
	queue.push(toolExecutionEndEvent(finalized))
	return finalized
}

// --- event and message construction ---------------------------------------

// createErrorToolResult builds the error result every failure path uses.
//
// pi: agent-loop.ts:912-917.
func createErrorToolResult(message string) Result {
	return Result{
		Content: []ai.ContentBlock{ai.TextBlock(message)},
		Details: map[string]any{},
	}
}

// createToolResultMessage turns a finalized outcome into the toolResult
// message that lands in the transcript and goes back to the model.
//
// Untyped tools can return results without content; normalize so a nil
// never enters session history or provider payloads.
//
// pi: agent-loop.ts:930-944.
func createToolResultMessage(finalized ToolOutcome) ai.Message {
	content := finalized.Result.Content
	if content == nil {
		content = []ai.ContentBlock{}
	}
	msg := ai.Message{
		Role:        ai.RoleToolResult,
		ToolCallID:  finalized.ToolCall.ID,
		ToolName:    finalized.ToolCall.Name,
		Content:     content,
		Details:     finalized.Result.Details,
		Usage:       finalized.Result.Usage,
		IsError:     finalized.IsError,
		TimestampMS: nowMS(),
	}
	if finalized.DurationMS != nil {
		msg.DurationMS = *finalized.DurationMS
	}
	return msg
}

// shouldTerminateToolBatch reports the batch-termination rule: every
// finalized result in the batch must set Terminate, and the batch must be
// non-empty. One terminating call among several does not stop the loop.
//
// pi: agent-loop.ts:690-692.
func shouldTerminateToolBatch(outcomes []ToolOutcome) bool {
	if len(outcomes) == 0 {
		return false
	}
	for _, o := range outcomes {
		if !o.Result.Terminate {
			return false
		}
	}
	return true
}

// emitToolExecutionEnd emits one tool_execution_end event.
//
// pi: agent-loop.ts:919-928.
func emitToolExecutionEnd(finalized ToolOutcome, emit EventSink) error {
	return emit(EvToolExecutionEnd{
		ToolCallID: finalized.ToolCall.ID,
		ToolName:   finalized.ToolCall.Name,
		Result:     finalized.Result,
		IsError:    finalized.IsError,
		DurationMS: finalized.DurationMS,
	})
}

// toolExecutionEndEvent is the queue-pushed form of emitToolExecutionEnd.
func toolExecutionEndEvent(finalized ToolOutcome) AgentEvent {
	return EvToolExecutionEnd{
		ToolCallID: finalized.ToolCall.ID,
		ToolName:   finalized.ToolCall.Name,
		Result:     finalized.Result,
		IsError:    finalized.IsError,
		DurationMS: finalized.DurationMS,
	}
}

// agentEventQueue is the unbounded FIFO workers push onto and the loop
// goroutine drains.
//
// It has the same shape as ai.EventStream (push never blocks, close is
// idempotent, pop drains what is left after close) but is typed for
// AgentEvent. Push never blocks: like pi's FifoQueue the pending queue is
// unbounded, so a worker is only ever waiting on its own tool, which context
// cancellation unblocks.
type agentEventQueue struct {
	mu       sync.Mutex
	nonEmpty *sync.Cond
	items    []AgentEvent
	closed   bool
}

func newAgentEventQueue() *agentEventQueue {
	q := &agentEventQueue{}
	q.nonEmpty = sync.NewCond(&q.mu)
	return q
}

// push appends one event. Pushing onto a closed queue is a no-op: the drain
// has already given up (a failed sink), and dropping the event is the point.
func (q *agentEventQueue) push(ev AgentEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, ev)
	q.nonEmpty.Signal()
}

// pop blocks until an event is available or the queue is closed, and drains
// any remaining events after close before reporting closed.
func (q *agentEventQueue) pop() (AgentEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.nonEmpty.Wait()
	}
	if len(q.items) > 0 {
		ev := q.items[0]
		q.items[0] = nil // release the reference for GC
		q.items = q.items[1:]
		return ev, true
	}
	return nil, false
}

// close marks the queue finished. Idempotent, and safe to call from the last
// worker or from the drain loop on a sink failure.
func (q *agentEventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.nonEmpty.Broadcast()
}
