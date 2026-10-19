// list.go — session discovery: one project's transcripts, summarized for a
// client that needs to show and resume them (the desktop shell's session
// sidebar, docs/desktop-shell-plan.md §1 P-D0).
//
// # Why this lives in the session package
//
// The two things a listing needs — the directory layout and the "what was
// this session about" title — are both defined here: the layout by
// SessionDirFor/SessionFileName (pi_format.go) and the title by the same
// first-user-message rule the CLI's `ultrac sessions` command prints
// (main.go's sessionTitle, sunk here as SessionTitle so the two share one
// implementation instead of drifting).
//
// # Cost
//
// A listing parses every .jsonl in the project's session directory in full
// (ReadSessionFile), because the title, the message count and the display
// name all live in the entries. That is O(total bytes on disk) with a 50MB
// per-file ceiling (maxSessionFileBytes); a project with hundreds of large
// transcripts pays for it on every list_sessions call. The alternative —
// a sidecar index — is a cache-invalidation problem this phase does not
// need. Files that fail to parse are skipped, the same best-effort rule
// findMostRecentSession applies.
package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ultrac/internal/ai"
)

// sessionTitleMaxLen caps a derived title, matching the CLI's truncation.
const sessionTitleMaxLen = 48

// SessionSummary is one listed session: its identity, its recency, and the
// metadata a sidebar renders. It is also the list_sessions RPC command's
// wire shape (internal/rpc), which is why the fields carry JSON tags.
type SessionSummary struct {
	SessionID string `json:"sessionId"`
	// Path is the transcript file — the handle resume_session takes.
	Path string `json:"path"`
	// Title is the first user message, truncated (SessionTitle), or the
	// literal "(no user message)" for a session that never got a prompt.
	// It is always derived, never overridden: a session that also carries
	// a display name reports it separately in Name, and the client picks
	// which to show.
	Title string `json:"title"`
	// Timestamp is the header's creation timestamp (ISO 8601, pi's shape).
	Timestamp string `json:"timestamp"`
	// MessageCount counts the transcript's message entries — the system
	// patches included. It is the file's size in messages, which is what a
	// sidebar shows. get_state's messageCount is the live agent's, which
	// carries one more: the tool-loadout seed, which is never persisted
	// (it lives in the agent's initial state, not the file).
	MessageCount int `json:"messageCount"`
	// Name is the latest session_info display name, "" when unset.
	Name string `json:"name,omitempty"`
	// CWD is the header's recorded working directory, verbatim.
	CWD string `json:"cwd,omitempty"`
}

// ListSessions summarizes the session files for cwd, newest first.
//
// "Newest" is file mtime — the same recency findMostRecentSession uses —
// so a session that was resumed and appended to sorts above one merely
// created later. Files that cannot be read or parsed are skipped rather
// than failing the listing: one corrupt transcript must not blind the
// client to every other session.
func ListSessions(cwd string) ([]SessionSummary, error) {
	resolved := resolveSessionPath(cwd)
	dir := SessionDirFor(resolved)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no sessions yet: an empty list, not an error
		}
		return nil, err
	}

	type candidate struct {
		path  string
		mtime time.Time
	}
	var candidates []candidate
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{path: filepath.Join(dir, e.Name()), mtime: info.ModTime()})
	}
	// mtime desc; the name breaks ties deterministically (it embeds the
	// header timestamp, so equal mtimes still order chronologically).
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].mtime.Equal(candidates[j].mtime) {
			return candidates[i].mtime.After(candidates[j].mtime)
		}
		return candidates[i].path > candidates[j].path
	})

	out := make([]SessionSummary, 0, len(candidates))
	for _, c := range candidates {
		header, entries, err := ReadSessionFile(c.path)
		if err != nil || header == nil {
			continue // unreadable or headerless: not a session we can resume
		}
		out = append(out, SessionSummary{
			SessionID:    header.ID,
			Path:         c.path,
			Title:        SessionTitle(entries),
			Timestamp:    header.Timestamp,
			MessageCount: countMessageEntries(entries),
			Name:         latestSessionName(entries),
			CWD:          header.CWD,
		})
	}
	return out, nil
}

// SessionTitle is the first user message's leading text, truncated — the
// "what was this session about" line, read out of pi-shaped entries.
//
// Sunk from the CLI's print path (main.go) so `ultrac sessions` and the
// RPC list_sessions command show the same title for the same file.
func SessionTitle(entries []*SessionEntry) string {
	for _, e := range entries {
		if e == nil || e.Type != TypeMessage {
			continue
		}
		msg, ok := EntryMessage(e)
		if !ok || msg.Role != ai.RoleUser {
			continue
		}
		for _, b := range msg.Content {
			if b.Text == "" {
				continue
			}
			title := strings.Join(strings.Fields(b.Text), " ")
			if len(title) > sessionTitleMaxLen {
				title = title[:sessionTitleMaxLen] + "..."
			}
			return title
		}
	}
	return "(no user message)"
}

// countMessageEntries counts the file's message entries, system patches
// included — the transcript's size in messages. Bookkeeping entries
// (model_change, session_info, …) are not messages and not counted.
func countMessageEntries(entries []*SessionEntry) int {
	n := 0
	for _, e := range entries {
		if e != nil && e.Type == TypeMessage {
			n++
		}
	}
	return n
}

// latestSessionName returns the last session_info name, "" when unset —
// the same last-wins rule SessionManager.SessionName applies to the live
// manager, on raw entries.
func latestSessionName(entries []*SessionEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i] != nil && entries[i].Type == TypeSessionInfo {
			return strings.TrimSpace(entries[i].Name)
		}
	}
	return ""
}
