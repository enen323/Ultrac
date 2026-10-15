package agent

// Test fakes for the P2 loop and Agent tests.
//
// The whole P2 suite runs against a fake provider serving preset event
// scripts — pi tests its own loop the same way, and the plan's P2 gate
// requires no real API calls. The scripts below produce the 12-event
// protocol in the shapes real adapters emit: a growing `partial` per update
// and an authoritative final message on done.

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ultrac/internal/ai"
)

// fakeProvider is an ai.StreamFn serving one preset script per call.
type fakeProvider struct {
	mu       sync.Mutex
	scripts  [][]ai.AssistantEvent
	models   []ai.Model
	contexts []ai.TranscriptContext
	opts     []ai.StreamOptions

	// gate, when non-nil, blocks provider calls until a token arrives (or
	// the context is done). gateAt selects which call blocks: 0 means every
	// call, otherwise only the 1-based call index gateAt. Used to hold a run
	// mid-flight for steering tests.
	gate   chan struct{}
	gateAt int
}

// callCount reports how many provider requests were made.
func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.contexts)
}

// transcript returns the normalized transcript of call i.
func (f *fakeProvider) transcript(i int) []ai.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.contexts) {
		return nil
	}
	return f.contexts[i].Messages
}

// model returns the model of call i.
func (f *fakeProvider) model(i int) ai.Model {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.models) {
		return ai.Model{}
	}
	return f.models[i]
}

// options returns the stream options of call i.
func (f *fakeProvider) options(i int) ai.StreamOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.opts) {
		return ai.StreamOptions{}
	}
	return f.opts[i]
}

func (f *fakeProvider) streamFn(ctx context.Context, model ai.Model, tc ai.TranscriptContext, opts ai.StreamOptions) ai.Stream {
	f.mu.Lock()
	idx := len(f.contexts)
	f.contexts = append(f.contexts, tc)
	f.models = append(f.models, model)
	f.opts = append(f.opts, opts)
	var script []ai.AssistantEvent
	if idx < len(f.scripts) {
		script = f.scripts[idx]
	}
	f.mu.Unlock()

	if f.gate != nil && (f.gateAt == 0 || f.gateAt == idx+1) {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return abortedStream(model)
		}
	}
	if ctx.Err() != nil {
		return abortedStream(model)
	}
	if script == nil {
		// An unexpected extra request fails loudly instead of hanging.
		return errorStream(model, fmt.Sprintf("fake provider: no script for call %d", idx+1))
	}
	return ai.StreamOf(script...)
}

// abortedStream is what a provider returns when the context is already done.
func abortedStream(model ai.Model) ai.Stream {
	return errorStream(model, "operation aborted")
}

// errorStream builds a terminal EvError stream. Mirrors what the real
// adapters do for pre-request failures.
func errorStream(model ai.Model, message string) ai.Stream {
	s := ai.NewEventStream()
	msg := ai.Message{
		Role:         ai.RoleAssistant,
		API:          string(model.API),
		Provider:     string(model.Provider),
		Model:        model.ID,
		StopReason:   ai.StopError,
		ErrorMessage: message,
		TimestampMS:  1000,
	}
	reason := ai.StopError
	if message == "operation aborted" {
		reason = ai.StopAborted
	}
	s.Push(ai.EvError{Reason: reason, Error: msg})
	s.End()
	return s
}

// --- script builders --------------------------------------------------------

const (
	fakeAPI        = "fake-api"
	fakeProviderID = "fake"
	fakeModelID    = "fake-model"
	fakeStamp      = int64(1000)
)

// fakeModel is the model every fake script reports.
func fakeModel() ai.Model {
	return ai.Model{ID: fakeModelID, API: ai.APIAnthropicMessages, Provider: ai.ProviderAnthropic}
}

// textScript builds a one-text-block response ending with done.
func textScript(text string, stop ai.StopReason) []ai.AssistantEvent {
	with := func(t string) ai.Message {
		m := ai.Message{Role: ai.RoleAssistant, TimestampMS: fakeStamp}
		if t != "" {
			m.Content = []ai.ContentBlock{ai.TextBlock(t)}
		}
		return m
	}
	final := with(text)
	final.API, final.Provider, final.Model = fakeAPI, fakeProviderID, fakeModelID
	final.StopReason = stop
	final.Usage = &ai.Usage{Input: 1, Output: 1}
	return []ai.AssistantEvent{
		ai.EvStart{Partial: with("")},
		ai.EvTextStart{ContentIndex: 0, Partial: with("")},
		ai.EvTextDelta{ContentIndex: 0, Delta: text, Partial: with(text)},
		ai.EvTextEnd{ContentIndex: 0, Content: text, Partial: with(text)},
		ai.EvDone{Reason: stop, Message: final},
	}
}

// toolCallScript builds a response whose content is the given tool calls,
// with start/delta/end events per call and a final done.
func toolCallScript(stop ai.StopReason, calls ...ai.ContentBlock) []ai.AssistantEvent {
	partial := func(n int) ai.Message {
		m := ai.Message{Role: ai.RoleAssistant, TimestampMS: fakeStamp}
		if n > 0 {
			m.Content = append([]ai.ContentBlock(nil), calls[:n]...)
		}
		return m
	}
	events := []ai.AssistantEvent{ai.EvStart{Partial: partial(0)}}
	for i, c := range calls {
		events = append(events,
			ai.EvToolCallStart{ContentIndex: i, Partial: partial(i + 1)},
			ai.EvToolCallDelta{ContentIndex: i, Delta: `{"partial":true}`, Partial: partial(i + 1)},
			ai.EvToolCallEnd{ContentIndex: i, ToolCall: c, Partial: partial(i + 1)},
		)
	}
	final := partial(len(calls))
	final.API, final.Provider, final.Model = fakeAPI, fakeProviderID, fakeModelID
	final.StopReason = stop
	final.Usage = &ai.Usage{Input: 1, Output: 1}
	events = append(events, ai.EvDone{Reason: stop, Message: final})
	return events
}

// errorScript builds a response that fails mid-protocol (after start).
func errorScript(reason ai.StopReason, message string) []ai.AssistantEvent {
	base := ai.Message{Role: ai.RoleAssistant, TimestampMS: fakeStamp}
	base.API, base.Provider, base.Model = fakeAPI, fakeProviderID, fakeModelID
	base.StopReason = reason
	base.ErrorMessage = message
	return []ai.AssistantEvent{
		ai.EvStart{Partial: base},
		ai.EvError{Reason: reason, Error: base},
	}
}

// callBlock builds a complete tool-call content block. A nil argument map
// becomes an empty object, matching what real adapters emit (never null).
func callBlock(id, name string, args map[string]any) ai.ContentBlock {
	if args == nil {
		args = map[string]any{}
	}
	return ai.ToolCallBlock(id, name, args)
}

// --- event recorder ----------------------------------------------------------

// recorder captures every event a sink receives, in emission order.
type recorder struct {
	mu     sync.Mutex
	events []AgentEvent
	failAt int // when > 0, the sink fails on the event with this 1-based index
}

func (r *recorder) sink(ev AgentEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	if r.failAt > 0 && len(r.events) == r.failAt {
		return fmt.Errorf("recorder: sink failure at event %d", r.failAt)
	}
	return nil
}

func (r *recorder) snapshot() []AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AgentEvent(nil), r.events...)
}

// names returns the event-type sequence, the shape the plan's DoD asserts on.
func (r *recorder) names() []string {
	events := r.snapshot()
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, EventName(ev))
	}
	return out
}

// toolEnds returns the tool_execution_end events in emission order.
func (r *recorder) toolEnds() []EvToolExecutionEnd {
	var out []EvToolExecutionEnd
	for _, ev := range r.snapshot() {
		if e, ok := ev.(EvToolExecutionEnd); ok {
			out = append(out, e)
		}
	}
	return out
}

// toolUpdates returns the tool_execution_update events in emission order.
func (r *recorder) toolUpdates() []EvToolExecutionUpdate {
	var out []EvToolExecutionUpdate
	for _, ev := range r.snapshot() {
		if e, ok := ev.(EvToolExecutionUpdate); ok {
			out = append(out, e)
		}
	}
	return out
}

// toolResultMessages returns the toolResult messages in emission order,
// extracted from their message_start events.
func (r *recorder) toolResultMessages() []ai.Message {
	var out []ai.Message
	for _, ev := range r.snapshot() {
		if e, ok := ev.(EvMessageStart); ok && e.Message.Role == ai.RoleToolResult {
			out = append(out, e.Message)
		}
	}
	return out
}

// messages returns every message_start payload in emission order.
func (r *recorder) messages() []ai.Message {
	var out []ai.Message
	for _, ev := range r.snapshot() {
		if e, ok := ev.(EvMessageStart); ok {
			out = append(out, e.Message)
		}
	}
	return out
}

// --- behavior tools ----------------------------------------------------------

// toolBehavior configures behaviorTool.
type toolBehavior struct {
	delay         time.Duration
	text          string // result text; empty echoes "<name> ran"
	err           error
	terminate     bool
	isError       bool
	executionMode ToolExecutionMode
	onUpdate      bool // emit one progress update while running

	// instrumentation (all optional)
	calls      *int32        // execution counter
	concurrent *int32        // in-flight counter
	maxSeen    *int32        // high-water mark of concurrent
	started    *int32        // increments when execution begins
	release    chan struct{} // if non-nil, execution waits for close before returning
}

// behaviorTool builds a typed tool with the given behavior. Its schema
// accepts any object, so arguments pass validation untouched.
func behaviorTool(name string, b toolBehavior) Tool {
	return Erase(&AgentTool[map[string]any]{
		Tool: ai.Tool{
			Name:        name,
			Description: "behavior tool " + name,
			Parameters:  []byte(`{"type":"object"}`),
		},
		Label:         name,
		ExecutionMode: b.executionMode,
		Execute: func(ctx context.Context, toolCallID string, in map[string]any, onUpdate func(Result)) (Result, error) {
			if b.calls != nil {
				atomic.AddInt32(b.calls, 1)
			}
			if b.started != nil {
				atomic.AddInt32(b.started, 1)
			}
			if b.concurrent != nil {
				cur := atomic.AddInt32(b.concurrent, 1)
				for {
					old := atomic.LoadInt32(b.maxSeen)
					if cur <= old || atomic.CompareAndSwapInt32(b.maxSeen, old, cur) {
						break
					}
				}
				defer atomic.AddInt32(b.concurrent, -1)
			}
			if b.release != nil {
				select {
				case <-b.release:
				case <-ctx.Done():
					return Result{}, ctx.Err()
				}
			}
			if b.delay > 0 {
				select {
				case <-time.After(b.delay):
				case <-ctx.Done():
					return Result{}, ctx.Err()
				}
			}
			if b.onUpdate && onUpdate != nil {
				onUpdate(NewTextResult(name + " progress"))
			}
			if b.err != nil {
				return Result{}, b.err
			}
			text := b.text
			if text == "" {
				text = name + " ran"
			}
			return Result{
				Content:   []ai.ContentBlock{ai.TextBlock(text)},
				Terminate: b.terminate,
				IsError:   b.isError,
			}, nil
		},
	})
}

// userMessage builds a user message with a current timestamp.
func userMessage(text string) ai.Message {
	m := ai.NewTextMessage(ai.RoleUser, text)
	m.TimestampMS = time.Now().UnixMilli()
	return m
}

// textOf returns the text of a result's content blocks, joined the way pi's
// contentText does. Used for single-block assertions.
func textOf(blocks []ai.ContentBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == ai.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// waitForGoroutines blocks until the goroutine count drops to want (or the
// deadline passes), so leak assertions are not flaky.
func waitForGoroutines(want int, deadline time.Duration) int {
	limit := time.Now().Add(deadline)
	for {
		n := goroutineCount()
		if n <= want {
			return n
		}
		if time.Now().After(limit) {
			return n
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func goroutineCount() int {
	return runtime.NumGoroutine()
}
