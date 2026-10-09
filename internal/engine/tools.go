package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"ultrac/internal/contract/event"
	"ultrac/internal/contract/provider"
	"ultrac/internal/contract/tool"
	"ultrac/internal/logging"
	"ultrac/internal/permission"
	"ultrac/internal/shell"
)

// maxParallelTools bounds concurrent tool execution.
//
// Unbounded parallelism is how twenty parallel reads become twenty thousand
// open file handles. Twelve is the same order the TypeScript side used and is
// well above the batch sizes that actually occur.
const maxParallelTools = 12

// toolOutcome is one call's result, kept alongside its position so results can
// be returned in the order the model asked for them.
type toolOutcome struct {
	index  int
	result event.ToolResult
}

// runTools executes a batch of tool calls and returns their results in order.
//
// The ordering rule is the load-bearing part. Calls the tool reports as
// concurrency-safe are batched and run in parallel; anything else runs alone,
// in place. That is not an optimisation detail — it is what keeps a
// `rm -rf` from running concurrently with the `mkdir` it depends on, and the
// TypeScript implementation had the same distinction for the same reason.
//
// A call that is refused or unknown still produces a result. The API requires
// one tool_result per tool_use, so skipping a denied call would make the next
// request invalid rather than merely less useful.
func (e *Engine) runTools(
	ctx context.Context,
	calls []provider.ToolCall,
	yield func(event.Event, error) bool,
	t *tracker,
) ([]event.ToolResult, bool) {
	results := make([]event.ToolResult, 0, len(calls))

	// Group consecutive concurrency-safe calls so they can run together, and
	// let each unsafe call run on its own.
	for i := 0; i < len(calls); {
		if !e.isConcurrencySafe(calls[i]) {
			out, stopped := e.runOneSafely(ctx, i, calls[i], yield, t)
			if stopped {
				return results, true
			}
			results = append(results, out.result)
			i++
			continue
		}

		// Collect the run of safe calls.
		end := i
		for end < len(calls) && e.isConcurrencySafe(calls[end]) {
			end++
		}
		batch := calls[i:end]
		outs := e.runBatch(ctx, i, batch, yield, t)
		for _, o := range outs {
			results = append(results, o.result)
		}
		i = end
	}
	return results, false
}

// runBatch executes concurrency-safe calls with a bounded worker pool.
func (e *Engine) runBatch(
	ctx context.Context,
	base int,
	calls []provider.ToolCall,
	yield func(event.Event, error) bool,
	t *tracker,
) []toolOutcome {
	outs := make([]toolOutcome, len(calls))

	// Progress events arrive from worker goroutines, so they are marshalled
	// onto the loop goroutine through a channel. Emitting to yield directly
	// from a worker would be a data race on the consumer's state, and the
	// consumer is not guaranteed to be goroutine-safe.
	progress := make(chan event.Progress, 64)
	progressDone := make(chan struct{})

	go func() {
		defer close(progressDone)
		for p := range progress {
			// A dropped progress event is preferable to blocking a worker.
			t.emit(yield, event.ToolProgress(p))
		}
	}()

	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for j, call := range calls {
		j, call := j, call
		id := call.ID
		// Progress is forwarded through the channel rather than emitted
		// directly: this closure runs on a worker goroutine and the consumer
		// of yield is not required to be goroutine-safe.
		pr := func(p tool.Progress) {
			if p.Message == "" {
				return
			}
			select {
			case progress <- event.Progress{ToolUseID: id, Message: p.Message, Fraction: p.Fraction}:
			default:
				// A slow consumer drops progress, never blocks a worker.
			}
		}

		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out, _ := e.runOne(ctx, base+j, call, pr, t)
			outs[j] = out
		}()
	}
	wg.Wait()
	close(progress)
	<-progressDone

	// Announce results on the loop goroutine, in order, after the batch has
	// finished. Emitting them from the workers would interleave related calls
	// with unrelated ones.
	for _, o := range outs {
		t.emit(yield, event.ToolOutput(o.result))
	}
	return outs
}

// runOneSafely runs a single call and reports cancellation.
func (e *Engine) runOneSafely(
	ctx context.Context,
	index int,
	call provider.ToolCall,
	yield func(event.Event, error) bool,
	t *tracker,
) (toolOutcome, bool) {
	progress := func(p tool.Progress) {
		if p.Message == "" {
			return
		}
		t.emit(yield, event.ToolProgress(event.Progress{
			ToolUseID: call.ID,
			Message:   p.Message,
			Fraction:  p.Fraction,
		}))
	}
	out, stopped := e.runOne(ctx, index, call, progress, t)
	if stopped {
		return out, true
	}
	if !t.emit(yield, event.ToolOutput(out.result)) {
		return out, true
	}
	return out, false
}

// runOne performs one tool call end to end: look up, decide, execute.
func (e *Engine) runOne(
	ctx context.Context,
	index int,
	call provider.ToolCall,
	progress tool.ProgressFunc,
	t *tracker,
) (toolOutcome, bool) {
	out := toolOutcome{index: index, result: event.ToolResult{
		ToolUseID: call.ID,
		Name:      call.Name,
	}}

	if e.opts.Tools == nil {
		out.result.Content = "no tools are available in this run"
		out.result.IsError = true
		return out, false
	}

	tl, ok := e.opts.Tools.Lookup(call.Name)
	if !ok {
		out.result.Content = fmt.Sprintf(
			"unknown tool %q. Available tools: %s", call.Name, strings.Join(e.opts.Tools.Names(), ", "))
		out.result.IsError = true
		return out, false
	}

	// Cancellation is checked before the permission prompt as well as after:
	// prompting for a call that has already been cancelled wastes the user's
	// attention on a decision that no longer matters.
	if err := ctx.Err(); err != nil {
		out.result.Content = "the tool call was cancelled before it ran"
		out.result.IsError = true
		return out, false
	}

	verdict := e.decide(ctx, tl, call)
	if verdict.Decision != tool.DecisionAllow {
		out.result.Content = denialMessage(call.Name, verdict)
		out.result.IsError = true
		return out, false
	}

	input := call.Input
	if len(verdict.UpdatedInput) > 0 {
		input = verdict.UpdatedInput
	}

	uc := &tool.UseContext{
		SessionID: e.opts.SessionID,
		WorkDir:   e.opts.WorkDir,
		TurnIndex: t.currentTurn(),
	}

	res, err := tl.Call(ctx, input, uc, e.canUseFunc(), progress)
	if err != nil {
		// A Go error means the call never reached a verdict. It is reported as
		// a failure the model can read, not as a broken turn: `the file was
		// deleted mid-edit` should not end the conversation.
		e.log.Error(err, logging.F("tool", call.Name))
		out.result.Content = fmt.Sprintf("%s failed to run: %v", call.Name, err)
		out.result.IsError = true
		return out, false
	}
	if res == nil {
		out.result.Content = call.Name + " returned no result"
		out.result.IsError = true
		return out, false
	}

	out.result.Content = res.Content
	out.result.IsError = res.IsError
	return out, false
}

// decide applies policy to one call.
func (e *Engine) decide(ctx context.Context, tl tool.Erased, call provider.ToolCall) permission.Result {
	if e.opts.Permission == nil {
		// No policy configured means the conservative default, not the
		// permissive one: a call is allowed only if the tool itself says it
		// only reads. Anything else becomes an ask, which without an asker
		// becomes a deny.
		if tl.IsReadOnly(call.Input) {
			return permission.Result{Decision: tool.DecisionAllow, Reason: "read-only tool, no policy configured"}
		}
		return permission.Result{
			Decision: tool.DecisionAsk,
			Reason:   "no permission policy is configured for this run",
		}
	}

	req := e.classify(tl, call)
	res := e.opts.Permission.Check(ctx, req)
	if res.Decision != tool.DecisionAsk || e.opts.Ask == nil {
		return res
	}

	answer, err := e.opts.Ask(ctx, AskRequest{
		Tool:    call.Name,
		Input:   call.Input,
		Reason:  res.Reason,
		Warning: res.Warning,
	})
	if err != nil {
		return permission.Result{
			Decision: tool.DecisionDeny,
			Reason:   "the permission prompt failed: " + err.Error(),
		}
	}
	return answer
}

// classify builds the policy request for a call.
//
// It lives in the engine, not in the tools, because only the engine knows the
// session's working directories and mode. A tool that classified itself would
// have to be handed the whole policy context, which is the coupling the
// CanUseFunc design avoids.
func (e *Engine) classify(tl tool.Erased, call provider.ToolCall) permission.Request {
	req := permission.Request{
		Tool:        call.Name,
		Input:       call.Input,
		ReadOnly:    tl.IsReadOnly(call.Input),
		Destructive: tl.IsDestructive(call.Input),
		Kind:        kindFor(call.Name),
	}

	switch call.Name {
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(call.Input, &in); err == nil {
			req.Content = in.Command
			req.Bash = shell.Analyze(in.Command)
		}
	case "Write", "Edit", "Read":
		var in struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal(call.Input, &in); err == nil && in.FilePath != "" {
			path := in.FilePath
			if !isAbs(path) {
				path = joinWorkDir(e.opts.WorkDir, path)
			}
			req.Content = path
			// The relative form, because a rule written by hand says
			// `Read(go.mod)`, not the absolute path it resolves to.
			req.ContentAlt = relativeSlash(e.opts.WorkDir, path)
			req.Paths = []string{path}
		}
	case "Glob", "Grep":
		var in struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal(call.Input, &in); err == nil {
			req.Content = in.Pattern
			if in.Path != "" {
				req.ContentAlt = relativeSlash(e.opts.WorkDir, in.Path)
			}
		}
	case "WebFetch":
		var in struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(call.Input, &in); err == nil {
			req.Content = in.URL
		}
	}
	return req
}

// kindFor maps a tool name onto the policy category.
func kindFor(name string) permission.Kind {
	switch name {
	case "Read", "Glob", "Grep", "WebFetch", "WebSearch", "TaskOutput":
		return permission.KindRead
	case "Write", "Edit":
		return permission.KindEdit
	case "Bash":
		return permission.KindBash
	case "TodoWrite":
		return permission.KindInternal
	default:
		return permission.KindOther
	}
}

// isConcurrencySafe reports whether a call may run beside its neighbours.
func (e *Engine) isConcurrencySafe(call provider.ToolCall) bool {
	if e.opts.Tools == nil {
		return false
	}
	tl, ok := e.opts.Tools.Lookup(call.Name)
	if !ok {
		// Unknown tools are not run at all, so it does not matter; false keeps
		// them in the serial path where a mistake is harmless.
		return false
	}
	return tl.IsConcurrencySafe(call.Input)
}

// canUseFunc is the hook the tool contract offers.
//
// The engine has already decided, so this is a re-check rather than a second
// decision. It is wired up rather than passed as nil so a tool that wants to
// ask about a narrower follow-up (a sandbox override, a nested write) has a
// working path instead of a nil dereference.
func (e *Engine) canUseFunc() tool.CanUseFunc {
	return func(ctx context.Context, name string, input json.RawMessage) (tool.PermissionResult, error) {
		if e.opts.Permission == nil {
			return tool.PermissionResult{Decision: tool.DecisionAsk, Reason: "no permission policy configured"}, nil
		}
		req := e.classifyName(name, input)
		res := e.opts.Permission.Check(ctx, req)
		return tool.PermissionResult{Decision: res.Decision, Reason: res.Reason}, nil
	}
}

func (e *Engine) classifyName(name string, input json.RawMessage) permission.Request {
	return e.classify(erasedShell{name: name}, provider.ToolCall{Name: name, Input: input})
}

// erasedShell is a tool.Erased whose only answer is its name.
//
// canUseFunc only has a name and raw input, so the predicates have no
// implementation to call. Answering false for every predicate routes the call
// to the ask path, which is the safe direction.
type erasedShell struct{ name string }

func (s erasedShell) Name() string                                   { return s.name }
func (s erasedShell) Aliases() []string                              { return nil }
func (s erasedShell) SearchHint() string                             { return "" }
func (s erasedShell) InputSchema() json.RawMessage                   { return nil }
func (s erasedShell) InterruptBehavior() tool.InterruptBehavior      { return tool.InterruptBlock }
func (s erasedShell) RequiresUserInteraction() bool                  { return false }
func (s erasedShell) IsConcurrencySafe(json.RawMessage) bool         { return false }
func (s erasedShell) IsReadOnly(json.RawMessage) bool                { return false }
func (s erasedShell) IsDestructive(json.RawMessage) bool             { return true }
func (s erasedShell) IsOpenWorld(json.RawMessage) bool               { return true }
func (s erasedShell) IsSearchOrRead(json.RawMessage) tool.SearchRead { return tool.SearchRead{} }

func (s erasedShell) Description(context.Context, json.RawMessage, tool.DescriptionOptions) (string, error) {
	return "", errors.New("erasedShell has no description")
}

func (s erasedShell) Call(context.Context, json.RawMessage, *tool.UseContext, tool.CanUseFunc, tool.ProgressFunc) (*tool.Result, error) {
	return nil, errors.New("erasedShell cannot be called")
}

// denialMessage explains a refusal in terms of what to do about it.
//
// "Permission denied" on its own is the least actionable message an agent can
// produce: the user cannot tell whether they need a rule, a different mode, or
// a different tool.
func denialMessage(name string, res permission.Result) string {
	var b strings.Builder
	b.WriteString(name + " was not run: ")
	b.WriteString(res.Reason)
	if res.Warning != "" {
		b.WriteString(" (" + res.Warning + ")")
	}
	if res.Rule != nil {
		b.WriteString(" [rule: " + res.Rule.String() + "]")
	}
	b.WriteString("\nIf this call should be allowed, either add a permission rule, " +
		"or re-run with a mode that permits it (--permission-mode acceptEdits for file edits, " +
		"--dangerously-skip-permissions to approve everything).")
	return b.String()
}

func isAbs(p string) bool {
	if p == "" {
		return false
	}
	if p[0] == '/' || p[0] == '\\' {
		return true
	}
	return len(p) > 1 && p[1] == ':'
}

func joinWorkDir(dir, rel string) string {
	if dir == "" {
		return rel
	}
	sep := "/"
	if strings.Contains(dir, "\\") {
		sep = "\\"
	}
	return strings.TrimRight(dir, "/\\") + sep + strings.TrimLeft(rel, "/\\")
}

// relativeSlash renders path relative to dir with forward slashes, or ""
// when path is not below dir.
func relativeSlash(dir, path string) string {
	if dir == "" {
		return ""
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}
