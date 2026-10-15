package agent

// The agent loop: the Go transcription of pi's packages/agent/src/agent-loop.ts.
//
// Three entry points, mirroring pi's exports:
//
//	RunAgentLoop          — agentLoop(prompts, …): a new prompt run
//	RunAgentLoopContinue  — agentLoopContinue(context, …): continue from a transcript
//	RunToolCall           — runToolCall(…): one call through the full preflight/
//	                        execute/finalize path without events, for tools that
//	                        call other tools
//
// Everything else in this file is the machinery those three share: runLoop (the
// turn/steering/follow-up scheduler), streamAssistantResponse (the LLM boundary
// where AgentMessages become provider messages), declareToolChanges (the tool
// loadout announcement), and the tool-batch executors in toolcall.go.
//
// # Shape decisions (Go vs TypeScript)
//
//   - pi's hooks are async and its emit sink is async; here every hook and the
//     emit sink are synchronous and return an error. A hook error or an emit
//     error aborts the run and propagates to the caller, which plays the role
//     of pi's rejected promise reaching Agent.runWithLifecycle's catch.
//   - pi mutates the config object across turns (model/reasoning replacement);
//     here config is a value copied at entry and mutated locally, exactly like
//     pi's `config = {...config, …}` reassignments.
//   - AbortSignal becomes context.Context; every `signal?.aborted` check below
//     is a ctx.Err() check in the same position.
//   - pi's EventStream return value (events + final messages) is not needed at
//     this layer: the caller already receives every event through emit and the
//     final message list as the return value, so the loop returns
//     ([]AgentMessage, error) directly.

import (
	"context"
	"fmt"
	"time"

	"ultrac/internal/ai"
)

// EventSink receives every event the loop emits. It must not be called
// concurrently: the loop calls it from one goroutine only (worker-originated
// events are marshalled through a queue first — see toolcall.go).
//
// A non-nil error aborts the run; the loop stops emitting and returns it.
type EventSink func(ev AgentEvent) error

// RunAgentLoop starts an agent loop with new prompt messages.
//
// The prompts are added to the context and get message_start/message_end
// events of their own, before the first turn. It returns every message the run
// produced, prompts included.
//
// pi: packages/agent/src/agent-loop.ts:38-61 (agentLoop) and :102-126
// (runAgentLoop).
func RunAgentLoop(
	ctx context.Context,
	prompts []AgentMessage,
	context AgentContext,
	config AgentLoopConfig,
	emit EventSink,
	streamFn StreamFn,
) ([]AgentMessage, error) {
	initialMessages := declareToolChanges(context, prompts)
	newMessages := make([]AgentMessage, 0, len(context.Messages)+len(initialMessages))
	newMessages = append(newMessages, initialMessages...)

	merged := make([]ai.Message, 0, len(context.Messages)+len(initialMessages))
	merged = append(merged, context.Messages...)
	merged = append(merged, initialMessages...)
	currentContext := AgentContext{Messages: merged, Tools: context.Tools}

	if err := emit(EvAgentStart{}); err != nil {
		return newMessages, err
	}
	if err := emit(EvTurnStart{}); err != nil {
		return newMessages, err
	}
	for _, message := range initialMessages {
		if err := emit(EvMessageStart{Message: message}); err != nil {
			return newMessages, err
		}
		if err := emit(EvMessageEnd{Message: message}); err != nil {
			return newMessages, err
		}
	}

	return runLoop(ctx, currentContext, newMessages, config, emit, streamFn)
}

// RunAgentLoopContinue continues an agent loop from the current context
// without adding a new message. Used for retries — the context already has a
// user message or tool results.
//
// The last message must convert to a user or toolResult message via
// convertToLlm; that cannot be validated here because convertToLlm only runs
// once per turn (pi says the same, agent-loop.ts:63-70).
//
// pi: packages/agent/src/agent-loop.ts:71-100 (agentLoopContinue) and :128-151
// (runAgentLoopContinue).
func RunAgentLoopContinue(
	ctx context.Context,
	context AgentContext,
	config AgentLoopConfig,
	emit EventSink,
	streamFn StreamFn,
) ([]AgentMessage, error) {
	if len(context.Messages) == 0 {
		return nil, fmt.Errorf("agent: cannot continue: no messages in context")
	}
	if context.Messages[len(context.Messages)-1].Role == ai.RoleAssistant {
		return nil, fmt.Errorf("agent: cannot continue from message role: assistant")
	}

	newMessages := []AgentMessage{}
	currentContext := AgentContext{Messages: context.Messages, Tools: context.Tools}

	if err := emit(EvAgentStart{}); err != nil {
		return newMessages, err
	}
	if err := emit(EvTurnStart{}); err != nil {
		return newMessages, err
	}

	return runLoop(ctx, currentContext, newMessages, config, emit, streamFn)
}

// runLoop is the main loop logic shared by both entry points.
//
// Structure (pi: agent-loop.ts:163-321):
//
//	outer loop — re-entered when follow-up messages arrive after the agent
//	             would stop, or when finishTurn asked for one more turn
//	  inner loop — one assistant response plus its tool calls, plus any
//	               steering/prepared messages queued for the next request
//
// The inner loop's exit condition is the subtle part: it continues while there
// are more tool calls to run (the batch did not terminate) OR while messages
// are pending. Steering messages are polled before every request, including
// the first — the user may have typed while waiting.
func runLoop(
	ctx context.Context,
	currentContext AgentContext,
	newMessages []AgentMessage,
	config AgentLoopConfig,
	emit EventSink,
	streamFn StreamFn,
) ([]AgentMessage, error) {
	var lastCompletedTurn *PrepareNextTurnContext
	explicitContinuation := false
	// Check for steering messages at start (user may have typed while waiting).
	pendingMessages := pollSteering(ctx, config)

	for {
		hasMoreToolCalls := true

		for hasMoreToolCalls || len(pendingMessages) > 0 {
			var preparedMessages []AgentMessage
			if lastCompletedTurn != nil {
				if update := prepareNextTurn(ctx, config, *lastCompletedTurn); update != nil {
					if update.Context != nil {
						currentContext = *update.Context
					}
					preparedMessages = update.Messages
					config = withModelAndThinking(config, update.Model, update.ThinkingLevel)
				}
				// Preparation can be long-running (for example, compaction).
				// Pick up steering queued while it ran. Only poll again if the
				// earlier poll returned nothing; otherwise one-at-a-time mode
				// would deliver two messages in this turn.
				if len(pendingMessages) == 0 {
					pendingMessages = pollSteering(ctx, config)
				}
				if err := emit(EvTurnStart{}); err != nil {
					return newMessages, err
				}
			}

			// Process prepared and queued messages before the next assistant
			// response. declareToolChanges may prepend/patch a system message
			// announcing the tool loadout delta.
			for _, message := range declareToolChanges(currentContext, concat(preparedMessages, pendingMessages)) {
				if err := emit(EvMessageStart{Message: message}); err != nil {
					return newMessages, err
				}
				if err := emit(EvMessageEnd{Message: message}); err != nil {
					return newMessages, err
				}
				currentContext.Messages = append(currentContext.Messages, message)
				newMessages = append(newMessages, message)
			}
			pendingMessages = nil

			if update := prepareRequest(ctx, config, currentContext); update != nil {
				if update.Context != nil {
					currentContext = *update.Context
				}
				config = withModelAndThinking(config, update.Model, update.ThinkingLevel)
			}

			// Stream assistant response.
			message, err := streamAssistantResponse(ctx, &currentContext, config, emit, streamFn)
			if err != nil {
				return newMessages, err
			}
			newMessages = append(newMessages, message)

			if message.StopReason == ai.StopError || message.StopReason == ai.StopAborted {
				// Error and aborted responses are hard exits regardless of
				// what finishTurn says (pi: agent-loop.ts:245-256).
				turn := AgentTurnContext{
					Message:     message,
					ToolResults: nil,
					Context:     currentContext,
					NewMessages: newMessages,
				}
				if config.FinishTurn != nil {
					config.FinishTurn(ctx, turn)
				}
				if err := emit(EvTurnEnd{Message: message, ToolResults: nil}); err != nil {
					return newMessages, err
				}
				if err := emit(EvAgentEnd{Messages: newMessages}); err != nil {
					return newMessages, err
				}
				return newMessages, nil
			}

			toolCalls := toolCallsOf(message)

			var toolResults []ai.Message
			hasMoreToolCalls = false
			if len(toolCalls) > 0 {
				var batch executedToolBatch
				// A "length" stop means the output was cut off by the token
				// limit, so every tool call in the message may carry truncated
				// arguments. Fail them all instead of executing potentially
				// borked calls (pi: agent-loop.ts:264-270).
				if message.StopReason == ai.StopLength {
					batch = failToolCallsFromTruncatedMessage(ctx, toolCalls, emit)
				} else {
					var err error
					batch, err = executeToolCalls(ctx, currentContext, message, config, emit)
					if err != nil {
						return newMessages, err
					}
				}
				toolResults = batch.messages
				hasMoreToolCalls = !batch.terminate

				for _, result := range toolResults {
					currentContext.Messages = append(currentContext.Messages, result)
					newMessages = append(newMessages, result)
				}
			}

			lastCompletedTurn = &PrepareNextTurnContext{
				Message:     message,
				ToolResults: toolResults,
				Context:     currentContext,
				NewMessages: newMessages,
			}
			decision := TurnDefault
			if config.FinishTurn != nil {
				decision = config.FinishTurn(ctx, *lastCompletedTurn)
			}
			if err := emit(EvTurnEnd{Message: message, ToolResults: toolResults}); err != nil {
				return newMessages, err
			}

			if decision == TurnEnd {
				if err := emit(EvAgentEnd{Messages: newMessages}); err != nil {
					return newMessages, err
				}
				return newMessages, nil
			}

			explicitContinuation = decision == TurnContinue
			pendingMessages = pollSteering(ctx, config)
			if hasMoreToolCalls || len(pendingMessages) > 0 {
				explicitContinuation = false
			}
		}

		// Agent would stop here. Check for follow-up messages.
		followUpMessages := pollFollowUp(ctx, config)
		if len(followUpMessages) > 0 {
			// Set as pending so the inner loop processes them.
			explicitContinuation = false
			pendingMessages = followUpMessages
			continue
		}

		// No natural request was selected, so fulfill the continuation
		// decision with one context-only turn.
		if explicitContinuation {
			explicitContinuation = false
			continue
		}

		// No more messages, exit.
		break
	}

	if err := emit(EvAgentEnd{Messages: newMessages}); err != nil {
		return newMessages, err
	}
	return newMessages, nil
}

// streamAssistantResponse streams one assistant response from the LLM. This is
// where AgentMessage[] gets transformed to provider messages.
//
// pi: packages/agent/src/agent-loop.ts:381-469.
//
// The partial message is pushed into the live context on `start` and replaced
// in place on every update, so a consumer reading the context mid-stream sees
// the response so far — the same mutable-snapshot semantics as pi.
func streamAssistantResponse(
	ctx context.Context,
	context *AgentContext,
	config AgentLoopConfig,
	emit EventSink,
	streamFn StreamFn,
) (ai.Message, error) {
	// Apply context transform if configured (AgentMessage[] → AgentMessage[]).
	messages := context.Messages
	if config.TransformContext != nil {
		messages = config.TransformContext(ctx, messages)
	}

	// Convert to LLM-compatible messages (AgentMessage[] → Message[]).
	var llmMessages []ai.Message
	if config.ConvertToLLM != nil {
		llmMessages = config.ConvertToLLM(messages)
	} else {
		llmMessages = DefaultConvertToLLM(messages)
	}

	llmContext := ai.NormalizeContext(ai.Context{Messages: llmMessages})

	// Resolve API key (important for expiring tokens).
	resolvedAPIKey := config.APIKey
	if config.GetAPIKey != nil {
		if key := config.GetAPIKey(string(config.Model.Provider)); key != "" {
			resolvedAPIKey = key
		}
	}

	opts := config.StreamOptions
	opts.APIKey = resolvedAPIKey

	stream := streamFn(ctx, config.Model, llmContext, opts)
	if stream == nil {
		return ai.Message{}, fmt.Errorf("agent: stream function returned a nil stream for model %s/%s", config.Model.Provider, config.Model.ID)
	}

	// Record the requested level, whichever stream function answered.
	finalize := func() (ai.Message, error) {
		msg, err := stream.Result()
		if err != nil {
			return ai.Message{}, err
		}
		msg.ThinkingLevel = string(agentThinkingLevel(config.Reasoning))
		return msg, nil
	}

	var partial *ai.Message
	addedPartial := false

	for ev, err := range stream.Events() {
		if err != nil {
			// The Stream contract encodes failures as EvError, so an error in
			// the sequence is a protocol violation. Surface it rather than
			// pretending the stream ended cleanly.
			return ai.Message{}, err
		}
		switch e := ev.(type) {
		case ai.EvStart:
			m := e.Partial
			partial = &m
			context.Messages = append(context.Messages, m)
			addedPartial = true
			if err := emit(EvMessageStart{Message: m}); err != nil {
				return ai.Message{}, err
			}

		case ai.EvTextStart, ai.EvTextDelta, ai.EvTextEnd,
			ai.EvThinkingStart, ai.EvThinkingDelta, ai.EvThinkingEnd,
			ai.EvToolCallStart, ai.EvToolCallDelta, ai.EvToolCallEnd:
			if partial != nil {
				m, ok := eventPartial(ev)
				if !ok {
					return ai.Message{}, fmt.Errorf("agent: update event %s carries no partial message", ai.Name(ev))
				}
				partial = &m
				if len(context.Messages) > 0 {
					context.Messages[len(context.Messages)-1] = m
				}
				if err := emit(EvMessageUpdate{Message: m, AssistantEvent: ev}); err != nil {
					return ai.Message{}, err
				}
			}

		case ai.EvDone, ai.EvError:
			return finalizeStream(context, addedPartial, finalize, emit)
		}
	}

	// The stream ended without a terminal event. Result reports that as an
	// error; the caller (Agent) turns it into a failed run.
	return finalizeStream(context, addedPartial, finalize, emit)
}

// finalizeStream closes the streamed message: replaces the live partial with
// the authoritative final message (or appends it when no partial was ever
// pushed), emits the missing message_start when there was none, and emits
// message_end.
//
// pi: agent-loop.ts:443-456 and :460-468 — the done/error branch and the
// post-loop tail are the same code, so they share this helper.
func finalizeStream(
	context *AgentContext,
	addedPartial bool,
	finalize func() (ai.Message, error),
	emit EventSink,
) (ai.Message, error) {
	final, err := finalize()
	if err != nil {
		return ai.Message{}, err
	}
	if addedPartial && len(context.Messages) > 0 {
		context.Messages[len(context.Messages)-1] = final
	} else {
		context.Messages = append(context.Messages, final)
	}
	if !addedPartial {
		if err := emit(EvMessageStart{Message: final}); err != nil {
			return ai.Message{}, err
		}
	}
	if err := emit(EvMessageEnd{Message: final}); err != nil {
		return ai.Message{}, err
	}
	return final, nil
}

// eventPartial extracts the cumulative partial from one of the nine update
// events. Go's type switch cannot share a case body across concrete types
// while keeping the concrete field access, so the nine cases live here.
func eventPartial(ev ai.AssistantEvent) (ai.Message, bool) {
	switch e := ev.(type) {
	case ai.EvTextStart:
		return e.Partial, true
	case ai.EvTextDelta:
		return e.Partial, true
	case ai.EvTextEnd:
		return e.Partial, true
	case ai.EvThinkingStart:
		return e.Partial, true
	case ai.EvThinkingDelta:
		return e.Partial, true
	case ai.EvThinkingEnd:
		return e.Partial, true
	case ai.EvToolCallStart:
		return e.Partial, true
	case ai.EvToolCallDelta:
		return e.Partial, true
	case ai.EvToolCallEnd:
		return e.Partial, true
	default:
		return ai.Message{}, false
	}
}

// declareToolChanges declares tool loadout changes to the model.
//
// context.tools is what the runtime can execute; the transcript's system
// messages declare what the model may call. Before each request the difference
// becomes toolsAdded and toolsRemoved on a system message. When a pending
// system message exists, its tool fields are treated as intent and replaced
// with the delta between the committed transcript and the executable set, so
// replay always yields exactly context.tools. Otherwise a new system message is
// inserted before the first non-system pending message.
//
// pi: packages/agent/src/agent-loop.ts:333-363.
func declareToolChanges(context AgentContext, pendingMessages []AgentMessage) []AgentMessage {
	systemIndex := -1
	for i := len(pendingMessages) - 1; i >= 0; i-- {
		if pendingMessages[i].Role == ai.RoleSystem {
			systemIndex = i
			break
		}
	}
	var pending *AgentMessage
	if systemIndex >= 0 {
		pending = &pendingMessages[systemIndex]
	}

	// The baseline is what the transcript declares with the pending system
	// message's tool intent stripped: intent is replaced, never accumulated.
	baseline := pendingMessages
	if pending != nil {
		baseline = make([]AgentMessage, len(pendingMessages))
		copy(baseline, pendingMessages)
		baseline[systemIndex] = withToolChanges(*pending, nil, nil)
	}

	declared := make([]ai.Message, 0, len(context.Messages)+len(baseline))
	declared = append(declared, context.Messages...)
	declared = append(declared, baseline...)

	added, removed := ai.GetToolStateChanges(ai.GetCurrentTools(declared), toolDeclarations(context.Tools))
	unchanged := len(added) == 0 && len(removed) == 0

	if pending != nil {
		// Keep the caller's message object when it already declares no tool
		// changes.
		if unchanged && len(pending.ToolsAdded) == 0 && len(pending.ToolsRemoved) == 0 {
			return pendingMessages
		}
		out := make([]AgentMessage, len(baseline))
		copy(out, baseline)
		out[systemIndex] = withToolChanges(*pending, added, removed)
		return out
	}
	if unchanged {
		return pendingMessages
	}

	update := withToolChanges(AgentMessage{
		Role:        ai.RoleSystem,
		Content:     []ai.ContentBlock{ai.TextBlock("")},
		TimestampMS: nowMS(),
	}, added, removed)
	insertIndex := len(pendingMessages)
	for i, message := range pendingMessages {
		if message.Role != ai.RoleSystem {
			insertIndex = i
			break
		}
	}
	out := make([]AgentMessage, 0, len(pendingMessages)+1)
	out = append(out, pendingMessages[:insertIndex]...)
	out = append(out, update)
	out = append(out, pendingMessages[insertIndex:]...)
	return out
}

// withToolChanges copies a system message with its tool fields replaced by the
// given delta; empty lists omit the field (pi: agent-loop.ts:368-375).
func withToolChanges(message AgentMessage, added []ai.Tool, removed []ai.ToolReference) AgentMessage {
	message.ToolsAdded = nil
	message.ToolsRemoved = nil
	if len(added) > 0 {
		message.ToolsAdded = added
	}
	if len(removed) > 0 {
		message.ToolsRemoved = removed
	}
	return message
}

// failToolCallsFromTruncatedMessage fails all tool calls from an assistant
// message that was truncated by the output token limit.
//
// Streamed tool-call arguments are finalized with a best-effort JSON salvage
// parser, so a truncated message can yield tool calls whose arguments parse
// and validate but are silently incomplete. None of them are safe to execute;
// report each as an error so the model can re-issue them.
//
// pi: packages/agent/src/agent-loop.ts:478-503.
func failToolCallsFromTruncatedMessage(ctx context.Context, toolCalls []ai.ContentBlock, emit EventSink) executedToolBatch {
	messages := make([]ai.Message, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		if err := emit(EvToolExecutionStart{
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Name,
			Args:       toolCall.Arguments,
		}); err != nil {
			return executedToolBatch{messages: messages}
		}
		finalized := ToolOutcome{
			ToolCall: toolCall,
			Result: createErrorToolResult(fmt.Sprintf(
				"Tool call %q was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.",
				toolCall.Name)),
			IsError: true,
		}
		if err := emitToolExecutionEnd(finalized, emit); err != nil {
			return executedToolBatch{messages: messages}
		}
		msg := createToolResultMessage(finalized)
		if err := emit(EvMessageStart{Message: msg}); err != nil {
			return executedToolBatch{messages: messages}
		}
		if err := emit(EvMessageEnd{Message: msg}); err != nil {
			return executedToolBatch{messages: messages}
		}
		messages = append(messages, msg)
	}
	return executedToolBatch{messages: messages, terminate: false}
}

// --- small shared helpers -------------------------------------------------

// DefaultConvertToLLM is the default conversion: keep the four roles the LLM
// protocols understand, drop everything else.
//
// pi: packages/agent/src/agent.ts:38-46 (defaultConvertToLlm).
func DefaultConvertToLLM(messages []AgentMessage) []ai.Message {
	out := make([]ai.Message, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
			out = append(out, message)
		}
	}
	return out
}

// toolCallsOf returns the tool-call blocks of an assistant message, in source
// order.
func toolCallsOf(message ai.Message) []ai.ContentBlock {
	var calls []ai.ContentBlock
	for _, block := range message.Content {
		if block.Type == ai.BlockToolCall {
			calls = append(calls, block)
		}
	}
	return calls
}

// findTool resolves a tool by name in a context's tool set, as pi's
// tools.find does: first match wins.
func findTool(tools []Tool, name string) Tool {
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// toolDeclarations projects the runtime tools into the declarations the model
// sees (pi's toToolDeclaration).
func toolDeclarations(tools []Tool) []ai.Tool {
	out := make([]ai.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, ai.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return out
}

// concat returns a+b without mutating either.
func concat(a, b []AgentMessage) []AgentMessage {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]AgentMessage, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

// pollSteering drains the steering queue through the config hook. A missing
// hook means nothing is ever queued.
func pollSteering(ctx context.Context, config AgentLoopConfig) []AgentMessage {
	if config.GetSteeringMessages == nil {
		return nil
	}
	return config.GetSteeringMessages(ctx)
}

// pollFollowUp drains the follow-up queue through the config hook.
func pollFollowUp(ctx context.Context, config AgentLoopConfig) []AgentMessage {
	if config.GetFollowUpMessages == nil {
		return nil
	}
	return config.GetFollowUpMessages(ctx)
}

// prepareRequest runs the prepareRequest hook, nil-safe.
func prepareRequest(ctx context.Context, config AgentLoopConfig, currentContext AgentContext) *AgentRequestUpdate {
	if config.PrepareRequest == nil {
		return nil
	}
	update := config.PrepareRequest(ctx, PrepareRequestContext{
		Context:       currentContext,
		Model:         config.Model,
		ThinkingLevel: agentThinkingLevel(config.Reasoning),
	})
	return &update
}

// prepareNextTurn runs the prepareNextTurn hook, nil-safe.
func prepareNextTurn(ctx context.Context, config AgentLoopConfig, turn PrepareNextTurnContext) *AgentLoopTurnUpdate {
	if config.PrepareNextTurn == nil {
		return nil
	}
	update := config.PrepareNextTurn(ctx, turn)
	return &update
}

// withModelAndThinking applies an update's model/thinkingLevel replacement,
// mirroring pi's inline config spread: an absent field keeps the current
// value, and thinkingLevel "off" maps back to the unset stream option.
func withModelAndThinking(config AgentLoopConfig, model *ai.Model, level *ThinkingLevel) AgentLoopConfig {
	if model != nil {
		config.Model = *model
	}
	if level != nil {
		config.Reasoning = streamThinkingLevel(*level)
	}
	return config
}

// agentThinkingLevel maps the stream option back to the agent-level level:
// unset means "off" (pi: config.reasoning ?? "off").
func agentThinkingLevel(level ai.ThinkingLevel) ThinkingLevel {
	if level == "" {
		return ThinkOff
	}
	return ThinkingLevel(level)
}

// streamThinkingLevel maps the agent-level level to the stream option: "off"
// becomes unset, because ai.ThinkingLevel has no "off" member (pi keeps
// reasoning undefined when thinking is off).
func streamThinkingLevel(level ThinkingLevel) ai.ThinkingLevel {
	if level == ThinkOff || level == "" {
		return ""
	}
	return ai.ThinkingLevel(level)
}

// nowMS is the wall clock in milliseconds, for message timestamps.
func nowMS() int64 { return time.Now().UnixMilli() }
