package ai

import "context"

// StreamFn runs one request against a provider and returns its event stream.
//
// This is the Go counterpart of pi's StreamFunction
// (packages/ai/src/types.ts:760-770 / agent/src/types.ts:423-499): the agent
// loop knows nothing about vendors; it holds a StreamFn and a Model, and the
// Model's Api selects which registered adapter runs. Adding a provider is one
// new package plus an init() registration — the loop does not change.
//
// # Contract (violating any of these is a bug, not a style issue)
//
//  1. Never return a nil Stream and never panic. Every failure — request
//     encoding, HTTP, protocol — is encoded inside the stream as a terminal
//     EvError whose Reason is StopError or StopAborted. pi enforces the same
//     rule (anthropic-messages.ts:887-897, openai-completions.ts:707-729).
//  2. A successful stream emits EvStart before any update and terminates with
//     EvDone; a failure terminates with EvError. A stream that ends without a
//     terminal event is a protocol violation.
//  3. Content is never replayed. Retries may only happen before the first
//     event is pushed; after that a failure is terminal, because the consumer
//     has already rendered what arrived.
//  4. A cancelled context terminates the stream with EvError{Reason:
//     StopAborted} as soon as the in-flight I/O observes it — never by leaving
//     the stream open, and never by panicking out of the goroutine.
type StreamFn func(ctx context.Context, model Model, tc TranscriptContext, opts StreamOptions) Stream

// StreamRequest runs model through the provider registered for model.API.
//
// It is the registry entry point: the loop calls this (or holds a StreamFn
// resolved from it) and never imports a provider package. An unregistered Api
// is a configuration error, reported as an error return rather than a panic,
// so a host that blank-imports only the adapters it ships gets a clear message
// instead of a crash.
//
// The name is StreamRequest rather than Stream because Stream is the interface
// every provider returns; the verb needs a distinct noun.
func StreamRequest(ctx context.Context, model Model, tc TranscriptContext, opts StreamOptions) (Stream, error) {
	fn, ok := lookup(model.API)
	if !ok {
		return nil, &UnknownAPIError{API: model.API}
	}
	return fn(ctx, model, tc, opts), nil
}

// UnknownAPIError reports a Model whose Api has no registered adapter.
type UnknownAPIError struct {
	API Api
}

func (e *UnknownAPIError) Error() string {
	return "ai: no provider registered for api " + string(e.API) + " (registered: " + joinApis(Apis()) + ")"
}

func joinApis(apis []Api) string {
	out := ""
	for i, a := range apis {
		if i > 0 {
			out += ", "
		}
		out += string(a)
	}
	return out
}
