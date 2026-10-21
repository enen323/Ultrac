package compact

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"ultrac/internal/ai"
)

// Conversation serialization and file-operation tracking — pi's
// compaction/utils.ts, in one file.
//
// Both exist for the same reason: the summarization request must not look
// like a conversation to continue. The serializer renders the transcript as
// plainly-labelled text (so the model cannot mistake it for its own turns),
// and the file tracker distills which paths were touched so the checkpoint
// can carry a compact inventory of them.

// toolResultMaxChars caps a serialized tool result. Full content is not
// needed to summarize what happened, and an uncapped result can make the
// summarization request larger than the context it summarizes.
//
// pi: utils.ts:94 (TOOL_RESULT_MAX_CHARS).
const toolResultMaxChars = 2000

// SerializeConversation renders LLM messages as labelled text for the
// summarization prompt.
//
// pi: serializeConversation (utils.ts:120-155). Two deliberate omissions:
// thinking blocks (replayed thinking is often not part of the model's live
// context — a block that does not match its request is dropped — and
// including it can make the request larger than the context it summarizes)
// and system messages (prompt state, not conversation; the compaction entry
// carries the boundary snapshot instead).
//
// The input is the session projection's output, which is already LLM-shaped:
// pi calls convertToLlm first, but Ultrac's projection folds custom entry
// types into user messages on read (pi_context.go contextMessages).
func SerializeConversation(messages []ai.Message) string {
	var parts []string
	for _, msg := range messages {
		switch msg.Role {
		case ai.RoleUser:
			if content := contentText(msg, ""); content != "" {
				parts = append(parts, "[User]: "+content)
			}
		case ai.RoleAssistant:
			var toolCalls []string
			for _, block := range msg.Content {
				if block.Type != ai.BlockToolCall {
					continue
				}
				toolCalls = append(toolCalls, block.Name+"("+formatToolCallArgs(block.Arguments)+")")
			}
			if hasTextBlock(msg.Content) {
				parts = append(parts, "[Assistant]: "+contentText(msg, "\n"))
			}
			if len(toolCalls) > 0 {
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(toolCalls, "; "))
			}
		case ai.RoleToolResult:
			if content := contentText(msg, ""); content != "" {
				parts = append(parts, "[Tool result]: "+truncateForSummary(content, toolResultMaxChars))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// contentText joins a message's text blocks with sep — pi's contentText
// (utils/text.ts:6-12), which the serializer calls with "" for user and
// toolResult content and "\n" for assistant content.
func contentText(m ai.Message, sep string) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == ai.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, sep)
}

func hasTextBlock(blocks []ai.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == ai.BlockText {
			return true
		}
	}
	return false
}

// formatToolCallArgs renders a tool call's arguments as k=v pairs.
//
// pi: utils.ts:133-135. One unavoidable deviation: pi iterates
// Object.entries, which preserves the JSON object's insertion order, while a
// Go map does not — so the keys are sorted. The rendering is informational
// (the arguments also appear verbatim in the serialized tool call block), and
// determinism matters more here than matching an order no reader depends on.
func formatToolCallArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		value, err := json.Marshal(args[k])
		if err != nil {
			continue // a value json cannot render carries no information here
		}
		parts = append(parts, k+"="+string(value))
	}
	return strings.Join(parts, ", ")
}

// truncateForSummary keeps the head of an oversized text and reports how much
// was cut.
//
// pi: utils.ts:100-104.
func truncateForSummary(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	truncated := len(text) - maxChars
	return text[:maxChars] + "\n\n[... " + strconv.Itoa(truncated) + " more characters truncated]"
}

// ============================================================================
// File operation tracking
// ============================================================================

// FileOperations accumulates the paths a set of messages touched.
//
// pi: FileOperations (utils.ts:12-16). The three sets stay separate until
// ComputeFileLists resolves them: a file both read and written is modified,
// not read.
type FileOperations struct {
	Read    map[string]bool
	Written map[string]bool
	Edited  map[string]bool
}

// CreateFileOps returns an empty accumulator.
//
// pi: createFileOps (utils.ts:18-24).
func CreateFileOps() FileOperations {
	return FileOperations{
		Read:    make(map[string]bool),
		Written: make(map[string]bool),
		Edited:  make(map[string]bool),
	}
}

// ExtractFileOpsFromMessage records the file operations one message implies.
//
// pi: extractFileOpsFromMessage (utils.ts:30-45). Tool results contribute
// through their nested calls (calls a codemode script made); assistant
// messages through their toolCall blocks. Every other role is ignored.
func ExtractFileOpsFromMessage(message ai.Message, fileOps FileOperations) {
	if message.Role == ai.RoleToolResult {
		if message.NestedCalls == nil {
			return
		}
		for _, call := range message.NestedCalls.Calls {
			addFileOp(call.Name, call.Arguments, fileOps)
		}
		return
	}
	if message.Role != ai.RoleAssistant {
		return
	}
	for _, block := range message.Content {
		if block.Type != ai.BlockToolCall {
			continue
		}
		addFileOp(block.Name, block.Arguments, fileOps)
	}
}

// addFileOp files one tool call under its verb: read / write / edit. Any
// other tool, or a call without a string "path", is not a file operation.
//
// pi: addFileOp (utils.ts:47-61).
func addFileOp(toolName string, args map[string]any, fileOps FileOperations) {
	path, _ := args["path"].(string)
	if path == "" {
		return
	}
	switch toolName {
	case "read":
		fileOps.Read[path] = true
	case "write":
		fileOps.Written[path] = true
	case "edit":
		fileOps.Edited[path] = true
	}
}

// ComputeFileLists resolves the accumulator into the two lists a compaction
// entry stores: files only read, and files modified (written or edited).
// Both lists are deduplicated and sorted; both are non-nil so they marshal
// as [] rather than null (pi's arrays are always present).
//
// pi: computeFileLists (utils.ts:67-72).
func ComputeFileLists(fileOps FileOperations) (readFiles, modifiedFiles []string) {
	readFiles = []string{}
	modifiedFiles = []string{}
	modified := make(map[string]bool, len(fileOps.Written)+len(fileOps.Edited))
	for f := range fileOps.Written {
		modified[f] = true
	}
	for f := range fileOps.Edited {
		modified[f] = true
	}
	for f := range fileOps.Read {
		if !modified[f] {
			readFiles = append(readFiles, f)
		}
	}
	for f := range modified {
		modifiedFiles = append(modifiedFiles, f)
	}
	sort.Strings(readFiles)
	sort.Strings(modifiedFiles)
	return readFiles, modifiedFiles
}

// FormatFileOperations renders the file lists as the XML sections appended to
// a summary.
//
// pi: formatFileOperations (utils.ts:77-87).
func FormatFileOperations(readFiles, modifiedFiles []string) string {
	var sections []string
	if len(readFiles) > 0 {
		sections = append(sections, "<read-files>\n"+strings.Join(readFiles, "\n")+"\n</read-files>")
	}
	if len(modifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+strings.Join(modifiedFiles, "\n")+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}
