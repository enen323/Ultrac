package session

// list_test.go — the discovery surface the RPC list_sessions command and
// the desktop shell's sidebar sit on (docs/desktop-shell-plan.md §1, P-D0).
//
// The sessions are real files written by the real manager: a listing that
// only works against hand-built structs proves nothing about the thing
// clients actually call. The config home is redirected at a temp root
// (setupHomeDir) so nothing touches ~/.ultrac.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ultrac/internal/ai"
)

// writeExchange appends one user/assistant exchange, which is what flushes
// the file (the lazy rule: nothing lands on disk until the first
// conversation message).
func writeExchange(t *testing.T, m *SessionManager, prompt, answer string) {
	t.Helper()
	for _, msg := range []ai.Message{
		{Role: ai.RoleUser, Content: []ai.ContentBlock{ai.TextBlock(prompt)}, TimestampMS: time.Now().UnixMilli()},
		{Role: ai.RoleAssistant, Content: []ai.ContentBlock{ai.TextBlock(answer)}, TimestampMS: time.Now().UnixMilli()},
	} {
		if _, err := m.AppendMessage(msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
}

func TestListSessionsNewestFirstWithMetadata(t *testing.T) {
	setupHomeDir(t)
	cwd := `D:\repo`

	older := CreateSession(cwd)
	writeExchange(t, older, "the older question", "the older answer")

	newer := CreateSession(cwd)
	// A sections patch, as every session that has chatted carries one: the
	// count includes system messages, so this session reports 3 and the
	// older one (no patch) reports 2.
	if _, err := newer.AppendMessage(ai.Message{
		Role:        ai.RoleSystem,
		Content:     []ai.ContentBlock{ai.TextBlock("")},
		Sections:    map[string]*string{"preamble": strPtr("be brief")},
		TimestampMS: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("AppendMessage (patch): %v", err)
	}
	writeExchange(t, newer, "the newer question", "the newer answer")
	if _, err := newer.AppendSessionInfo("named session"); err != nil {
		t.Fatalf("AppendSessionInfo: %v", err)
	}

	// mtime ordering: the two files are written within the same
	// millisecond often enough that the tie-break cannot be trusted to
	// express intent — set the times explicitly.
	now := time.Now()
	if err := os.Chtimes(older.SessionFile(), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer.SessionFile(), now, now); err != nil {
		t.Fatal(err)
	}

	list, err := ListSessions(cwd)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(list))
	}

	first := list[0]
	if first.SessionID != newer.SessionID() {
		t.Errorf("newest first: got %s, want %s", first.SessionID, newer.SessionID())
	}
	if first.Path != newer.SessionFile() {
		t.Errorf("path = %q, want %q", first.Path, newer.SessionFile())
	}
	if first.Title != "the newer question" {
		t.Errorf("title = %q, want the first user message", first.Title)
	}
	if first.MessageCount != 3 {
		t.Errorf("messageCount = %d, want 3 (the system patch + user + assistant)", first.MessageCount)
	}
	if first.Name != "named session" {
		t.Errorf("name = %q, want the session_info name", first.Name)
	}
	if first.CWD != cwd {
		t.Errorf("cwd = %q, want %q (verbatim from the header)", first.CWD, cwd)
	}
	if first.Timestamp == "" {
		t.Error("timestamp must carry the header's creation time")
	}

	second := list[1]
	if second.SessionID != older.SessionID() {
		t.Errorf("second = %s, want the older session", second.SessionID)
	}
	if second.MessageCount != 2 {
		t.Errorf("older messageCount = %d, want 2 (no system patch in that file)", second.MessageCount)
	}
	if second.Name != "" {
		t.Errorf("name = %q, want absent (omitempty) when unset", second.Name)
	}
}

func TestListSessionsEmptyIsNotAnError(t *testing.T) {
	setupHomeDir(t)
	// A cwd that has never chatted: the directory does not exist yet.
	list, err := ListSessions(`D:\never-used`)
	if err != nil {
		t.Fatalf("ListSessions on an unused cwd: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("listed %d sessions, want 0", len(list))
	}
}

func TestListSessionsSkipsUnreadableFiles(t *testing.T) {
	setupHomeDir(t)
	cwd := `D:\repo`
	good := CreateSession(cwd)
	writeExchange(t, good, "a real session", "a real answer")

	// Two shapes that are not sessions: pure garbage, and entries with no
	// header (the second is the one OpenSession refuses, so a client must
	// never be offered it as resumable).
	dir := SessionDirFor(resolveSessionPath(cwd))
	if err := os.WriteFile(filepath.Join(dir, "garbage.jsonl"), []byte("not json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "headerless.jsonl"),
		[]byte(`{"type":"message","id":"x1","parentId":null,"timestamp":"2026-10-19T08:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"orphan"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := ListSessions(cwd)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d sessions, want 1 (the unreadable ones skipped)", len(list))
	}
	if list[0].SessionID != good.SessionID() {
		t.Errorf("listed %s, want the readable session", list[0].SessionID)
	}
}

func TestSessionTitleRules(t *testing.T) {
	msg := func(role ai.Role, text string) *SessionEntry {
		raw, err := json.Marshal(ai.Message{
			Role:        role,
			Content:     []ai.ContentBlock{ai.TextBlock(text)},
			TimestampMS: time.Now().UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return &SessionEntry{Type: TypeMessage, ID: "e1", Message: raw}
	}

	t.Run("first user message wins over later ones", func(t *testing.T) {
		got := SessionTitle([]*SessionEntry{msg(ai.RoleAssistant, "an answer"), msg(ai.RoleUser, "the question")})
		if got != "the question" {
			t.Errorf("title = %q, want the question", got)
		}
	})

	t.Run("whitespace collapses and long titles truncate", func(t *testing.T) {
		long := "this prompt is deliberately much longer than the truncation limit so it gets cut"
		got := SessionTitle([]*SessionEntry{msg(ai.RoleUser, "  spaced\n\nout\ttext  ")})
		if got != "spaced out text" {
			t.Errorf("title = %q, want collapsed whitespace", got)
		}
		got = SessionTitle([]*SessionEntry{msg(ai.RoleUser, long)})
		if len(got) != 48+3 || got[len(got)-3:] != "..." {
			t.Errorf("title = %q (%d chars), want 48 chars + ellipsis", got, len(got))
		}
	})

	t.Run("no user message falls back", func(t *testing.T) {
		if got := SessionTitle(nil); got != "(no user message)" {
			t.Errorf("title = %q, want the fallback", got)
		}
		if got := SessionTitle([]*SessionEntry{msg(ai.RoleAssistant, "only an answer")}); got != "(no user message)" {
			t.Errorf("title = %q, want the fallback", got)
		}
	})
}
