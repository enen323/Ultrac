// pi_context.go — rebuilding the model context from a session tree.
//
// Port of the reading half of pi's session-manager.ts: buildSessionPath
// (:390-416), buildContextEntries (:476-512), sessionEntryToContextMessages
// (:439-466) and getSessionContextSettings (:418-433), plus the custom-role
// conversion from core/messages.ts convertToLlm (:148-196).
//
// # Why the walk and not the file order
//
// Entries chain through parentId. After a rewind the file still contains the
// messages that were abandoned, in the order they were first written, so
// reading top-to-bottom would replay a conversation that never happened
// (plan R7). The leaf pointer names the tip; the walk from it is the truth.
package session

import (
	"encoding/json"

	"ultrac/internal/ai"
)

// Summary framing constants — pi's core/messages.ts:11-24. A summary is
// stored as its own entry type and folded back into context as a user
// message with this exact framing, so a compacted transcript rebuilds
// identically in both implementations.
const (
	compactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactionSummarySuffix = "\n</summary>"
	branchSummaryPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	branchSummarySuffix     = "</summary>"
)

// buildPath walks from the leaf to the root and returns the path in order.
//
// leafID "" (pi's null) means "before any entry" and yields an empty path.
// The walk guards against a cycle: a corrupt parent chain stops rather than
// looping forever, same as the old BuildChain.
func buildPath(entries []*SessionEntry, leafID string, byID map[string]*SessionEntry) []*SessionEntry {
	if leafID == "" {
		return nil
	}
	var path []*SessionEntry
	seen := make(map[string]bool, len(entries))
	current, ok := byID[leafID]
	for ok {
		if current.ID != "" {
			if seen[current.ID] {
				break
			}
			seen[current.ID] = true
		}
		path = append(path, current)
		if current.ParentID == nil {
			break
		}
		current, ok = byID[*current.ParentID]
	}
	// Reverse in place: the walk collected leaf-first.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// buildContext is the package-level projection over an entry list, leaf and
// index. Both the manager and callers holding a bare entry slice (a resumed
// file, a test) use it.
func buildContext(entries []*SessionEntry, leafID string, byID map[string]*SessionEntry) SessionContext {
	path := buildPath(entries, leafID, byID)
	thinkingLevel, model := contextSettings(path)
	messages := contextMessages(contextEntries(path))
	return SessionContext{Messages: messages, ThinkingLevel: thinkingLevel, Model: model}
}

// contextSettings folds the path's state entries: the last thinking level
// change wins, and the model comes from the last model_change — or from the
// last assistant message when no model_change was ever recorded (pi's
// getSessionContextSettings, session-manager.ts:418-433).
func contextSettings(path []*SessionEntry) (thinkingLevel string, model *ModelRef) {
	thinkingLevel = "off"
	for _, e := range path {
		switch e.Type {
		case TypeThinkingLevelChange:
			thinkingLevel = e.ThinkingLevel
		case TypeModelChange:
			model = &ModelRef{Provider: e.Provider, ModelID: e.ModelID}
		case TypeMessage:
			if msg, ok := EntryMessage(e); ok && msg.Role == ai.RoleAssistant {
				model = &ModelRef{Provider: msg.Provider, ModelID: msg.Model}
			}
		}
	}
	return thinkingLevel, model
}

// contextEntries applies the compaction window: when the path contains a
// compaction entry, everything before the compaction's firstKeptEntryId is
// summarized away, system messages in the kept prefix are dropped (the
// compaction entry carries its own systemMessage), and the compaction entry
// itself leads the result.
//
// pi: buildContextEntries (session-manager.ts:476-512). Compaction itself is
// a non-goal this phase (plan §2.2) — this exists so a file pi compacted
// rebuilds the same context here, which is half of the P4 interop DoD.
func contextEntries(path []*SessionEntry) []*SessionEntry {
	var compaction *SessionEntry
	compactionIdx := -1
	for i := range path {
		if path[i].Type == TypeCompaction {
			compaction = path[i]
			compactionIdx = i
		}
	}
	if compaction == nil {
		return path
	}

	firstKept := compaction.ExtraString("firstKeptEntryId")
	out := []*SessionEntry{compaction}
	foundFirstKept := firstKept == ""
	for i := 0; i < compactionIdx; i++ {
		e := path[i]
		if !foundFirstKept && e.ID == firstKept {
			foundFirstKept = true
		}
		if !foundFirstKept {
			continue
		}
		if e.Type == TypeMessage && EntryMessageRole(e) == string(ai.RoleSystem) {
			continue
		}
		out = append(out, e)
	}
	out = append(out, path[compactionIdx+1:]...)
	return out
}

// contextMessages converts the selected entries into LLM messages.
//
// Non-message entries contribute what pi's convertToLlm (messages.ts:148-196)
// makes of them: a custom_message becomes a user message, branch and
// compaction summaries become user messages with the framing above. State
// entries (model_change, thinking_level_change, session_info, usage, label,
// custom, context_edit) contribute nothing.
func contextMessages(entries []*SessionEntry) []ai.Message {
	var out []ai.Message
	for _, e := range entries {
		switch e.Type {
		case TypeMessage:
			msg, ok := EntryMessage(e)
			if !ok {
				continue
			}
			out = append(out, repairMessageContent(msg))
		case TypeCustomMessage:
			// A custom message enters context as a user message; display and
			// details are TUI/extension metadata and stay out of the prompt.
			content, ok := customMessageContent(e)
			if !ok {
				continue
			}
			out = append(out, ai.Message{
				Role:        ai.RoleUser,
				Content:     content,
				TimestampMS: parseMillis(e.Timestamp),
			})
		case TypeBranchSummary:
			summary := e.ExtraString("summary")
			if summary == "" {
				continue
			}
			out = append(out, textUserMessage(branchSummaryPrefix+summary+branchSummarySuffix, e.Timestamp))
		case TypeCompaction:
			// The compaction entry may carry the complete system prompt state
			// at the boundary; it leads, then the summary as a user message.
			if sys, ok := compactionSystemMessage(e); ok {
				out = append(out, sys)
			}
			summary := e.ExtraString("summary")
			if summary == "" {
				continue
			}
			out = append(out, textUserMessage(compactionSummaryPrefix+summary+compactionSummarySuffix, e.Timestamp))
		}
	}
	return out
}

// repairMessageContent applies pi's sessionEntryToContextMessages tolerance
// (session-manager.ts:444-451): a message with null content is decoded into
// the empty form of its role rather than dropped, because old files, forks
// and hand edits all produce them.
func repairMessageContent(msg ai.Message) ai.Message {
	if msg.Content != nil {
		return msg
	}
	switch msg.Role {
	case ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult:
		msg.Content = []ai.ContentBlock{}
	}
	return msg
}

// customMessageContent reads a custom_message entry's content: a bare string
// becomes one text block, an array is decoded as blocks.
func customMessageContent(e *SessionEntry) ([]ai.ContentBlock, bool) {
	raw, ok := e.Extra["content"]
	if !ok {
		return nil, false
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false
		}
		return []ai.ContentBlock{ai.TextBlock(s)}, true
	}
	var blocks []ai.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// compactionSystemMessage decodes the systemMessage a compaction entry
// captured at its boundary.
func compactionSystemMessage(e *SessionEntry) (ai.Message, bool) {
	raw, ok := e.Extra["systemMessage"]
	if !ok {
		return ai.Message{}, false
	}
	var msg ai.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ai.Message{}, false
	}
	return msg, true
}

func textUserMessage(text, timestamp string) ai.Message {
	return ai.Message{
		Role:        ai.RoleUser,
		Content:     []ai.ContentBlock{ai.TextBlock(text)},
		TimestampMS: parseMillis(timestamp),
	}
}

// parseMillis reads an entry timestamp as Unix milliseconds; 0 when absent or
// unparseable. pi's `new Date(timestamp).getTime()` yields NaN for an invalid
// string; the message timestamp is informational, so 0 is the honest Go
// equivalent here.
func parseMillis(ts string) int64 {
	t := parseISO(ts)
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// ProjectedEntry pairs one context entry with the messages it contributes to
// the model — pi's ProjectedSessionEntry (session-manager.ts:209-214).
type ProjectedEntry struct {
	// Source is the raw append-only entry.
	Source *SessionEntry
	// Messages is what this entry is allowed to say in context. Empty for
	// state-only entries and for an older compaction retained inside a
	// newer one's window.
	Messages []ai.Message
}

// BuildProjection builds the compaction-aware projection over an entry path
// (the caller passes a root-first path, e.g. GetBranch's result).
//
// pi: buildSessionProjection (session-manager.ts:543-570). The newest
// compaction leads the result; an older compaction that sits inside its
// retained range contributes nothing — without that rule a twice-compacted
// session would replay both summaries into every request.
//
// This is the projection compaction planning runs on; BuildSessionContext
// stays the flattened form for the agent loop.
func BuildProjection(path []*SessionEntry) []ProjectedEntry {
	entries := contextEntries(path)
	out := make([]ProjectedEntry, len(entries))
	for i, e := range entries {
		if e.Type == TypeCompaction && i > 0 {
			out[i] = ProjectedEntry{Source: e}
			continue
		}
		out[i] = ProjectedEntry{Source: e, Messages: contextMessages([]*SessionEntry{e})}
	}
	return out
}

// ProjectedMessages flattens a projection into the message list it requests.
func ProjectedMessages(projected []ProjectedEntry) []ai.Message {
	var out []ai.Message
	for _, pe := range projected {
		out = append(out, pe.Messages...)
	}
	return out
}

// EntryContextMessages returns the context messages one entry contributes —
// the per-entry form of the projection (pi's sessionEntryToContextMessages,
// session-manager.ts:439-466).
func EntryContextMessages(e *SessionEntry) []ai.Message {
	return contextMessages([]*SessionEntry{e})
}
