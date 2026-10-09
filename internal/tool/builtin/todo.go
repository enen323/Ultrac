package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"ultrac/internal/contract/tool"
)

// TodoItem is one checklist entry. The field set is the TypeScript side's.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`
}

// TodoInput mirrors TodoWriteTool's schema.
type TodoInput struct {
	Todos []TodoItem `json:"todos"`
}

const todoSchema = `{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "The updated todo list",
      "items": {
        "type": "object",
        "properties": {
          "content": {
            "type": "string",
            "minLength": 1,
            "description": "The imperative form describing what needs to be done, e.g., \"Run tests\", \"Build the project\""
          },
          "status": {
            "type": "string",
            "enum": ["pending", "in_progress", "completed"],
            "description": "The task status"
          },
          "activeForm": {
            "type": "string",
            "minLength": 1,
            "description": "The present continuous form shown while the task is in progress, e.g., \"Running tests\", \"Building the project\""
          }
        },
        "required": ["content", "status", "activeForm"],
        "additionalProperties": false
      }
    }
  },
  "required": ["todos"],
  "additionalProperties": false
}`

const todoDescription = `Use this tool to create and manage a structured task list for your current coding session. This helps you track progress, organize complex tasks, and demonstrate thoroughness to the user.

## When to Use This Tool

Use this tool proactively in these scenarios:
- Complex multi-step tasks - When a task requires 3 or more distinct steps or actions
- Non-trivial and complex tasks - Tasks that require careful planning or multiple operations
- Plan mode - When using plan mode, create a task list to track the work
- User explicitly requests task list - When the user directly asks you to use the task management
- User provides multiple tasks - When users provide a list of things to be done

Skip using this tool when:
- There is only a single, straightforward task
- The task is trivial and tracking it provides no organizational benefit
- The task can be completed in less than 3 trivial steps
- The task is purely conversational or informational

## Task States and Management

1. **Task States**: Use these states to track progress:
   - pending: Task not yet started
   - in_progress: Currently working on (limit to ONE task at a time)
   - completed: Task finished successfully

2. **Task Management**:
   - Update task status in real-time as you work
   - Mark tasks complete IMMEDIATELY after finishing (don't batch up multiple task completions)
   - Only mark a task as completed when you have FULLY accomplished it
   - If you encounter errors, blockers, or cannot finish, keep the task as in_progress
   - When blocked, create a new task describing what needs to be resolved

3. **Task Breakdown**:
   - Create specific, actionable items
   - Break complex tasks into smaller, manageable steps
   - Use clear, descriptive task names

## Examples

Example 1 — Creating a todo list for a multi-step feature:
{"todos": [{"content": "Add dark mode toggle to Settings page", "status": "pending", "activeForm": "Adding dark mode toggle to Settings page"}]}

Example 2 — Marking a task in progress when starting work:
{"todos": [{"content": "Run tests and ensure they pass", "status": "in_progress", "activeForm": "Running tests and ensuring they pass"}]}

Example 3 — Marking a task completed after finishing it:
{"todos": [{"content": "Fix authentication bug in login flow", "status": "completed", "activeForm": "Fixing authentication bug in login flow"}]}`

// todoStore holds one checklist per session.
//
// A package-level map rather than a field threaded through the call: the tool
// contract has no slot for per-session state, and adding one would change the
// interface every other tool implements for the benefit of one. The session id
// in UseContext keys it, and the map is bounded because a session that ends
// should not leave a list behind — see ClearTodos, which the CLI calls.
var (
	todoMu    sync.Mutex
	todoStore = map[string][]TodoItem{}
)

// ClearTodos drops a session's list. Called when a session ends.
func ClearTodos(sessionID string) {
	todoMu.Lock()
	defer todoMu.Unlock()
	delete(todoStore, sessionID)
}

// TodosFor returns a copy of a session's list, for the system prompt.
func TodosFor(sessionID string) []TodoItem {
	todoMu.Lock()
	defer todoMu.Unlock()
	out := make([]TodoItem, len(todoStore[sessionID]))
	copy(out, todoStore[sessionID])
	return out
}

// RenderTodos returns the session's checklist as prompt text, or "" when there
// is nothing to show.
func RenderTodos(sessionID string) string {
	items := TodosFor(sessionID)
	if len(items) == 0 {
		return ""
	}
	return renderTodos(items)
}

type todoTool struct {
	spec[TodoInput]
}

func (todoTool) Call(_ context.Context, in TodoInput, uc *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	for i, item := range in.Todos {
		if strings.TrimSpace(item.Content) == "" {
			return errResult("todo: todos[%d].content cannot be empty", i), nil
		}
		if strings.TrimSpace(item.ActiveForm) == "" {
			return errResult("todo: todos[%d].activeForm cannot be empty", i), nil
		}
		switch item.Status {
		case "pending", "in_progress", "completed":
		default:
			return errResult("todo: todos[%d].status must be pending, in_progress or completed; got %q",
				i, item.Status), nil
		}
	}

	key := ""
	if uc != nil {
		key = uc.SessionID
	}

	todoMu.Lock()
	old := todoStore[key]
	// An all-completed list is stored empty, matching the TypeScript side: the
	// checklist is a working aid, and a fully-struck-through list left on
	// screen is noise.
	allDone := len(in.Todos) > 0
	for _, item := range in.Todos {
		if item.Status != "completed" {
			allDone = false
			break
		}
	}
	stored := in.Todos
	if allDone {
		stored = []TodoItem{}
	}
	todoStore[key] = stored
	todoMu.Unlock()

	summary := renderTodos(in.Todos)
	meta := map[string]any{
		"oldTodos": jsonAny(old),
		"newTodos": jsonAny(in.Todos),
	}
	if allDone {
		return okResult(summary+"\n\nTodos have been modified successfully. All items are complete.", meta), nil
	}
	return okResult(summary+"\n\nTodos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable", meta), nil
}

// renderTodos shows the list back to the model.
//
// Echoing it is not decoration: the model wrote the list two turns ago, and
// seeing it rendered is what catches a status that was never advanced.
func renderTodos(items []TodoItem) string {
	var b strings.Builder
	b.WriteString("Todo list:\n")
	for _, item := range items {
		mark := "[ ]"
		switch item.Status {
		case "in_progress":
			mark = "[~]"
		case "completed":
			mark = "[x]"
		}
		b.WriteString(mark + " " + item.Content)
		if item.Status == "in_progress" && item.ActiveForm != "" {
			b.WriteString("  (" + item.ActiveForm + ")")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func jsonAny(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[TodoInput](todoTool{spec: spec[TodoInput]{
		Name_:           "TodoWrite",
		Hint:            "manage the session task checklist",
		Desc:            todoDescription,
		Schema:          todoSchema,
		ReadOnly:        false,
		ConcurrencySafe: false,
		SearchRead:      tool.SearchRead{List: true},
	}}))
}
