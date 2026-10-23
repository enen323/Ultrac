// pi_tree.go — the whole-tree views the RPC branch commands serve
// (docs/compaction-branch-plan.md P-C4): getTree's node shape, the fork
// selector's candidate list, and the pure decision a fork makes.
//
// # Why package-level functions, not manager methods
//
// Every function here is a pure walk over an entry slice: the caller hands
// it TreeEntries() (the pointer view, same contract as GetBranch) and gets
// plain data back. That keeps the manager's locked surface small, makes the
// tree testable without a session at all, and leaves the mutating half
// (Branch / ResetLeaf) where it has always been — pi_manager.go.
//
// pi's own getTree lives on SessionManager (session-manager.ts:1529-1567)
// and getUserMessagesForForking on the agent-session layer
// (agent-session.ts:4186-4201); the split here is Ultrac's, the algorithms
// are pi's.

package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"ultrac/internal/ai"
)

// TreeNode is one node of get_tree's response (pi's SessionTreeNode,
// session-manager.ts:199-204).
//
// Entry is the entry line as JSON — MarshalEntry's output, so every field
// the file carries (Extra included: compaction summaries, branch-summary
// bodies, usage) reaches the client verbatim, the way pi's node does. A
// struct would drop Extra (json:"-"), which is why this is raw JSON.
//
// Label is pi's resolved label for the entry. This version never writes
// label entries, so it is always absent; the field exists so the wire
// shape matches pi's and a future label writer fills it without a
// protocol change (plan D8).
type TreeNode struct {
	Entry    json.RawMessage `json:"entry"`
	Children []*TreeNode     `json:"children"`
	Label    string          `json:"label,omitempty"`

	// stamp is the sort key for children (the entry's timestamp). It is
	// unexported on purpose: the wire never sees it.
	stamp string
}

// BuildTree assembles the session's entries into a forest (pi's getTree,
// session-manager.ts:1529-1567).
//
// Three rules carry the semantics:
//
//   - A well-formed session has exactly one root (the entry whose parentId
//     is null). Orphans — entries whose parentId points at an id the file
//     does not contain — are returned as roots too, so a damaged chain
//     still shows its entries instead of silently swallowing them.
//   - An entry whose parentId is its own id is a root (pi's explicit
//     guard against a self-loop wedging the walk).
//   - Children sort by timestamp, oldest first — the file order a reader
//     expects, since appends land at the end but a branch's children can
//     interleave in file order. The sort is iterative so a deep tree
//     cannot overflow the stack (pi's own note).
func BuildTree(entries []*SessionEntry) ([]*TreeNode, error) {
	nodes := make(map[string]*TreeNode, len(entries))
	for _, e := range entries {
		line, err := MarshalEntry(e)
		if err != nil {
			return nil, fmt.Errorf("session: encode entry %s: %w", e.ID, err)
		}
		nodes[e.ID] = &TreeNode{
			Entry: json.RawMessage(bytes.TrimSuffix(line, []byte("\n"))),
			stamp: e.Timestamp,
		}
	}

	var roots []*TreeNode
	for _, e := range entries {
		node := nodes[e.ID]
		if e.ParentID == nil || *e.ParentID == e.ID {
			roots = append(roots, node)
			continue
		}
		parent := nodes[*e.ParentID]
		if parent == nil {
			// Orphan: the parent chain is broken. pi treats it as a root
			// rather than dropping the entry.
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}

	// Iterative post-order-free walk: pop a node, sort its children, push
	// them. Timestamps are fixed-width RFC3339 (NowISO / toISOString), so
	// lexicographic order is chronological order.
	stack := append([]*TreeNode(nil), roots...)
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		sort.SliceStable(node.Children, func(i, j int) bool {
			return node.Children[i].stamp < node.Children[j].stamp
		})
		stack = append(stack, node.Children...)
	}
	return roots, nil
}

// ForkMessage is one candidate in the fork selector (pi's
// {entryId, text} pair, agent-session.ts:4186-4201).
type ForkMessage struct {
	EntryID string `json:"entryId"`
	Text    string `json:"text"`
}

// UserForkMessages lists every user message in the session, in file order
// — the fork selector's options (pi's getUserMessagesForForking).
//
// A user message with no text is skipped, exactly as pi skips it: the
// selector exists to re-edit a prompt, and an image-only message has no
// prompt to put back in the editor.
func UserForkMessages(entries []*SessionEntry) []ForkMessage {
	out := make([]ForkMessage, 0, len(entries))
	for _, e := range entries {
		if e.Type != TypeMessage {
			continue
		}
		if EntryMessageRole(e) != string(ai.RoleUser) {
			continue
		}
		msg, ok := EntryMessage(e)
		if !ok {
			continue
		}
		text := ai.MessageText(msg)
		if text == "" {
			continue
		}
		out = append(out, ForkMessage{EntryID: e.ID, Text: text})
	}
	return out
}

// ForkMove is where a fork lands the leaf, and what the client gets back.
//
// LeafID "" means the resetLeaf position: before any entry, so the next
// append becomes a new root. EditorText is the picked user message's text
// — the shell puts it back in the composer — and is empty for non-user
// targets. NoOp marks the target that already is the leaf: nothing moves
// (pi's navigateTree early return, agent-session.ts:4005-4007).
type ForkMove struct {
	LeafID     string
	EditorText string
	NoOp       bool
}

// PlanFork resolves one fork request against the tree (pi's navigateTree
// decision half, agent-session.ts:4113-4128, without the summary branch —
// that is plan D7's "no-summary fork").
//
// The rule: a user message target moves the leaf to its parent (null for a
// root message) and hands the text back for re-editing; any other target
// moves the leaf to the target itself. This is deliberately more
// permissive than pi's runtimeHost.fork, which refuses non-user targets
// outright ("Invalid entry ID for forking", agent-session-runtime.ts:282)
// — the RPC command serves the tree navigator's semantics, where
// navigating to any entry is legitimate.
//
// The missing-entry error is pi's wire literal, kept verbatim so the
// double-run compares equal strings.
func PlanFork(entries []*SessionEntry, targetID, leafID string) (ForkMove, error) {
	var target *SessionEntry
	for _, e := range entries {
		if e.ID == targetID {
			target = e
			break
		}
	}
	if target == nil {
		//lint:ignore ST1005 wire literal matches pi
		return ForkMove{}, fmt.Errorf("Invalid entry ID for forking")
	}
	if targetID == leafID {
		return ForkMove{LeafID: leafID, NoOp: true}, nil
	}
	if target.Type == TypeMessage && EntryMessageRole(target) == string(ai.RoleUser) {
		move := ForkMove{}
		if target.ParentID != nil {
			move.LeafID = *target.ParentID
		}
		if msg, ok := EntryMessage(target); ok {
			move.EditorText = ai.MessageText(msg)
		}
		return move, nil
	}
	return ForkMove{LeafID: target.ID}, nil
}
