package builtin

import (
	"context"
	"strings"
	"sync"
	"time"

	"ultrac/internal/contract/tool"
)

// ---------------------------------------------------------------------------
// Agent
// ---------------------------------------------------------------------------

// AgentInput mirrors AgentTool's base schema.
type AgentInput struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type,omitempty"`
	Model        string `json:"model,omitempty"`
}

// AgentRun is one subagent invocation handed to the runner.
type AgentRun struct {
	Description  string
	Prompt       string
	SubagentType string
	Model        string
	SessionID    string
	WorkDir      string
}

// AgentRunner executes a subagent and returns its final text.
//
// Injected by the engine, because running a subagent means running the query
// loop again, and the engine is the thing that owns the loop. If builtin
// imported the engine directly the two packages would be a cycle — and the
// cycle would be real, not incidental.
type AgentRunner func(ctx context.Context, run AgentRun) (string, error)

var (
	agentMu     sync.RWMutex
	agentRunner AgentRunner
)

// SetAgentRunner installs the subagent executor. Nil disables the tool.
func SetAgentRunner(r AgentRunner) {
	agentMu.Lock()
	defer agentMu.Unlock()
	agentRunner = r
}

func currentAgentRunner() AgentRunner {
	agentMu.RLock()
	defer agentMu.RUnlock()
	return agentRunner
}

const agentSchema = `{
  "type": "object",
  "properties": {
    "description": {
      "type": "string",
      "description": "A short (3-5 word) description of the task"
    },
    "prompt": {
      "type": "string",
      "description": "The task for the agent to perform"
    },
    "subagent_type": {
      "type": "string",
      "description": "The type of specialized agent to use for this task"
    },
    "model": {
      "type": "string",
      "enum": ["sonnet", "opus", "haiku"],
      "description": "Optional model override for this agent. If omitted, inherits from the parent."
    }
  },
  "required": ["description", "prompt"],
  "additionalProperties": false
}`

const agentDescription = `Launch a new agent to handle complex, multi-step tasks autonomously.

The Agent tool launches specialized agents (subprocesses) that autonomously handle complex tasks. Each agent type has specific capabilities and tools available to it.

Available agent types:
- general-purpose: General-purpose agent for researching complex questions, searching for code, and executing multi-step tasks. When you are searching for a keyword or file and are not confident that you will find the right match in the first few tries use this agent to perform the search for you.

When to use the Agent tool:
- If the task requires multiple rounds of exploration, or you want to keep a long search out of your own context
- If the task's output is a summary rather than a set of edits you must review

Usage notes:
- Always include a short description (3-5 words) summarizing what the agent will do
- The subagent runs its own loop and returns only its final message, so the search does not fill your context
- A subagent's transcript is recorded separately and does not interleave into this conversation`

type agentTool struct {
	spec[AgentInput]
}

func (agentTool) Call(ctx context.Context, in AgentInput, uc *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return errResult("agent: prompt is required"), nil
	}
	if strings.TrimSpace(in.Description) == "" {
		return errResult("agent: description is required"), nil
	}

	runner := currentAgentRunner()
	if runner == nil {
		return errResult(
			"agent: this engine has no subagent runner installed in this mode. " +
				"Carry out the task in this conversation instead."), nil
	}

	run := AgentRun{
		Description:  in.Description,
		Prompt:       in.Prompt,
		SubagentType: in.SubagentType,
		Model:        in.Model,
	}
	if uc != nil {
		run.SessionID = uc.SessionID
		run.WorkDir = uc.WorkDir
	}

	out, err := runner(ctx, run)
	if err != nil {
		return errResult("agent: %v", err), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(the subagent produced no text)"
	}
	return okResult(out, map[string]any{
		"description":  in.Description,
		"subagentType": defaultString(in.SubagentType, "general-purpose"),
		"prompt":       in.Prompt,
	}), nil
}

// ---------------------------------------------------------------------------
// TaskOutput
// ---------------------------------------------------------------------------

// TaskOutputInput mirrors TaskOutputTool's schema.
type TaskOutputInput struct {
	TaskID  string `json:"task_id"`
	Block   *bool  `json:"block,omitempty"`
	Timeout *int   `json:"timeout,omitempty"`
}

// TaskOutput is what a task's output provider returns.
type TaskOutput struct {
	TaskID      string `json:"task_id"`
	TaskType    string `json:"task_type"`
	Status      string `json:"status"`
	Description string `json:"description"`
	Output      string `json:"output"`
	Error       string `json:"error,omitempty"`
}

// TaskOutputProvider looks up a background task.
//
// Returns false when no task has that id, which is a different answer from a
// task that exists and has produced nothing — the model should retry one and
// stop waiting for the other.
type TaskOutputProvider func(taskID string) (TaskOutput, bool)

var (
	taskMu       sync.RWMutex
	taskProvider TaskOutputProvider
)

// SetTaskOutputProvider installs the lookup. Nil disables the tool.
func SetTaskOutputProvider(p TaskOutputProvider) {
	taskMu.Lock()
	defer taskMu.Unlock()
	taskProvider = p
}

func currentTaskProvider() TaskOutputProvider {
	taskMu.RLock()
	defer taskMu.RUnlock()
	return taskProvider
}

const taskOutputSchema = `{
  "type": "object",
  "properties": {
    "task_id": {
      "type": "string",
      "description": "The task ID to get output from"
    },
    "block": {
      "type": "boolean",
      "default": true,
      "description": "Whether to wait for completion"
    },
    "timeout": {
      "type": "number",
      "minimum": 0,
      "maximum": 600000,
      "default": 30000,
      "description": "Max wait time in ms"
    }
  },
  "required": ["task_id"],
  "additionalProperties": false
}`

const taskOutputDescription = `- Retrieves output from a running or completed task
- Takes a task_id parameter identifying the task
- Returns the task output along with status information
- Use this tool when you need to check on a background task's progress

NOTE: this engine has no background task registry yet (that is T2.5), so this
tool answers only for tasks the current session started and still knows about.
If it reports an unknown id, that is the reason — it does not mean the task
failed.`

type taskOutputTool struct {
	spec[TaskOutputInput]
}

func (taskOutputTool) Call(_ context.Context, in TaskOutputInput, _ *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	id := strings.TrimSpace(in.TaskID)
	if id == "" {
		return errResult("taskoutput: task_id is required"), nil
	}

	provider := currentTaskProvider()
	if provider == nil {
		return errResult(
			"taskoutput: this engine has no task registry in this mode, so %q cannot be looked up. "+
				"Background work is not tracked across calls here.", id), nil
	}

	// A block request within the tool's own timeout window. The poll loop is
	// deliberately short-lived: a tool that can wait ten minutes is a tool that
	// can hold a turn open with nothing to show.
	timeout := 30 * time.Second
	if in.Timeout != nil {
		timeout = time.Duration(*in.Timeout) * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	block := in.Block == nil || *in.Block

	for {
		out, ok := provider(id)
		if !ok {
			return errResult("taskoutput: no task with id %q in this session", id), nil
		}
		// Return as soon as the task is done, the caller asked not to wait, or
		// the wait has gone on long enough. The loop cannot spin: the deadline
		// always arrives.
		if !block || out.Status != "running" || time.Now().After(deadline) {
			return formatTaskOutput(out), nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func formatTaskOutput(out TaskOutput) *tool.Result {
	var b strings.Builder
	b.WriteString("Task " + out.TaskID + " (" + out.TaskType + "): " + out.Status + "\n")
	if out.Description != "" {
		b.WriteString(out.Description + "\n")
	}
	if out.Output != "" {
		b.WriteString("\n" + out.Output)
	}
	res := &tool.Result{Content: truncateBytes(b.String(), maxOutputBytes), Metadata: map[string]any{
		"taskId": out.TaskID, "status": out.Status, "taskType": out.TaskType,
	}}
	res.IsError = out.Status == "failed"
	return res
}

func defaultString(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[AgentInput](agentTool{spec: spec[AgentInput]{
		Name_:     "Agent",
		Aliases_:  []string{"Task"},
		Hint:      "delegate a multi-step task to a subagent",
		Desc:      agentDescription,
		Schema:    agentSchema,
		ReadOnly:  true,
		OpenWorld: true,
		// Not concurrency-safe: several subagents editing the same tree at once
		// is a way to produce conflicts the user then has to untangle.
		ConcurrencySafe: false,
		SearchRead:      tool.SearchRead{Search: true},
	}}))

	tool.RegisterBuiltin(tool.Adapt[TaskOutputInput](taskOutputTool{spec: spec[TaskOutputInput]{
		Name_:           "TaskOutput",
		Hint:            "read a background task's output",
		Desc:            taskOutputDescription,
		Schema:          taskOutputSchema,
		ReadOnly:        true,
		ConcurrencySafe: true,
		SearchRead:      tool.SearchRead{Read: true},
	}}))
}
