// Package engine runs the agent loop.
//
// # What it is
//
// One function matters: Query. It takes a prompt and produces a stream of
// events, driving the think → act → observe cycle until the model stops asking
// for tools, the context is cancelled, or a turn limit is reached.
//
// # Why an iterator and not a callback or a channel
//
// The TypeScript engine was an `async function*`, and iter.Seq2 is the closest
// thing Go has to it. The consumer stopping the loop is the cancellation
// signal, so a frontend that stops reading cannot leave a producer blocked on
// a send, and no goroutine has to be reaped. The second value carries the
// error, which makes ignoring one a visible choice rather than an accident.
//
// # What it deliberately does not do
//
// It does not render anything. The TypeScript loop wrote ANSI codes straight
// to the terminal, which is why its engine could not be reused by a second
// frontend. Here the loop only produces values, and a sink decides what they
// look like. That separation is what makes --output-format=stream-json a sink
// swap rather than a second engine.
//
// # Layering
//
// Part of the orchestration layer. It depends on the contract packages, the
// capability packages (tool, permission, session), and never on a frontend.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"ultrac/internal/contract/event"
	"ultrac/internal/contract/provider"
	"ultrac/internal/contract/tool"
	"ultrac/internal/logging"
	"ultrac/internal/permission"
)

// Options configures an Engine.
type Options struct {
	// Provider is the model endpoint. Required.
	Provider provider.Provider
	// Tools is the tool set for this run. A nil registry means no tools, which
	// is a legitimate configuration (a pure chat turn).
	Tools *tool.Registry
	// Model overrides the provider's configured model on every request.
	Model string
	// SystemPrompt is the assembled system prompt. Building it is prompt's job,
	// not this package's.
	SystemPrompt string

	// WorkDir is the session's working directory, handed to tools.
	WorkDir string
	// SessionID identifies the session, for transcripts and tool state.
	SessionID string

	// Permission decides tool calls. A nil value allows only tool calls whose
	// own predicates say they are read-only — the conservative default, since
	// "no policy configured" must not mean "anything goes".
	Permission *permission.Engine

	// Ask is consulted when policy says a call needs the user. Nil means there
	// is nobody to ask, and the engine denies with an actionable message
	// instead of hanging.
	Ask AskFunc

	// MaxTurns caps assistant turns. Zero means the default.
	MaxTurns int
	// MaxTokens caps one response. Zero means the provider's default.
	MaxTokens int
	// Thinking enables extended reasoning when non-nil.
	Thinking *provider.Thinking

	// OnToolProgress, when set, receives a tool's progress reports.
	Logger *logging.Logger
}

// AskFunc puts a permission question to the user and returns the verdict.
//
// It is a function rather than an interface because there is exactly one
// method, and an interface with one method that no test will ever mock twice
// is a struct with extra steps.
type AskFunc func(ctx context.Context, req AskRequest) (permission.Result, error)

// AskRequest is the question posed to the user.
type AskRequest struct {
	Tool    string
	Input   json.RawMessage
	Reason  string
	Warning string
}

// DefaultMaxTurns is the loop's cap when Options leaves it at zero.
//
// 40 is the TypeScript default. The number is not a budget — it is a guard
// against a loop that has stopped making progress and is instead burning the
// user's quota repeating itself.
const DefaultMaxTurns = 40

// Engine runs queries. It is stateless between calls: everything a run needs
// is in the Params, so two runs cannot interfere.
type Engine struct {
	opts Options
	log  *logging.Logger
}

// New builds an Engine.
func New(opts Options) *Engine {
	log := opts.Logger
	if log == nil {
		log = logging.Default()
	}
	return &Engine{opts: opts, log: log}
}

// Params is one query.
type Params struct {
	// Prompt is the new user message.
	Prompt string
	// Messages is prior conversation state, for a resumed session. The engine
	// appends to a copy; the caller's slice is not modified.
	Messages []provider.Message
	// MaxTurns overrides Options for this query.
	MaxTurns int
	// SystemPrompt overrides Options for this query.
	SystemPrompt string
}

// Result summarises a finished query.
//
// It is produced after the stream ends, so a caller that needs it must drain
// the whole sequence. The terminal sink uses it for the `result` line.
type Result struct {
	Turns     int
	Usage     provider.Usage
	Stop      provider.StopReason
	Text      string
	IsError   bool
	Failure   *event.Failure
	ToolCalls int
}

// tracker accumulates the run summary while events stream past. It is written
// only by the loop goroutine, so it needs no lock of its own beyond the
// Done channel that publishes it.
type tracker struct {
	mu        sync.Mutex
	result    Result
	done      chan struct{}
	closeOnce sync.Once
}

func newTracker() *tracker { return &tracker{done: make(chan struct{})} }

func (t *tracker) add(ev event.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch ev.Kind {
	case event.KindTextDelta:
		t.result.Text += ev.Text
	case event.KindToolUse:
		t.result.ToolCalls++
	case event.KindUsage:
		if ev.Usage != nil {
			t.result.Usage.InputTokens += ev.Usage.InputTokens
			t.result.Usage.OutputTokens += ev.Usage.OutputTokens
			t.result.Usage.CacheReadTokens += ev.Usage.CacheReadTokens
			t.result.Usage.CacheWriteTokens += ev.Usage.CacheWriteTokens
		}
	case event.KindMessageStop:
		t.result.Stop = ev.Stop
	case event.KindFailure:
		if ev.Failure != nil {
			f := *ev.Failure
			t.result.Failure = &f
			t.result.IsError = true
		}
	}
}

func (t *tracker) turn() {
	t.mu.Lock()
	t.result.Turns++
	t.mu.Unlock()
}

// currentTurn is the turn number tools are told about.
func (t *tracker) currentTurn() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result.Turns
}

// Snapshot returns the result. It is only valid once Done is closed.
func (t *tracker) Snapshot() Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result
}

// Result returns the summary and a channel closed when the query ends.
//
// The channel lets a sink wait for the total without the loop having to run to
// completion first — which it cannot, since the sink is what consumes it.
func (e *Engine) Result(ctx context.Context, p Params) (*ResultHandle, event.Seq) {
	t := newTracker()
	return &ResultHandle{t: t}, e.run(ctx, p, t)
}

// ResultHandle publishes the outcome of a query.
type ResultHandle struct{ t *tracker }

// Wait blocks until the query finishes and returns its summary.
func (h *ResultHandle) Wait(ctx context.Context) (Result, error) {
	if h == nil || h.t == nil {
		return Result{}, fmt.Errorf("engine: no result handle")
	}
	select {
	case <-h.t.done:
		return h.t.Snapshot(), nil
	case <-ctx.Done():
		return h.t.Snapshot(), ctx.Err()
	}
}
