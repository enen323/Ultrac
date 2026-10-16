// pi_manager.go — SessionManager: the append-only session tree, pi's
// session-manager.ts core subset (docs/pi-core-harness-plan.md §2.3: "session
// JSONL ✅ 子集 — 追加写 + 头行 + 读取重建；分支树延后").
//
// # What is ported
//
//   - The file layout and the lazy-flush rule: nothing is written until the
//     session holds a user or assistant message, at which point the header and
//     every entry so far land in one create-write; after that each entry is a
//     single append (pi's _persist, session-manager.ts:1172-1189). Opening and
//     closing pi without chatting leaves no file behind, and a first turn that
//     never completes still has its prompt on disk (#10000).
//   - The parent/child tree: entries chain through parentId, the leaf pointer
//     tracks the tip, and context is rebuilt by walking parent links — never
//     by trusting file order (plan R7).
//   - Compaction-aware context projection (buildContextEntries), so a file pi
//     compacted rebuilds the same context here.
//
// # What is deliberately not ported (yet)
//
//   - branch()/resetLeaf()/createBranchedSession()/forkFrom(): the tree
//     structure exists and is walked, but no API moves the leaf. Deferred with
//     the rest of the branching UI (plan §2.3).
//   - Labels (type "label"), usage entries, context_edit: read and preserved
//     through Extra, never written by this version.
//   - appendCustomEntry/appendCustomMessageEntry: the extension hooks pi
//     exposes; not part of this phase's subset.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ultrac/internal/ai"
)

// maxSessionFileBytes caps how much of one session file is read into memory.
// Same ceiling as the old Store's MaxReadBytes: a multi-GB transcript read
// whole is an OOM, and anything that large needs a different strategy anyway.
const maxSessionFileBytes = 50 * 1024 * 1024

// ModelRef names the model a session last used (pi's SessionProjection.model).
type ModelRef struct {
	Provider string
	ModelID  string
}

// SessionContext is the rebuilt conversation: what the next request sends,
// plus the session state the transcript carries (pi's SessionContext,
// session-manager.ts:223-227).
type SessionContext struct {
	Messages      []ai.Message
	ThinkingLevel string
	Model         *ModelRef
}

// SessionManager owns one session file.
//
// It is safe for concurrent use: the agent loop appends from the loop
// goroutine while tools may read the context, and the mutex keeps the
// in-memory tree, the leaf pointer and the file consistent.
type SessionManager struct {
	mu sync.Mutex

	cwd         string
	sessionDir  string
	sessionFile string
	persist     bool

	// flushed reports whether the file exists on disk yet. The lazy rule:
	// setup entries stay in memory until the first user/assistant message.
	flushed bool

	header  *Header
	entries []*SessionEntry
	byID    map[string]*SessionEntry
	leafID  string // "" means "before any entry" (pi's null leaf)
}

// CreateSession starts a new session for cwd in the default directory
// (~/.ultrac/sessions/<encoded-cwd>), creating the directory.
//
// pi: SessionManager.create (session-manager.ts:1755-1758) — the mechanism is
// pi's, the root is Ultrac's own ~/.ultrac.
func CreateSession(cwd string) *SessionManager {
	resolved := resolveSessionPath(cwd)
	dir := SessionDirFor(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// A read-only home is not fatal here: the session still works in
		// memory and every append reports the same error. Degrading to
		// in-memory would silently lose the transcript instead.
		fmt.Fprintf(os.Stderr, "session: cannot create %s: %v\n", dir, err)
	}
	m := &SessionManager{
		cwd:        resolved,
		sessionDir: dir,
		persist:    true,
		byID:       make(map[string]*SessionEntry),
	}
	m.newSession("")
	return m
}

// OpenSession loads an existing session file.
//
// pi: SessionManager.open (session-manager.ts:1766-1786). A non-empty file
// whose first line is not a valid session header is refused rather than
// initialised — overwriting a file we do not understand is how a transcript
// gets destroyed.
func OpenSession(path string) (*SessionManager, error) {
	resolved := resolveSessionPath(path)
	header, entries, err := ReadSessionFile(resolved)
	if err != nil {
		return nil, err
	}
	if header == nil {
		if entries != nil {
			return nil, fmt.Errorf("session: %s is not a valid session file", resolved)
		}
		// Empty file: initialise it in place, preserving the explicit path.
		m := &SessionManager{
			cwd:         resolveSessionPath(""),
			sessionDir:  filepath.Dir(resolved),
			sessionFile: resolved,
			persist:     true,
			byID:        make(map[string]*SessionEntry),
		}
		m.newSession("")
		m.flushed = false
		return m, nil
	}
	// The header's cwd is kept verbatim — pi's getSessionHeaderCwd returns
	// the string as written, and only comparisons resolve it
	// (session-manager.ts:739-746). Resolving here would rewrite "/repo"
	// into a Windows drive path and change what a later Append records.
	cwd := header.CWD
	if cwd == "" {
		cwd = resolveSessionPath("")
	}
	m := &SessionManager{
		cwd:         cwd,
		sessionDir:  filepath.Dir(resolved),
		sessionFile: resolved,
		persist:     true,
		header:      header,
		byID:        make(map[string]*SessionEntry, len(entries)),
	}
	migrateEntries(header, entries)
	m.entries = entries
	m.reindex()
	m.flushed = true
	return m, nil
}

// ContinueRecentSession opens the most recently modified session for cwd, or
// creates a new one when there is none.
//
// pi: SessionManager.continueRecent (session-manager.ts:1793-1801). "Recent"
// is file mtime, and a header whose cwd does not match is skipped — a session
// file shared into this directory from another project must not be resumed as
// if it belonged here.
func ContinueRecentSession(cwd string) (*SessionManager, error) {
	resolved := resolveSessionPath(cwd)
	dir := SessionDirFor(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create %s: %w", dir, err)
	}
	path, err := findMostRecentSession(dir, resolved)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return CreateSession(resolved), nil
	}
	return OpenSession(path)
}

// InMemorySession builds a session with no file behind it. Tests and
// one-shot runs use it; nothing is ever written to disk.
//
// pi: SessionManager.inMemory (session-manager.ts:1804-1806).
func InMemorySession(cwd string) *SessionManager {
	m := &SessionManager{
		cwd:     resolveSessionPath(cwd),
		persist: false,
		byID:    make(map[string]*SessionEntry),
	}
	m.newSession("")
	return m
}

// newSession resets the manager to a fresh session. id "" generates one.
func (m *SessionManager) newSession(id string) {
	if id != "" && !ValidSessionID(id) {
		panic("session: invalid session id " + id)
	}
	m.header = NewHeader(id, m.cwd, "")
	m.entries = nil
	m.byID = make(map[string]*SessionEntry)
	m.leafID = ""
	m.flushed = false
	if m.persist {
		m.sessionFile = filepath.Join(m.sessionDir, SessionFileName(m.header.Timestamp, m.header.ID))
	}
}

// SessionID returns the session's uuidv7.
func (m *SessionManager) SessionID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.header.ID
}

// SessionFile returns the transcript path, or "" for an in-memory session
// that has not flushed yet.
func (m *SessionManager) SessionFile() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionFile
}

// CWD returns the session's working directory.
func (m *SessionManager) CWD() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cwd
}

// SessionDir returns the directory holding the session file.
func (m *SessionManager) SessionDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionDir
}

// Header returns a copy of the session header.
func (m *SessionManager) Header() Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.header
}

// Entries returns a shallow copy of the entries, in file order.
func (m *SessionManager) Entries() []SessionEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionEntry, len(m.entries))
	for i, e := range m.entries {
		out[i] = *e
	}
	return out
}

// GetLeafID returns the current leaf entry id, "" before any entry.
func (m *SessionManager) GetLeafID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leafID
}

// AppendMessage appends one conversation message and returns the entry id.
//
// The message is stored as pi's wire shape (ai.Message's JSON), so a file
// written here is the file pi would have written.
func (m *SessionManager) AppendMessage(msg ai.Message) (string, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("session: encode message: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.appendLocked(TypeMessage)
	e.Message = raw
	return e.ID, m.persistLocked(e)
}

// AppendModelChange records a provider/model switch (pi's
// appendModelChange, session-manager.ts:1230-1241). It does not enter LLM
// context; the projection reads it back as session state.
func (m *SessionManager) AppendModelChange(provider, modelID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.appendLocked(TypeModelChange)
	e.Provider = provider
	e.ModelID = modelID
	return e.ID, m.persistLocked(e)
}

// AppendThinkingLevelChange records a thinking-level switch (pi's
// appendThinkingLevelChange, session-manager.ts:1217-1227).
func (m *SessionManager) AppendThinkingLevelChange(level string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.appendLocked(TypeThinkingLevelChange)
	e.ThinkingLevel = level
	return e.ID, m.persistLocked(e)
}

// AppendSessionInfo records the session's display name (pi's
// appendSessionInfo, session-manager.ts:1304-1315). The name is sanitised the
// same way: newlines would break the one-line-per-entry file.
func (m *SessionManager) AppendSessionInfo(name string) (string, error) {
	sanitized := strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(name))
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.appendLocked(TypeSessionInfo)
	e.Name = sanitized
	return e.ID, m.persistLocked(e)
}

// SessionName returns the latest session_info name, "" when unset. An empty
// name explicitly clears the title (pi's getSessionName,
// session-manager.ts:1318-1329).
func (m *SessionManager) SessionName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].Type == TypeSessionInfo {
			return strings.TrimSpace(m.entries[i].Name)
		}
	}
	return ""
}

// appendLocked builds the entry as a child of the current leaf and advances
// the leaf. Caller holds the mutex and fills the type-specific fields.
func (m *SessionManager) appendLocked(typ string) *SessionEntry {
	e := &SessionEntry{
		Type:      typ,
		ID:        newEntryID(m.idSetLocked()),
		Timestamp: NowISO(),
	}
	if m.leafID != "" {
		parent := m.leafID
		e.ParentID = &parent
	}
	m.entries = append(m.entries, e)
	m.byID[e.ID] = e
	m.leafID = e.ID
	return e
}

func (m *SessionManager) idSetLocked() map[string]bool {
	taken := make(map[string]bool, len(m.byID))
	for id := range m.byID {
		taken[id] = true
	}
	return taken
}

// persistLocked applies pi's lazy rule: the file is created (exclusively, with
// the header and every entry so far) the first time the session holds a user
// or assistant message; after that each entry is one append.
//
// Starting at the user message rather than the first assistant reply is
// deliberate (pi #10000): a turn that crashes mid-stream still leaves the
// prompt on disk.
func (m *SessionManager) persistLocked(entry *SessionEntry) error {
	if !m.persist || m.sessionFile == "" {
		return nil
	}
	if m.flushed {
		return appendLine(m.sessionFile, entry)
	}
	if !m.hasConversationLocked() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.sessionFile), 0o755); err != nil {
		return fmt.Errorf("session: create directory for %s: %w", m.sessionFile, err)
	}
	f, err := os.OpenFile(m.sessionFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("session: create %s: %w", m.sessionFile, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	if _, err := w.Write(mustMarshalHeader(m.header)); err != nil {
		return fmt.Errorf("session: write header %s: %w", m.sessionFile, err)
	}
	for _, e := range m.entries {
		if _, err := w.Write(mustMarshalEntry(e)); err != nil {
			return fmt.Errorf("session: write entry %s: %w", e.ID, err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("session: flush %s: %w", m.sessionFile, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("session: sync %s: %w", m.sessionFile, err)
	}
	m.flushed = true
	return nil
}

func (m *SessionManager) hasConversationLocked() bool {
	for _, e := range m.entries {
		if e.Type != TypeMessage {
			continue
		}
		switch EntryMessageRole(e) {
		case string(ai.RoleUser), string(ai.RoleAssistant):
			return true
		}
	}
	return false
}

// GetBranch walks from an entry to the root, returning the path in order.
// Empty fromID means the current leaf (pi's getBranch,
// session-manager.ts:1469-1479).
func (m *SessionManager) GetBranch(fromID string) []*SessionEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	start := fromID
	if start == "" {
		start = m.leafID
	}
	return buildPath(m.entries, start, m.byID)
}

// BuildSessionContext rebuilds the conversation from the current leaf,
// compaction-aware (pi's buildSessionContext, session-manager.ts:1497-1500).
func (m *SessionManager) BuildSessionContext() SessionContext {
	m.mu.Lock()
	defer m.mu.Unlock()
	return buildContext(m.entries, m.leafID, m.byID)
}

// reindex rebuilds the id index and the leaf pointer after a load. The leaf is
// the last entry in file order — pi's _buildIndex (session-manager.ts:
// 1103-1122) does the same, and the file's last line is the tip because
// entries are only ever appended.
func (m *SessionManager) reindex() {
	m.byID = make(map[string]*SessionEntry, len(m.entries))
	m.leafID = ""
	for _, e := range m.entries {
		if e.Type == "session" || e.ID == "" {
			continue
		}
		m.byID[e.ID] = e
		m.leafID = e.ID
	}
}

// ReadSessionFile loads a session file, tolerating blank and malformed lines
// the same way pi's loadEntriesFromFile does (session-manager.ts:627-670): a
// crash mid-append leaves a truncated last line, and refusing the file over
// one bad byte would lose the session.
//
// A file whose first parsed line is not a valid header returns an error —
// that file is not a session and must not be appended to.
func ReadSessionFile(path string) (*Header, []*SessionEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && info.Size() > maxSessionFileBytes {
		return nil, nil, fmt.Errorf(
			"session: %s is %d bytes, over the %d byte read limit; refusing to load it into memory",
			path, info.Size(), maxSessionFileBytes)
	}

	var (
		header  *Header
		entries []*SessionEntry
	)
	scanner := bufio.NewScanner(f)
	// Long lines are normal: a message with a large tool result is one line
	// of a few megabytes. The default 64KB limit would truncate it into a
	// parse error and silently drop the entry.
	scanner.Buffer(make([]byte, 0, 256*1024), int(maxSessionFileBytes))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		parsed, perr := ParseLine(line)
		if perr != nil {
			return nil, nil, perr
		}
		switch v := parsed.(type) {
		case nil:
			continue // malformed line: skipped
		case *Header:
			if header == nil {
				header = v
			}
		case *SessionEntry:
			entries = append(entries, v)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return nil, nil, fmt.Errorf("session: read %s: %w", path, err)
	}
	if header == nil && len(entries) > 0 {
		return nil, entries, nil
	}
	return header, entries, nil
}

// findMostRecentSession returns the newest .jsonl in dir whose header matches
// cwd, or "" when there is none (pi's findMostRecentSession,
// session-manager.ts:749-768). Discovery is best-effort: an unreadable file is
// skipped rather than failing the listing.
func findMostRecentSession(dir, cwd string) (string, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("session: list %s: %w", dir, err)
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
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mtime.After(candidates[j].mtime) })
	for _, c := range candidates {
		header, _, err := ReadSessionFile(c.path)
		if err != nil || header == nil {
			continue
		}
		if header.CWD != "" && resolveSessionPath(header.CWD) == cwd {
			return c.path, nil
		}
	}
	return "", nil
}

// migrateEntries brings entries from an older session version up to the
// current shape, in memory only.
//
// pi rewrites the file when a migration applies (session-manager.ts:1092-1094).
// This version does not: rewriting an append-only transcript to fix metadata
// is exactly the operation the format exists to avoid, and the in-memory
// result is identical for every reader in this process. The v2→v3 rename
// (hookMessage → custom) is not applied because no writer here produces
// hookMessage; a file containing one simply drops those entries on decode,
// which is the same outcome pi's Unmarshal tolerance gives an unknown role.
func migrateEntries(header *Header, entries []*SessionEntry) {
	if header == nil || header.Version >= 2 {
		return
	}
	var prev string
	for _, e := range entries {
		if e.ID == "" {
			e.ID = newEntryID(nil)
		}
		if e.ParentID == nil && prev != "" {
			parent := prev
			e.ParentID = &parent
		}
		prev = e.ID
	}
	header.Version = CurrentSessionVersion
}

// mustMarshalHeader / mustMarshalEntry panic on an encode failure. Both
// encoders only fail on types json cannot represent, and every field here is
// a string, int or json.RawMessage — so a failure is a programming error, and
// the caller has no better option than failing the process rather than
// writing a half-line into an append-only file.
func mustMarshalHeader(h *Header) []byte {
	data, err := MarshalHeader(h)
	if err != nil {
		panic(err)
	}
	return data
}

func mustMarshalEntry(e *SessionEntry) []byte {
	data, err := MarshalEntry(e)
	if err != nil {
		panic(err)
	}
	return data
}

// appendLine writes one already-encoded line to the end of the file.
func appendLine(path string, e *SessionEntry) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("session: open %s for append: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(mustMarshalEntry(e)); err != nil {
		return fmt.Errorf("session: append to %s: %w", path, err)
	}
	return f.Sync()
}
