package agent

// Agent: the stateful wrapper around the low-level loop — the Go transcription
// of pi's packages/agent/src/agent.ts.
//
// The Agent owns the durable transcript, reduces loop events into public
// state, dispatches events to subscribers, and exposes the two message queues
// (steering and follow-up) plus the run lifecycle (Prompt / Continue / Abort /
// WaitForIdle).
//
// # Relationship to the loop
//
// The loop (loop.go) works on a per-run snapshot of the transcript; the
// Agent's state.messages is the durable one. The two stay in sync because
// every message the loop appends is also emitted as message_start/
// message_end, and processEvents appends on message_end — the same
// double-bookkeeping pi does (the loop mutates context.messages, the Agent
// mutates _state.messages).
//
// # Shape decisions (Go vs TypeScript)
//
//   - pi's async run (a promise) becomes a goroutine plus a done channel;
//     WaitForIdle selects on it with a context.
//   - pi's AbortController becomes a cancellable context derived from the
//     caller's.
//   - pi's getter-based live state becomes a State() snapshot: slices and
//     the pending-tool-call map are copied, because a caller holding the
//     live slice would race the run.
//   - Hook options that only exist to configure providers (transport,
//     onPayload/onResponse, maxRetryDelayMs) are absent here by design: the
//     frozen ai.StreamOptions carries the contract-level subset, and
//     provider-specific knobs live in the provider packages (P1 decision).

import (
	"context"
	"errors"
	"sync"

	"ultrac/internal/ai"
)

// AgentOptions configures an Agent. Every field is optional except StreamFn,
// and a nil StreamFn falls back to the registry (ai.StreamRequest), mirroring
// pi's getDefaultStreamFn().
//
// pi: packages/agent/src/agent.ts:113-141.
type AgentOptions struct {
	// InitialState seeds the transcript, model, tools, and thinking level.
	InitialState *AgentInitialState

	ConvertToLLM        ConvertToLLMFunc
	TransformContext    TransformContextFunc
	StreamFn            StreamFn
	GetAPIKey           GetAPIKeyFunc
	BeforeToolCall      BeforeToolCallFunc
	AfterToolCall       AfterToolCallFunc
	FinishTurn          FinishTurnFunc
	PrepareRequest      PrepareRequestFunc
	PrepareNextTurn     func(ctx context.Context) *AgentLoopTurnUpdate
	PrepareNextTurnWith func(ctx context.Context, turn PrepareNextTurnContext) *AgentLoopTurnUpdate

	// SteeringMode / FollowUpMode select how many queued messages drain at a
	// queue drain point. Default: one-at-a-time.
	SteeringMode QueueMode
	FollowUpMode QueueMode

	// SessionID is forwarded to providers for cache-aware backends.
	SessionID string
	// ThinkingBudgets overrides token budgets per level.
	ThinkingBudgets *ai.ThinkingBudgets
	// MaxRetries caps provider client-side retries (provider-level policy
	// defaults to 0; the loop layer owns retry strategy).
	MaxRetries int
	// ToolExecution selects the batch strategy for multi-call messages.
	// Default: parallel.
	ToolExecution ToolExecutionMode
}

// AgentInitialState seeds a new Agent. systemPrompt and tools become the
// leading system message unless messages already starts with one.
//
// pi: packages/agent/src/agent.ts:77-81.
type AgentInitialState struct {
	SystemPrompt  string
	Model         *ai.Model
	ThinkingLevel *ThinkingLevel
	Tools         []Tool
	Messages      []ai.Message
}

// errAlreadyProcessing is returned when a run is started while another is
// active. pi throws the same way ("Agent is already processing…").
var errAlreadyProcessing = errors.New("agent: already processing a prompt; use Steer or FollowUp to queue messages, or wait for completion")

// Agent is the stateful wrapper around the low-level agent loop.
//
// pi: packages/agent/src/agent.ts:188-613.
type Agent struct {
	mu sync.Mutex

	state mutableAgentState

	listenersMu sync.Mutex
	listeners   map[uint64]func(ctx context.Context, ev AgentEvent) error
	nextID      uint64

	steeringQueue *pendingMessageQueue
	followUpQueue *pendingMessageQueue

	activeRun *activeRun

	// Configuration, fixed at construction.
	convertToLLM        ConvertToLLMFunc
	transformContext    TransformContextFunc
	streamFn            StreamFn
	getAPIKey           GetAPIKeyFunc
	beforeToolCall      BeforeToolCallFunc
	afterToolCall       AfterToolCallFunc
	finishTurn          FinishTurnFunc
	prepareRequest      PrepareRequestFunc
	prepareNextTurn     func(ctx context.Context) *AgentLoopTurnUpdate
	prepareNextTurnWith func(ctx context.Context, turn PrepareNextTurnContext) *AgentLoopTurnUpdate
	sessionID           string
	thinkingBudgets     *ai.ThinkingBudgets
	maxRetries          int
	toolExecution       ToolExecutionMode
}

// mutableAgentState is the Agent's internal state. systemPrompt is derived
// from messages (a replay of every system message), so it is not stored.
//
// pi: packages/agent/src/agent.ts:70-75 (MutableAgentState).
type mutableAgentState struct {
	model            ai.Model
	thinkingLevel    ThinkingLevel
	tools            []Tool
	messages         []ai.Message
	isStreaming      bool
	streamingMessage *ai.Message
	pendingToolCalls map[string]bool
	errorMessage     string
}

// activeRun is the in-flight run: its done channel closes after the run and
// all awaited agent_end listeners have settled, which is what WaitForIdle
// waits on.
//
// pi: packages/agent/src/agent.ts:176-180 (ActiveRun).
type activeRun struct {
	done   chan struct{}
	cancel context.CancelFunc
}

// pendingMessageQueue is one of the two injection queues.
//
// pi: packages/agent/src/agent.ts:143-174.
type pendingMessageQueue struct {
	mode     QueueMode
	messages []ai.Message
}

func (q *pendingMessageQueue) enqueue(m ai.Message) { q.messages = append(q.messages, m) }

func (q *pendingMessageQueue) hasItems() bool { return len(q.messages) > 0 }

// peek returns the messages the next drain would take, without consuming
// them: everything in "all" mode, only the oldest in "one-at-a-time".
func (q *pendingMessageQueue) peek() []ai.Message {
	if q.mode == QueueAll {
		return append([]ai.Message(nil), q.messages...)
	}
	if len(q.messages) == 0 {
		return nil
	}
	return []ai.Message{q.messages[0]}
}

func (q *pendingMessageQueue) drain() []ai.Message {
	drained := q.peek()
	q.messages = append([]ai.Message(nil), q.messages[len(drained):]...)
	return drained
}

func (q *pendingMessageQueue) clear() { q.messages = nil }

// NewAgent builds an Agent from options.
//
// pi: packages/agent/src/agent.ts:230-254 (constructor).
func NewAgent(opts AgentOptions) *Agent {
	a := &Agent{
		listeners:     make(map[uint64]func(ctx context.Context, ev AgentEvent) error),
		steeringQueue: &pendingMessageQueue{mode: QueueOneAtATime},
		followUpQueue: &pendingMessageQueue{mode: QueueOneAtATime},

		convertToLLM:        opts.ConvertToLLM,
		transformContext:    opts.TransformContext,
		streamFn:            opts.StreamFn,
		getAPIKey:           opts.GetAPIKey,
		beforeToolCall:      opts.BeforeToolCall,
		afterToolCall:       opts.AfterToolCall,
		finishTurn:          opts.FinishTurn,
		prepareRequest:      opts.PrepareRequest,
		prepareNextTurn:     opts.PrepareNextTurn,
		prepareNextTurnWith: opts.PrepareNextTurnWith,
		sessionID:           opts.SessionID,
		thinkingBudgets:     opts.ThinkingBudgets,
		maxRetries:          opts.MaxRetries,
		toolExecution:       opts.ToolExecution,
	}
	if a.streamFn == nil {
		a.streamFn = defaultStreamFn
	}
	if opts.SteeringMode != "" {
		a.steeringQueue.mode = opts.SteeringMode
	}
	if opts.FollowUpMode != "" {
		a.followUpQueue.mode = opts.FollowUpMode
	}

	var initial AgentInitialState
	if opts.InitialState != nil {
		initial = *opts.InitialState
	}

	// The leading system message is built from the seed prompt and tools
	// unless the seeded transcript already starts with one (pi:
	// createMutableAgentState, agent.ts:82-111).
	tools := append([]Tool(nil), initial.Tools...)
	messages := append([]ai.Message(nil), initial.Messages...)
	if initialMessage, ok := ai.CreateInitialSystemMessage(initial.SystemPrompt, toolDeclarations(tools)); ok {
		if len(messages) == 0 || messages[0].Role != ai.RoleSystem {
			messages = append([]ai.Message{initialMessage}, messages...)
		}
	}

	model := ai.Model{}
	if initial.Model != nil {
		model = *initial.Model
	}
	level := ThinkOff
	if initial.ThinkingLevel != nil {
		level = *initial.ThinkingLevel
	}

	a.state = mutableAgentState{
		model:            model,
		thinkingLevel:    level,
		tools:            tools,
		messages:         messages,
		pendingToolCalls: map[string]bool{},
	}
	return a
}

// defaultStreamFn resolves the provider through the registry, the way pi's
// getDefaultStreamFn does. A lookup failure is encoded in-stream, never
// returned: the StreamFn contract forbids both a nil stream and a panic.
func defaultStreamFn(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts ai.StreamOptions) ai.Stream {
	stream, err := ai.StreamRequest(ctx, model, tc, opts)
	if err != nil {
		s := ai.NewEventStream()
		msg := ai.Message{
			Role:         ai.RoleAssistant,
			API:          string(model.API),
			Provider:     string(model.Provider),
			Model:        model.ID,
			StopReason:   ai.StopError,
			ErrorMessage: err.Error(),
			TimestampMS:  nowMS(),
		}
		s.Push(ai.EvError{Reason: ai.StopError, Error: msg})
		s.End()
		return s
	}
	return stream
}

// Subscribe registers a listener for agent lifecycle events and returns its
// unsubscribe function.
//
// Listeners run in subscription order on the goroutine that emits the event
// (the run's goroutine), and are part of the run's settlement: the agent
// becomes idle only after the awaited agent_end listeners finish — the same
// rule pi documents for subscribe (agent.ts:256-269).
func (a *Agent) Subscribe(fn func(ctx context.Context, ev AgentEvent) error) func() {
	a.listenersMu.Lock()
	defer a.listenersMu.Unlock()
	a.nextID++
	id := a.nextID
	a.listeners[id] = fn
	return func() {
		a.listenersMu.Lock()
		defer a.listenersMu.Unlock()
		delete(a.listeners, id)
	}
}

// State returns a snapshot of the current agent state. Slices and the
// pending-tool-call map are copied: a caller holding the live structures
// would race the run.
//
// pi: packages/agent/src/agent.ts:276-278 (get state) with the copy
// semantics of types.ts:382-421.
func (a *Agent) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AgentState{
		SystemPrompt:     ai.GetCurrentSystemPrompt(a.state.messages),
		Model:            a.state.model,
		ThinkingLevel:    a.state.thinkingLevel,
		Tools:            append([]Tool(nil), a.state.tools...),
		Messages:         append([]ai.Message(nil), a.state.messages...),
		IsStreaming:      a.state.isStreaming,
		StreamingMessage: copyMessagePtr(a.state.streamingMessage),
		PendingToolCalls: copyBoolMap(a.state.pendingToolCalls),
		ErrorMessage:     a.state.errorMessage,
	}
}

// SteeringMode reports how queued steering messages drain.
func (a *Agent) SteeringMode() QueueMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.steeringQueue.mode
}

// SetSteeringMode controls how queued steering messages drain.
func (a *Agent) SetSteeringMode(mode QueueMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.mode = mode
}

// FollowUpMode reports how queued follow-up messages drain.
func (a *Agent) FollowUpMode() QueueMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.followUpQueue.mode
}

// SetFollowUpMode controls how queued follow-up messages drain.
func (a *Agent) SetFollowUpMode(mode QueueMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.mode = mode
}

// SetModel replaces the active model for future turns.
//
// pi mutates agent.state.model directly from the session layer
// (agent-session.ts:2509, setModel); Go has no shared single-threaded
// mutation, so the assignment takes the state lock. The caller owns the
// validation and the persistence (a model_change entry) — the agent only
// holds the value.
func (a *Agent) SetModel(model ai.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.model = model
}

// SetThinkingLevel replaces the requested reasoning level for future turns.
//
// Same shape as SetModel: pi assigns agent.state.thinkingLevel from the
// session (agent-session.ts:2645) after clamping against the model's
// available levels, which is the caller's job here.
func (a *Agent) SetThinkingLevel(level ThinkingLevel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.thinkingLevel = level
}

// SteeringMessages previews the queued steering messages without consuming
// them. P6's RPC clear_queue and get_state need the two queues separately —
// PeekQueuedMessages merges them in drain order, which cannot say which
// queue a message came from.
func (a *Agent) SteeringMessages() []ai.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ai.Message(nil), a.steeringQueue.messages...)
}

// FollowUpMessages previews the queued follow-up messages without consuming
// them (see SteeringMessages).
func (a *Agent) FollowUpMessages() []ai.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ai.Message(nil), a.followUpQueue.messages...)
}

// Steer queues a message to be injected after the current assistant turn's
// tool calls finish.
//
// pi: packages/agent/src/agent.ts:299-301.
func (a *Agent) Steer(message ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.enqueue(message)
}

// FollowUp queues a message to run only after the agent would otherwise stop.
//
// pi: packages/agent/src/agent.ts:304-306.
func (a *Agent) FollowUp(message ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.enqueue(message)
}

// ClearSteeringQueue removes all queued steering messages.
func (a *Agent) ClearSteeringQueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.clear()
}

// ClearFollowUpQueue removes all queued follow-up messages.
func (a *Agent) ClearFollowUpQueue() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue.clear()
}

// ClearAllQueues removes every queued steering and follow-up message.
func (a *Agent) ClearAllQueues() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQueue.clear()
	a.followUpQueue.clear()
}

// HasQueuedMessages reports whether either queue still holds messages.
func (a *Agent) HasQueuedMessages() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.steeringQueue.hasItems() || a.followUpQueue.hasItems()
}

// PeekQueuedMessages previews the messages selected for the next turn
// without consuming them: steering first, follow-up only when steering is
// empty.
//
// pi: packages/agent/src/agent.ts:330-333.
func (a *Agent) PeekQueuedMessages() []ai.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	if steering := a.steeringQueue.peek(); len(steering) > 0 {
		return steering
	}
	return a.followUpQueue.peek()
}

// Abort cancels the current run, if one is active. In-flight tool calls
// observe the cancellation through their context.
//
// pi: packages/agent/src/agent.ts:341-343.
func (a *Agent) Abort() {
	a.mu.Lock()
	run := a.activeRun
	a.mu.Unlock()
	if run != nil {
		run.cancel()
	}
}

// WaitForIdle blocks until the current run and all awaited event listeners
// have finished, or ctx is done. It returns nil when no run is active.
//
// pi: packages/agent/src/agent.ts:350-352 (waitForIdle).
func (a *Agent) WaitForIdle(ctx context.Context) error {
	a.mu.Lock()
	run := a.activeRun
	a.mu.Unlock()
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reset clears conversation state and queues while retaining the replayed
// prompt/tool baseline (the current system message).
//
// pi: packages/agent/src/agent.ts:355-368.
func (a *Agent) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeRun != nil {
		return errAlreadyProcessing
	}
	baseline, ok := ai.GetCurrentSystemMessage(a.state.messages)
	if ok {
		a.state.messages = []ai.Message{baseline}
	} else {
		a.state.messages = nil
	}
	a.state.isStreaming = false
	a.state.streamingMessage = nil
	a.state.pendingToolCalls = map[string]bool{}
	a.state.errorMessage = ""
	a.steeringQueue.clear()
	a.followUpQueue.clear()
	return nil
}

// Prompt starts a new prompt run from one or more messages.
//
// Run-level failures (stream errors, aborts) are not returned here: they are
// surfaced as events and as State().ErrorMessage, exactly as pi's promise
// rejection is swallowed by runWithLifecycle's catch (agent.ts:523-530). The
// returned error is for programming mistakes: a concurrent run, or a nil
// message list.
//
// pi: packages/agent/src/agent.ts:371-381.
func (a *Agent) Prompt(ctx context.Context, messages ...ai.Message) error {
	if len(messages) == 0 {
		return errors.New("agent: prompt requires at least one message")
	}
	return a.runPromptMessages(ctx, messages, false)
}

// Continue continues from the current transcript. The last message must be a
// user or tool-result message; when it is an assistant message, queued
// steering or follow-up messages are run as a new prompt instead.
//
// pi: packages/agent/src/agent.ts:384-411.
func (a *Agent) Continue(ctx context.Context) error {
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		return errAlreadyProcessing
	}
	messages := a.state.messages
	onlySystem := true
	for _, m := range messages {
		if m.Role != ai.RoleSystem {
			onlySystem = false
			break
		}
	}
	if len(messages) == 0 || onlySystem {
		a.mu.Unlock()
		return errors.New("agent: no messages to continue from")
	}
	last := messages[len(messages)-1]
	if last.Role == ai.RoleAssistant {
		if queued := a.steeringQueue.drain(); len(queued) > 0 {
			a.mu.Unlock()
			// The queue was just drained, so the loop must not poll it
			// again at start — one-at-a-time mode would otherwise deliver
			// a second message in this turn.
			return a.runPromptMessages(ctx, queued, true)
		}
		if queued := a.followUpQueue.drain(); len(queued) > 0 {
			a.mu.Unlock()
			return a.runPromptMessages(ctx, queued, false)
		}
		a.mu.Unlock()
		return errors.New("agent: cannot continue from message role: assistant")
	}
	snapshot := a.contextSnapshotLocked()
	config := a.loopConfigLocked(nil)
	a.mu.Unlock()

	return a.runWithLifecycle(ctx, func(runCtx context.Context) error {
		_, err := RunAgentLoopContinue(runCtx, snapshot, config, a.runSink(runCtx), a.streamFn)
		return err
	})
}

// runPromptMessages runs a prompt through the full lifecycle.
func (a *Agent) runPromptMessages(ctx context.Context, messages []ai.Message, skipInitialSteeringPoll bool) error {
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		return errAlreadyProcessing
	}
	snapshot := a.contextSnapshotLocked()
	var skip *bool
	if skipInitialSteeringPoll {
		skip = &skipInitialSteeringPoll
	}
	config := a.loopConfigLocked(skip)
	a.mu.Unlock()

	return a.runWithLifecycle(ctx, func(runCtx context.Context) error {
		_, err := RunAgentLoop(runCtx, messages, snapshot, config, a.runSink(runCtx), a.streamFn)
		return err
	})
}

// contextSnapshotLocked copies the transcript and tool set for a run. The
// caller must hold a.mu.
func (a *Agent) contextSnapshotLocked() AgentContext {
	return AgentContext{
		Messages: append([]ai.Message(nil), a.state.messages...),
		Tools:    append([]Tool(nil), a.state.tools...),
	}
}

// loopConfigLocked builds the loop config from the agent's configuration and
// current state. The caller must hold a.mu.
//
// pi: packages/agent/src/agent.ts:467-505 (createLoopConfig).
func (a *Agent) loopConfigLocked(skipInitialSteeringPoll *bool) AgentLoopConfig {
	config := AgentLoopConfig{
		StreamOptions: ai.StreamOptions{
			Reasoning:       streamThinkingLevel(a.state.thinkingLevel),
			SessionID:       a.sessionID,
			ThinkingBudgets: a.thinkingBudgets,
			MaxRetries:      a.maxRetries,
		},
		Model:            a.state.model,
		ConvertToLLM:     a.convertToLLM,
		TransformContext: a.transformContext,
		GetAPIKey:        a.getAPIKey,
		FinishTurn:       a.finishTurn,
		PrepareRequest:   a.prepareRequest,
		ToolExecution:    a.toolExecution,
		BeforeToolCall:   a.beforeToolCall,
		AfterToolCall:    a.afterToolCall,
	}

	// pi composes the two prepareNextTurn variants into the loop's single
	// context-taking hook; the plain variant simply ignores the context.
	if a.prepareNextTurnWith != nil || a.prepareNextTurn != nil {
		withContext := a.prepareNextTurnWith
		plain := a.prepareNextTurn
		config.PrepareNextTurn = func(ctx context.Context, turn PrepareNextTurnContext) AgentLoopTurnUpdate {
			if withContext != nil {
				if update := withContext(ctx, turn); update != nil {
					return *update
				}
				return AgentLoopTurnUpdate{}
			}
			if update := plain(ctx); update != nil {
				return *update
			}
			return AgentLoopTurnUpdate{}
		}
	}

	// The queues are drained through the config hooks. skip is captured by
	// reference and consumed by the first call, exactly like pi's mutable
	// closure variable.
	skip := skipInitialSteeringPoll != nil && *skipInitialSteeringPoll
	config.GetSteeringMessages = func(ctx context.Context) []ai.Message {
		a.mu.Lock()
		defer a.mu.Unlock()
		if skip {
			skip = false
			return nil
		}
		return a.steeringQueue.drain()
	}
	config.GetFollowUpMessages = func(ctx context.Context) []ai.Message {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.followUpQueue.drain()
	}
	return config
}

// runSink binds the run's context into the Agent's event reducer.
func (a *Agent) runSink(ctx context.Context) EventSink {
	return func(ev AgentEvent) error {
		return a.processEvents(ctx, ev)
	}
}

// runWithLifecycle runs one prompt or continuation under the run lifecycle:
// exactly one active run at a time, streaming state set for its duration, and
// the run's completion signaled after the executor returns.
//
// An executor error is not returned: it is reported through the synthetic
// failure events and State().ErrorMessage, exactly as pi's promise rejection
// is swallowed by runWithLifecycle's catch (agent.ts:523-530). The error
// return of this method is for programming mistakes — a concurrent run.
//
// pi: packages/agent/src/agent.ts:507-530.
func (a *Agent) runWithLifecycle(ctx context.Context, executor func(runCtx context.Context) error) error {
	a.mu.Lock()
	if a.activeRun != nil {
		a.mu.Unlock()
		return errAlreadyProcessing
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &activeRun{done: make(chan struct{}), cancel: cancel}
	a.activeRun = run
	a.state.isStreaming = true
	a.state.streamingMessage = nil
	a.state.errorMessage = ""
	a.mu.Unlock()

	func() {
		defer close(run.done)
		defer func() {
			a.mu.Lock()
			a.state.isStreaming = false
			a.state.streamingMessage = nil
			a.state.pendingToolCalls = map[string]bool{}
			a.activeRun = nil
			a.mu.Unlock()
		}()
		if err := executor(runCtx); err != nil {
			a.handleRunFailure(runCtx, err)
		}
	}()
	cancel() // release the derived context's resources
	return nil
}

// handleRunFailure synthesizes the failure sequence for a run that threw:
// an assistant message carrying the error, then message_start/message_end,
// turn_end, and agent_end.
//
// The emits are best-effort: the original failure is the story, and a
// subscriber erroring on the synthetic events must not replace it.
//
// pi: packages/agent/src/agent.ts:532-548. Note pi's agent_end carries only
// the failure message, not the whole transcript; that is mirrored here.
func (a *Agent) handleRunFailure(ctx context.Context, runErr error) {
	a.mu.Lock()
	model := a.state.model
	a.mu.Unlock()

	stop := ai.StopError
	if ctx.Err() != nil {
		stop = ai.StopAborted
	}
	failure := ai.Message{
		Role:         ai.RoleAssistant,
		Content:      []ai.ContentBlock{ai.TextBlock("")},
		API:          string(model.API),
		Provider:     string(model.Provider),
		Model:        model.ID,
		Usage:        &ai.Usage{},
		StopReason:   stop,
		ErrorMessage: runErr.Error(),
		TimestampMS:  nowMS(),
	}
	_ = a.processEvents(ctx, EvMessageStart{Message: failure})
	_ = a.processEvents(ctx, EvMessageEnd{Message: failure})
	_ = a.processEvents(ctx, EvTurnEnd{Message: failure, ToolResults: nil})
	_ = a.processEvents(ctx, EvAgentEnd{Messages: []ai.Message{failure}})
	_ = a.processEvents(ctx, EvAgentSettled{Aborted: stop == ai.StopAborted})
}

// processEvents reduces internal state for one loop event, then awaits the
// subscribers.
//
// State reduction happens before the listeners run, so a listener calling
// State() sees the event already applied — the same ordering pi gets by
// reducing first and awaiting after (agent.ts:565-612). The first listener
// error is returned; remaining listeners still run, so one bad subscriber
// cannot silently suppress the others.
//
// agent_end only means no further loop events will be emitted: the run is
// considered idle later, after the awaited listeners finish and the run's
// deferred cleanup clears the runtime-owned state.
func (a *Agent) processEvents(ctx context.Context, ev AgentEvent) error {
	a.mu.Lock()
	switch e := ev.(type) {
	case EvMessageStart:
		a.state.streamingMessage = copyMessagePtr(&e.Message)
	case EvMessageUpdate:
		a.state.streamingMessage = copyMessagePtr(&e.Message)
	case EvMessageEnd:
		a.state.streamingMessage = nil
		a.state.messages = append(a.state.messages, e.Message)
	case EvToolExecutionStart:
		if a.state.pendingToolCalls == nil {
			a.state.pendingToolCalls = map[string]bool{}
		}
		a.state.pendingToolCalls[e.ToolCallID] = true
	case EvToolExecutionEnd:
		delete(a.state.pendingToolCalls, e.ToolCallID)
	case EvTurnEnd:
		if e.Message.Role == ai.RoleAssistant && e.Message.ErrorMessage != "" {
			a.state.errorMessage = e.Message.ErrorMessage
		}
	case EvAgentEnd:
		a.state.streamingMessage = nil
	}
	a.mu.Unlock()

	a.listenersMu.Lock()
	listeners := make([]func(ctx context.Context, ev AgentEvent) error, 0, len(a.listeners))
	for _, ln := range a.listeners {
		listeners = append(listeners, ln)
	}
	a.listenersMu.Unlock()

	var firstErr error
	for _, ln := range listeners {
		if err := ln(ctx, ev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// --- copy helpers -----------------------------------------------------------
//
// pi copies on assignment through accessor properties (types.ts:382-421); Go
// has no accessors, so the copies happen at the snapshot boundaries.

func copyMessagePtr(m *ai.Message) *ai.Message {
	if m == nil {
		return nil
	}
	clone := *m
	clone.Content = append([]ai.ContentBlock(nil), m.Content...)
	return &clone
}

func copyBoolMap(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
