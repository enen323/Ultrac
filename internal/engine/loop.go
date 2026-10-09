package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ultrac/internal/contract/event"
	"ultrac/internal/contract/provider"
	"ultrac/internal/contract/tool"
	"ultrac/internal/logging"
)

// run is the loop.
//
// Shape, and why it is this shape:
//
//	for each turn:
//	  ask the model for one message
//	  if it asked for tools, run them and feed the results back
//	  else stop
//
// Every exit path is explicit. A loop that can fail silently is a loop whose
// failures are discovered as "the agent stopped early for some reason", which
// is the hardest possible bug report to act on.
func (e *Engine) run(ctx context.Context, p Params, t *tracker) event.Seq {
	return func(yield func(event.Event, error) bool) {
		defer t.finish()

		if e.opts.Provider == nil {
			fail(yield, t, event.CodeNoProvider,
				"no model provider is configured: set ANTHROPIC_BASE_URL and a credential in ~/.ultrac/settings.json")
			return
		}

		prompt := strings.TrimSpace(p.Prompt)
		if prompt == "" && len(p.Messages) == 0 {
			fail(yield, t, event.CodeNoPrompt, "the query has no prompt")
			return
		}

		messages := make([]provider.Message, 0, len(p.Messages)+4)
		messages = append(messages, p.Messages...)
		if prompt != "" {
			messages = append(messages, provider.Message{
				Role:   provider.RoleUser,
				Blocks: []provider.Block{{Kind: provider.BlockText, Text: p.Prompt}},
			})
		}

		system := p.SystemPrompt
		if system == "" {
			system = e.opts.SystemPrompt
		}

		maxTurns := p.MaxTurns
		if maxTurns <= 0 {
			maxTurns = e.opts.MaxTurns
		}
		if maxTurns <= 0 {
			maxTurns = DefaultMaxTurns
		}

		schemas := e.toolSchemas()
		warnedAboutThinking := false

		for turn := 0; ; turn++ {
			// Cancellation is checked at the top of every turn rather than
			// inside the provider call, because a provider that ignores the
			// context would otherwise run one more full request.
			if err := ctx.Err(); err != nil {
				fail(yield, t, event.CodeCancelled, "the query was cancelled")
				return
			}
			if turn >= maxTurns {
				fail(yield, t, event.CodeMaxTurns,
					fmt.Sprintf("stopped after %d turns without finishing", maxTurns))
				return
			}
			t.turn()

			if !t.emit(yield, event.Event{Kind: event.KindMessageStart}) {
				return
			}

			req := provider.Request{
				Model:     e.opts.Model,
				System:    system,
				Messages:  messages,
				Tools:     schemas,
				MaxTokens: e.opts.MaxTokens,
				Thinking:  e.opts.Thinking,
			}

			out, stopped, err := e.streamTurn(ctx, req, yield, t)
			if stopped {
				return
			}
			if err != nil {
				fail(yield, t, errorCode(err), err.Error())
				return
			}

			if out.sawThinking && e.opts.Thinking != nil && !warnedAboutThinking {
				warnedAboutThinking = true
				t.emit(yield, event.NoticeEvent(event.Notice{
					Code: "thinking_not_replayed",
					Message: "extended thinking deltas are shown but not replayed to the model: " +
						"the signature needed to resend a thinking block is not yet plumbed through, " +
						"so multi-turn reasoning context is dropped (see T2)",
				}))
			}

			// The model's own turn goes back into the conversation before its
			// tool results, or the tool_result blocks would answer nothing.
			if len(out.blocks) > 0 {
				messages = append(messages, provider.Message{
					Role:   provider.RoleAssistant,
					Blocks: out.blocks,
				})
			}

			if len(out.toolCalls) == 0 {
				// The normal end: the model produced a message with no tool
				// calls, so there is nothing left to do.
				t.emit(yield, event.StopEvent(out.stop))
				return
			}

			results, stop := e.runTools(ctx, out.toolCalls, yield, t)
			if stop {
				return
			}
			t.emit(yield, event.StopEvent(out.stop))

			if len(results) == 0 {
				return
			}
			messages = append(messages, toolResultMessage(results))

			if out.stop == provider.StopMaxTokens {
				// The model ran out of room mid-tool-call. Feeding the results
				// back is still right, but the caller should know the turn was
				// cut off rather than completed.
				t.emit(yield, event.NoticeEvent(event.Notice{
					Code:    "max_tokens",
					Message: "the model hit its output limit; the response may be incomplete",
				}))
			}
		}
	}
}

// toolSchemas advertises the registered tools to the model.
//
// The description is computed per tool rather than fixed, because a tool may
// embed the working directory or a listing in it. It is computed once per run
// here, not per turn: the description cannot change mid-run, and recomputing
// it would mean describing every tool on every turn.
func (e *Engine) toolSchemas() []provider.ToolSchema {
	if e.opts.Tools == nil || e.opts.Tools.Len() == 0 {
		return nil
	}
	ctx := context.Background()
	opts := tool.DescriptionOptions{IsNonInteractive: e.opts.Ask == nil}

	all := e.opts.Tools.All()
	out := make([]provider.ToolSchema, 0, len(all))
	for _, t := range all {
		desc, err := t.Description(ctx, json.RawMessage(nil), opts)
		if err != nil {
			// A tool that cannot describe itself is not advertised. Sending an
			// empty description would teach the model to guess.
			e.log.Warn("tool description failed; tool not advertised",
				logging.F("tool", t.Name()), logging.F("error", err.Error()))
			continue
		}
		schema := t.InputSchema()
		if len(schema) == 0 {
			e.log.Warn("tool has no input schema; not advertised", logging.F("tool", t.Name()))
			continue
		}
		out = append(out, provider.ToolSchema{
			Name:        t.Name(),
			Description: desc,
			InputSchema: schema,
		})
	}
	return out
}
