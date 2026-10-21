package compact

import (
	"strings"
	"testing"

	"ultrac/internal/ai"
)

func TestSerializeConversationLabels(t *testing.T) {
	messages := []ai.Message{
		ai.NewTextMessage(ai.RoleUser, "please fix the bug"),
		ai.NewTextMessage(ai.RoleAssistant, "on it"),
		{
			Role: ai.RoleAssistant,
			Content: []ai.ContentBlock{
				ai.TextBlock("reading the file"),
				ai.ToolCallBlock("call-1", "read", map[string]any{"path": "main.go"}),
			},
			StopReason: ai.StopToolUse,
		},
		ai.NewToolResultMessage("call-1", "read", "package main", false),
	}
	got := SerializeConversation(messages)
	want := strings.Join([]string{
		"[User]: please fix the bug",
		"[Assistant]: on it",
		"[Assistant]: reading the file",
		`[Assistant tool calls]: read(path="main.go")`,
		"[Tool result]: package main",
	}, "\n\n")
	if got != want {
		t.Fatalf("SerializeConversation =\n%q\nwant\n%q", got, want)
	}
}

func TestSerializeConversationOmitsThinkingAndSystem(t *testing.T) {
	// pi's rule (utils.ts:106-118): thinking is omitted, and system
	// messages never reach the serializer at all — the compaction entry
	// carries the boundary snapshot instead.
	messages := []ai.Message{
		ai.NewTextMessage(ai.RoleSystem, "you are a coding assistant"),
		{
			Role: ai.RoleAssistant,
			Content: []ai.ContentBlock{
				ai.ThinkingBlock("deliberating", "sig", false),
				ai.TextBlock("answer"),
			},
		},
	}
	got := SerializeConversation(messages)
	if got != "[Assistant]: answer" {
		t.Fatalf("SerializeConversation = %q, want only the text block", got)
	}
}

func TestSerializeConversationSkipsEmptyContent(t *testing.T) {
	// An empty user or tool result contributes no line at all (pi:
	// `if (content)`).
	messages := []ai.Message{
		ai.NewTextMessage(ai.RoleUser, ""),
		ai.NewToolResultMessage("call-1", "read", "", false),
		ai.NewTextMessage(ai.RoleUser, "real"),
	}
	if got := SerializeConversation(messages); got != "[User]: real" {
		t.Fatalf("SerializeConversation = %q", got)
	}
}

func TestSerializeConversationTruncatesToolResults(t *testing.T) {
	// pi: utils.ts:93-104 — 2000 characters, with a marker reporting the cut.
	long := strings.Repeat("x", 2500)
	got := SerializeConversation([]ai.Message{ai.NewToolResultMessage("c", "read", long, false)})
	want := "[Tool result]: " + strings.Repeat("x", 2000) + "\n\n[... 500 more characters truncated]"
	if got != want {
		t.Fatalf("truncation wrong: got %d chars, want %d", len(got), len(want))
	}
}

func TestSerializeConversationToolCallArguments(t *testing.T) {
	// Arguments render as k=v with JSON-encoded values, sorted by key
	// (Go maps have no insertion order; see formatToolCallArgs).
	messages := []ai.Message{{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			ai.ToolCallBlock("c", "edit", map[string]any{"path": "a.go", "old": "x", "new": "y"}),
			ai.ToolCallBlock("c2", "bash", map[string]any{"command": "ls"}),
		},
	}}
	got := SerializeConversation(messages)
	want := `[Assistant tool calls]: edit(new="y", old="x", path="a.go"); bash(command="ls")`
	if got != want {
		t.Fatalf("SerializeConversation = %q, want %q", got, want)
	}
}

func TestExtractFileOpsFromAssistantToolCalls(t *testing.T) {
	ops := CreateFileOps()
	messages := []ai.Message{
		{Role: ai.RoleAssistant, Content: []ai.ContentBlock{
			ai.ToolCallBlock("c0", "read", map[string]any{"path": "main.go"}), // read, then modified below
			ai.ToolCallBlock("c1", "read", map[string]any{"path": "go.mod"}),
			ai.ToolCallBlock("c2", "write", map[string]any{"path": "main.go"}),
			ai.ToolCallBlock("c3", "edit", map[string]any{"path": "main.go"}),
			ai.ToolCallBlock("c4", "bash", map[string]any{"command": "ls"}),   // not a file op
			ai.ToolCallBlock("c5", "read", map[string]any{"file": "no path"}), // no path
		}},
		ai.NewTextMessage(ai.RoleUser, "and then?"),
	}
	for _, m := range messages {
		ExtractFileOpsFromMessage(m, ops)
	}
	if !ops.Read["go.mod"] || !ops.Read["main.go"] {
		t.Errorf("read set = %v", ops.Read)
	}
	if !ops.Written["main.go"] || !ops.Edited["main.go"] {
		t.Errorf("write/edit sets = %v / %v", ops.Written, ops.Edited)
	}
	if len(ops.Read) != 2 {
		t.Errorf("read set has %d entries, want 2 (bash and pathless calls ignored)", len(ops.Read))
	}
}

func TestExtractFileOpsFromNestedToolCalls(t *testing.T) {
	// Calls a codemode script made are recorded on the script's tool
	// result (pi: utils.ts:32-35).
	ops := CreateFileOps()
	ExtractFileOpsFromMessage(ai.Message{
		Role: ai.RoleToolResult,
		NestedCalls: &ai.NestedToolCalls{
			Calls: []ai.NestedToolCallRecord{
				{Name: "read", Arguments: map[string]any{"path": "a.go"}},
				{Name: "edit", Arguments: map[string]any{"path": "b.go"}},
			},
		},
	}, ops)
	if !ops.Read["a.go"] || !ops.Edited["b.go"] {
		t.Fatalf("nested ops not recorded: %v / %v", ops.Read, ops.Edited)
	}
}

func TestComputeFileLists(t *testing.T) {
	ops := CreateFileOps()
	ops.Read["b.go"] = true
	ops.Read["a.go"] = true
	ops.Read["main.go"] = true // read AND modified
	ops.Written["main.go"] = true
	ops.Edited["z.go"] = true

	readFiles, modifiedFiles := ComputeFileLists(ops)
	if strings.Join(readFiles, ",") != "a.go,b.go" {
		t.Errorf("readFiles = %v, want [a.go b.go] (deduped, sorted, modified excluded)", readFiles)
	}
	if strings.Join(modifiedFiles, ",") != "main.go,z.go" {
		t.Errorf("modifiedFiles = %v, want [main.go z.go]", modifiedFiles)
	}
	if readFiles == nil || modifiedFiles == nil {
		t.Error("lists must be non-nil so they marshal as [] (pi's arrays are always present)")
	}
}

func TestComputeFileListsEmpty(t *testing.T) {
	readFiles, modifiedFiles := ComputeFileLists(CreateFileOps())
	if len(readFiles) != 0 || len(modifiedFiles) != 0 {
		t.Fatalf("lists = %v / %v, want empty", readFiles, modifiedFiles)
	}
}

func TestFormatFileOperations(t *testing.T) {
	if got := FormatFileOperations(nil, nil); got != "" {
		t.Errorf("empty = %q, want no section at all", got)
	}
	got := FormatFileOperations([]string{"a.go"}, nil)
	want := "\n\n<read-files>\na.go\n</read-files>"
	if got != want {
		t.Errorf("read only = %q, want %q", got, want)
	}
	got = FormatFileOperations([]string{"a.go", "b.go"}, []string{"m.go"})
	want = "\n\n<read-files>\na.go\nb.go\n</read-files>\n\n<modified-files>\nm.go\n</modified-files>"
	if got != want {
		t.Errorf("both = %q, want %q", got, want)
	}
}
