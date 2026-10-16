// probe.go — the `ultrac probe` subcommand: stream one real request through
// a registered adapter and print the 12-event sequence.
//
// This is bin/probe (the throwaway P1 harness) promoted to a product
// surface. P1's landing record promised it: "the real CLI probe command
// (ultrac probe) lands in P5". The event matrix and the per-scenario
// verdicts are unchanged — the probe's job is to answer "does this endpoint
// speak the protocol we think it does", and that question has not changed.
//
// No single request produces all 12 events — that is a property of the
// protocol, not of the adapter. The matrix below is what "12 events all
// exercised" means in practice:
//
//	scenario                                events covered
//	──────────────────────────────────────  ────────────────────────────────
//	default (plain text)                    start / text_* / done
//	thinking model (-mode anthropic)        + thinking_start/delta/end
//	-tool                                   + toolcall_start/delta/end
//	-badkey (invalid credential)            error{reason:error}
//	-cancel-after N (cancel after N events)  error{reason:aborted}
//
// Usage:
//
//	ultrac probe                              # DeepSeek's anthropic endpoint
//	ultrac probe -mode openai                 # DeepSeek's chat completions
//	ultrac probe -tool                        # exercise tool_use
//	ultrac probe -badkey                      # force the error event
//	ultrac probe -cancel-after 3              # force the aborted event
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ultrac/internal/ai"
	_ "ultrac/internal/ai/anthropic" // blank import: init() registration
	_ "ultrac/internal/ai/openai"    // blank import: init() registration
	"ultrac/internal/config"
)

// allEvents is the frozen 12-event protocol (contract C2). A run reports
// which of these it actually observed.
var allEvents = []string{
	"start",
	"text_start", "text_delta", "text_end",
	"thinking_start", "thinking_delta", "thinking_end",
	"toolcall_start", "toolcall_delta", "toolcall_end",
	"done", "error",
}

// probeFlags is the probe's own tiny parser. It deliberately does not share
// parseFlags: the probe is a diagnostic with its own vocabulary, and folding
// it into the run flags would put --cancel-after and -badkey into the
// product surface.
type probeFlags struct {
	mode        string
	baseURL     string
	modelID     string
	prompt      string
	withTool    bool
	reasoning   string
	badKey      bool
	cancelAfter int
}

func runProbe(args []string, stdout, stderr io.Writer) int {
	f := &probeFlags{mode: "anthropic", prompt: "Reply with exactly: P1-OK"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := splitFlag(a)
		value := func() (string, bool) {
			if hasInline {
				return inline, true
			}
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch name {
		case "-mode":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "probe: -mode needs a value")
				return 2
			}
			f.mode = v
		case "-base":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "probe: -base needs a value")
				return 2
			}
			f.baseURL = v
		case "-model":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "probe: -model needs a value")
				return 2
			}
			f.modelID = v
		case "-p":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "probe: -p needs a value")
				return 2
			}
			f.prompt = v
		case "-tool":
			f.withTool = true
		case "-reasoning":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "probe: -reasoning needs a value")
				return 2
			}
			f.reasoning = v
		case "-badkey":
			f.badKey = true
		case "-cancel-after":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "probe: -cancel-after needs a value")
				return 2
			}
			n := 0
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
				fmt.Fprintf(stderr, "probe: -cancel-after must be a positive integer; got %q\n", v)
				return 2
			}
			f.cancelAfter = n
		default:
			fmt.Fprintf(stderr, "probe: unknown flag %q\n", name)
			return 2
		}
	}
	return probeRun(f, stdout, stderr)
}

func probeRun(f *probeFlags, stdout, stderr io.Writer) int {
	settings := probeSettings()
	apiKey := settings[config.EnvAuthToken]
	if apiKey == "" {
		apiKey = settings[config.EnvAPIKey]
	}
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}
	if apiKey == "" && !f.badKey {
		fmt.Fprintln(stderr, "no credential: set DEEPSEEK_API_KEY or configure ~/.ultrac/settings.json")
		return 2
	}
	if f.badKey {
		// Keep the endpoint and model, break only the credential: the
		// request must actually reach the server so the 401 travels the
		// real path (adapter → APIError → EvError), not a local shortcut.
		apiKey = "sk-probe-invalid-key"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var (
		model ai.Model
		opts  ai.StreamOptions
	)
	if f.reasoning != "" {
		opts.Reasoning = ai.ThinkingLevel(f.reasoning)
	}

	switch f.mode {
	case "openai":
		model = ai.WithDefaults(ai.Model{
			Provider: ai.ProviderDeepseek,
			ID:       firstNonEmpty(f.modelID, "deepseek-chat"),
		})
		model.API = ai.APIOpenAICompletions
		model.BaseURL = firstNonEmpty(f.baseURL, "https://api.deepseek.com")
	default: // anthropic | tools
		model = ai.WithDefaults(ai.Model{
			Provider: ai.ProviderDeepseek,
			API:      ai.APIAnthropicMessages,
			ID:       firstNonEmpty(f.modelID, settings[config.EnvModel], "deepseek-chat"),
		})
		model.BaseURL = firstNonEmpty(f.baseURL, settings[config.EnvBaseURL], "https://api.deepseek.com/anthropic")
	}
	opts.APIKey = apiKey

	systemPrompt := "You are a probe. Answer tersely."
	messages := []ai.Message{ai.NewTextMessage(ai.RoleUser, f.prompt)}
	var tools []ai.Tool
	if f.withTool || f.mode == "tools" {
		systemPrompt = "You are a probe. When asked about a file, call the read tool."
		tools = []ai.Tool{{
			Name:        "read",
			Description: "Read a file from disk",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}
	}

	tc := ai.NormalizeContext(ai.Context{
		SystemPrompt: systemPrompt,
		Messages:     messages,
		Tools:        tools,
	})

	fmt.Fprintf(stdout, "== probe: model=%s api=%s base=%s reasoning=%q tool=%v\n",
		model.ID, model.API, model.BaseURL, opts.Reasoning, f.withTool || f.mode == "tools")

	stream, err := ai.StreamRequest(ctx, model, tc, opts)
	if err != nil {
		fmt.Fprintln(stderr, "StreamRequest error:", err)
		return 1
	}

	start := time.Now()
	var seq []string
	var text strings.Builder
	var think strings.Builder
	for ev, err := range stream.Events() {
		if err != nil {
			fmt.Fprintln(stderr, "iterator error:", err)
			return 1
		}
		name := ai.Name(ev)
		seq = append(seq, name)
		first := ""
		if len(seq) == 1 {
			first = fmt.Sprintf(" (t+%dms)", time.Since(start).Milliseconds())
		}
		detail := ""
		switch e := ev.(type) {
		case ai.EvTextDelta:
			// Deltas only: text_end repeats the whole block, and adding it
			// too would double the text in this report.
			text.WriteString(e.Delta)
		case ai.EvThinkingDelta:
			think.WriteString(e.Delta)
		case ai.EvToolCallEnd:
			args, _ := json.Marshal(e.ToolCall.Arguments)
			detail = fmt.Sprintf(" id=%s name=%s args=%s", e.ToolCall.ID, e.ToolCall.Name, args)
		case ai.EvDone:
			detail = fmt.Sprintf(" reason=%s", e.Reason)
			if e.Message.Usage != nil {
				detail += fmt.Sprintf(" usage={in:%d out:%d cacheRead:%d total:%d cost:$%.6f}",
					e.Message.Usage.Input, e.Message.Usage.Output, e.Message.Usage.CacheRead,
					e.Message.Usage.TotalTokens, e.Message.Usage.Cost.Total)
			}
		case ai.EvError:
			detail = fmt.Sprintf(" reason=%s error=%s", e.Reason, e.Error.ErrorMessage)
		}
		fmt.Fprintf(stdout, "  %-16s%s%s\n", name, first, detail)

		// Cancelling from inside the loop is the honest simulation of a
		// user pressing escape mid-answer: the producer goroutine is still
		// reading the body, and the cancellation surfaces there as EvError
		// with reason=aborted (never as a dropped stream).
		if f.cancelAfter > 0 && len(seq) >= f.cancelAfter {
			fmt.Fprintf(stdout, "  (cancelling after %d events)\n", len(seq))
			cancel()
			f.cancelAfter = 0 // once
		}
	}

	msg, err := stream.Result()
	if err != nil {
		fmt.Fprintln(stderr, "Result error:", err)
		return 1
	}

	fmt.Fprintln(stdout, "== sequence:", strings.Join(seq, " → "))
	if think.Len() > 0 {
		fmt.Fprintf(stdout, "== thinking (%d chars): %s\n", think.Len(), truncate(think.String(), 200))
	}
	fmt.Fprintf(stdout, "== text (%d chars): %s\n", text.Len(), truncate(text.String(), 300))
	fmt.Fprintf(stdout, "== final: stopReason=%s blocks=%d durationMs=%d\n", msg.StopReason, len(msg.Content), msg.DurationMS)
	fmt.Fprintln(stdout, "== coverage:", coverage(seq))

	// Verdicts are scenario-specific: a bad-key run MUST end in an error
	// whose message mentions authentication, a cancelled run MUST end in an
	// aborted error, and a normal run must open with start and close with
	// done. One shared verdict would pass a failure.
	return probeVerdict(f, seq, msg, stdout)
}

// probeVerdict applies the per-scenario pass criteria.
func probeVerdict(f *probeFlags, seq []string, msg ai.Message, stdout io.Writer) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stdout, "VERDICT: FAIL — "+format+"\n", args...)
		return 1
	}
	joined := strings.Join(seq, " → ")
	last, _ := lastEvent(seq)

	switch {
	case f.badKey:
		if last != "error" {
			return fail("a rejected credential must terminate the stream with an error event")
		}
		if !strings.Contains(msg.ErrorMessage, "auth") && !strings.Contains(msg.ErrorMessage, "401") {
			return fail("error message does not mention authentication: %s", msg.ErrorMessage)
		}
		fmt.Fprintln(stdout, "VERDICT: PASS (error event)")
		return 0
	case last == "error":
		if msg.StopReason != ai.StopAborted {
			return fail("stopReason = %s, want aborted", msg.StopReason)
		}
		fmt.Fprintln(stdout, "VERDICT: PASS (aborted event)")
		return 0
	default:
		if !strings.HasPrefix(joined, "start → ") || !strings.HasSuffix(joined, " → done") {
			return fail("sequence must open with start and close with done")
		}
		if strings.Contains(joined, "toolcall_start") {
			// Tool turn: the model may go straight to the call without
			// preamble text (it did, nondeterministically, between two runs
			// of the same prompt), so the text-block requirement is waived
			// and the completion of the call is what must hold.
			if !strings.Contains(joined, "toolcall_end") {
				return fail("tool call opened but never completed")
			}
		} else if !strings.Contains(joined, "text_start → text_delta") || !strings.Contains(joined, "text_end") {
			return fail("no complete text block events")
		}
		if msg.StopReason != ai.StopStop && msg.StopReason != ai.StopToolUse {
			return fail("stopReason = %s", msg.StopReason)
		}
		fmt.Fprintln(stdout, "VERDICT: PASS")
		return 0
	}
}

// coverage reports which of the 12 protocol events this run observed. A
// successful run can never contain "error" and an error run contains little
// else, so the number is a per-run fact, not a score.
func coverage(seq []string) string {
	seen := make(map[string]bool, len(seq))
	for _, name := range seq {
		seen[name] = true
	}
	var have, missing []string
	for _, name := range allEvents {
		if seen[name] {
			have = append(have, name)
		} else {
			missing = append(missing, name)
		}
	}
	out := fmt.Sprintf("%d/12 seen: %s", len(have), strings.Join(have, " "))
	if len(missing) > 0 {
		out += fmt.Sprintf("  | missing: %s", strings.Join(missing, " "))
	}
	return out
}

func lastEvent(seq []string) (string, bool) {
	if len(seq) == 0 {
		return "", false
	}
	return seq[len(seq)-1], true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// probeSettings pulls the provider env block out of the user settings file.
// The probe bypasses the layer resolver on purpose: it is a raw endpoint
// check, and the layers' project-scoped env would make "what does my user
// config say" unanswerable from inside a project directory.
func probeSettings() map[string]string {
	raw, err := os.ReadFile(filepath.Join(config.ConfigHome(), "settings.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc.Env
}
