package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ultrac/internal/config"
)

// MaxReadBytes caps how much of a transcript is read into memory.
//
// Session files grow to multiple GB in long-running sessions. The tombstone
// path reads and rewrites the whole file, so an uncapped read is an OOM. 50MB
// is the same ceiling the TypeScript side uses, chosen because it covers any
// realistic resume and anything larger needs a different strategy anyway.
const MaxReadBytes = 50 * 1024 * 1024

// Store reads and writes transcripts for one configuration directory.
//
// It is safe for concurrent use. Tools run in parallel and several may append
// to the same transcript within a turn.
type Store struct {
	// Home is the configuration directory, normally ~/.ultrac. Tests point
	// it at a temp dir rather than mutating the environment, so parallel
	// tests cannot interfere.
	Home string

	mu sync.Mutex
	// handles caches open files per path. A session is written many times per
	// turn; reopening each time is a measurable cost on Windows.
	handles map[string]*handle
}

type handle struct {
	f    *os.File
	w    *bufio.Writer
	path string
	// dirty tracks unflushed bytes, so Close and Flush know whether a Sync is
	// worth the syscall.
	dirty bool
}

// NewStore returns a Store rooted at the default configuration directory.
func NewStore() *Store {
	return &Store{Home: config.ConfigHome(), handles: make(map[string]*handle)}
}

// NewStoreAt returns a Store rooted at an explicit directory.
func NewStoreAt(home string) *Store {
	return &Store{Home: home, handles: make(map[string]*handle)}
}

// ProjectsDir is where every project's transcripts live.
func (s *Store) ProjectsDir() string { return filepath.Join(s.Home, "projects") }

// ProjectDir returns the transcript directory for a working directory.
//
// The sanitisation is what keeps two repositories from colliding and what makes
// the name filesystem-safe. It must stay byte-identical to the TypeScript
// implementation's, or sessions become mutually invisible.
func (s *Store) ProjectDir(cwd string) string {
	return filepath.Join(s.ProjectsDir(), SanitizePath(cwd))
}

// TranscriptPath returns the transcript path for a session in a project.
func (s *Store) TranscriptPath(cwd, sessionID string) string {
	return filepath.Join(s.ProjectDir(cwd), sessionID+".jsonl")
}

// HistoryPath is the global prompt history, shared across every project.
func (s *Store) HistoryPath() string { return filepath.Join(s.Home, "history.jsonl") }

// Append writes entries to a transcript, creating parent directories.
//
// The file stays open between calls; call Flush or Close to make the bytes
// durable. That is the intended pattern: a turn appends a dozen times and
// flushes once, and the session is still readable after a crash because each
// Append is a separate write.
func (s *Store) Append(path string, entries ...*Entry) error {
	if len(entries) == 0 {
		return nil
	}
	h, err := s.openForWrite(path)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range entries {
		if e == nil {
			continue
		}
		data, err := marshalEntry(e)
		if err != nil {
			return fmt.Errorf("session: encode entry %s: %w", e.UUID, err)
		}
		if _, err := h.w.Write(data); err != nil {
			return fmt.Errorf("session: write %s: %w", path, err)
		}
		if err := h.w.WriteByte('\n'); err != nil {
			return fmt.Errorf("session: write %s: %w", path, err)
		}
		h.dirty = true
	}
	return nil
}

// marshalEntry encodes one entry.
//
// The two-pass shape is deliberate. Fields this version models are written
// first so they always appear with their canonical names, then Extra fills in
// everything else. Marshalling a struct with an inline Extra map would make the
// two sets collide, and the loser depends on map iteration order — which means
// a session could round-trip differently on two runs.
func marshalEntry(e *Entry) ([]byte, error) {
	type alias Entry
	data, err := json.Marshal((*alias)(e))
	if err != nil {
		return nil, err
	}
	if len(e.Extra) == 0 {
		return data, nil
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for k, v := range e.Extra {
		if _, taken := merged[k]; taken {
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		merged[k] = raw
	}
	return json.Marshal(merged)
}

// openForWrite returns the handle for path, opening it on first use.
//
// The whole check-open-register sequence runs under the mutex, and that is
// load-bearing on Windows: two goroutines racing to create the same file with
// O_CREATE intermittently gets "Access is denied" from CreateFile (one
// creator wins, the rest are refused), and the previous double-checked
// version performed the open outside the lock, so every loser silently
// dropped its append — the caller ignores the error and a transcript line
// vanishes. The critical section covers only the first open per path; steady
// state is a map lookup.
func (s *Store) openForWrite(path string) (*handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if h, ok := s.handles[path]; ok {
		return h, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("session: create directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}

	h := &handle{f: f, w: bufio.NewWriterSize(f, 64*1024), path: path}
	s.handles[path] = h
	return h, nil
}

// Flush pushes buffered bytes to the file. Durability is per-flush, so this is
// what a caller invokes at the end of a turn.
func (s *Store) Flush(path string) error {
	s.mu.Lock()
	h, ok := s.handles[path]
	s.mu.Unlock()
	if !ok {
		return nil
	}

	if err := h.w.Flush(); err != nil {
		return fmt.Errorf("session: flush %s: %w", path, err)
	}
	// Sync as well as Flush. Flush only reaches the OS; a power loss between
	// the two loses the turn, and the point of appending per turn is that the
	// turn survives.
	if err := h.f.Sync(); err != nil {
		return fmt.Errorf("session: sync %s: %w", path, err)
	}

	s.mu.Lock()
	h.dirty = false
	s.mu.Unlock()
	return nil
}

// FlushAll flushes every open transcript.
func (s *Store) FlushAll() error {
	s.mu.Lock()
	paths := make([]string, 0, len(s.handles))
	for p := range s.handles {
		paths = append(paths, p)
	}
	s.mu.Unlock()

	var firstErr error
	for _, p := range paths {
		if err := s.Flush(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close flushes and closes every open transcript. Safe to call twice.
func (s *Store) Close() error {
	s.mu.Lock()
	handles := s.handles
	s.handles = make(map[string]*handle)
	s.mu.Unlock()

	var firstErr error
	for _, h := range handles {
		if h.dirty {
			if err := h.w.Flush(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if err := h.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Read loads every entry in a transcript.
//
// Two tolerances, both load-bearing:
//
//   - A truncated final line is skipped. A crash mid-append leaves a partial
//     JSON object; refusing to read the file would lose an entire session over
//     one bad byte.
//   - A malformed line in the middle is skipped rather than fatal, because the
//     same writer may have been interrupted by something worse than a crash.
//
// Entries are returned in file order. The caller rebuilds the chain.
func (s *Store) Read(path string) ([]*Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && info.Size() > MaxReadBytes {
		return nil, fmt.Errorf(
			"session: %s is %d bytes, over the %d byte read limit; refusing to load it into memory",
			path, info.Size(), MaxReadBytes)
	}

	var out []*Entry
	scanner := bufio.NewScanner(f)
	// Long lines are normal here: a message with a large tool result is one
	// line of a few megabytes. The default 64KB limit would truncate it into
	// a parse error and silently drop the entry.
	scanner.Buffer(make([]byte, 0, 256*1024), int(MaxReadBytes))

	for scanner.Scan() {
		line := bytesTrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		e, err := unmarshalEntry(line)
		if err != nil {
			continue
		}
		out = append(out, e)
	}
	if err := scanner.Err(); err != nil {
		// A token-too-long error is the size limit doing its job, not a
		// corrupt file. Everything read so far is still valid.
		if !errors.Is(err, bufio.ErrTooLong) {
			return out, fmt.Errorf("session: read %s: %w", path, err)
		}
	}
	return out, nil
}

func bytesTrimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpaceByte(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// unmarshalEntry decodes one line, splitting known fields from Extra.
func unmarshalEntry(line []byte) (*Entry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}

	var e Entry
	// A missing or malformed type is not fatal: the entry is still recorded,
	// it just does not participate in the chain.
	_ = json.Unmarshal(raw["type"], &e.Type)
	_ = json.Unmarshal(raw["uuid"], &e.UUID)
	_ = json.Unmarshal(raw["parentUuid"], &e.ParentUUID)
	_ = json.Unmarshal(raw["timestamp"], &e.Timestamp)
	_ = json.Unmarshal(raw["message"], &e.Message)
	_ = json.Unmarshal(raw["isSidechain"], &e.IsSidechain)
	_ = json.Unmarshal(raw["sessionId"], &e.SessionID)
	_ = json.Unmarshal(raw["cwd"], &e.CWD)
	_ = json.Unmarshal(raw["version"], &e.Version)
	_ = json.Unmarshal(raw["userType"], &e.UserType)
	_ = json.Unmarshal(raw["entrypoint"], &e.Entrypoint)
	_ = json.Unmarshal(raw["permissionMode"], &e.PermissionMode)
	_ = json.Unmarshal(raw["gitBranch"], &e.GitBranch)
	_ = json.Unmarshal(raw["promptId"], &e.PromptID)
	_ = json.Unmarshal(raw["lastPrompt"], &e.LastPrompt)
	_ = json.Unmarshal(raw["operation"], &e.Operation)
	_ = json.Unmarshal(raw["content"], &e.Content)

	known := map[string]bool{
		"type": true, "uuid": true, "parentUuid": true, "timestamp": true,
		"message": true, "isSidechain": true, "sessionId": true, "cwd": true,
		"version": true, "userType": true, "entrypoint": true,
		"permissionMode": true, "gitBranch": true, "promptId": true,
		"lastPrompt": true, "operation": true, "content": true,
	}
	for k, v := range raw {
		if known[k] {
			continue
		}
		var anyVal any
		if err := json.Unmarshal(v, &anyVal); err != nil {
			continue
		}
		if e.Extra == nil {
			e.Extra = make(map[string]any)
		}
		e.Extra[k] = anyVal
	}
	return &e, nil
}

// NowISO returns the current time in the format transcripts use.
//
// UTC with millisecond precision, matching the existing files. A local-time
// timestamp would sort wrongly against the ones already on disk.
func NowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// SessionInfo is the summary shown in a session list.
type SessionInfo struct {
	SessionID    string
	Path         string
	CWD          string
	FirstPrompt  string
	Summary      string
	CustomTitle  string
	MessageCount int
	IsSidechain  bool
	Created      time.Time
	Modified     time.Time
}

// Title returns the best available human label for the session.
//
// The fallback chain is not cosmetic: a session with no prompt at all still
// has to be identifiable in a list, and its id prefix is the only thing that
// distinguishes it.
func (s SessionInfo) Title() string {
	switch {
	case s.CustomTitle != "":
		return s.CustomTitle
	case s.Summary != "":
		return s.Summary
	case s.FirstPrompt != "":
		return truncateRunes(s.FirstPrompt, 50)
	case s.SessionID != "":
		if len(s.SessionID) >= 8 {
			return s.SessionID[:8]
		}
		return s.SessionID
	default:
		return "Untitled session"
	}
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// List returns the sessions in a project, newest first.
func (s *Store) List(cwd string) ([]SessionInfo, error) {
	dir := s.ProjectDir(cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: list %s: %w", dir, err)
	}

	var out []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if !ValidUUID(id) {
			continue
		}
		info := s.readInfo(filepath.Join(dir, e.Name()), id)
		out = append(out, info)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// readInfo reads only what a session list needs.
//
// It scans the file rather than decoding every entry: a session list over a
// project with 500 sessions must not pull 500 full conversations into memory,
// and the fields a list shows all live at predictable places.
func (s *Store) readInfo(path, id string) SessionInfo {
	info := SessionInfo{SessionID: id, Path: path}

	f, err := os.Open(path)
	if err != nil {
		return info
	}
	defer f.Close()

	if st, err := f.Stat(); err == nil {
		info.Modified = st.ModTime()
		info.Created = st.ModTime()
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := bytesTrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		info.MessageCount++

		// Cheap substring probes rather than a full unmarshal: this runs over
		// every line of every session in a list.
		switch {
		case bytesContains(line, `"type":"last-prompt"`):
			if e, err := unmarshalEntry(line); err == nil {
				info.FirstPrompt = firstLine(e.LastPrompt)
			}
		case bytesContains(line, `"type":"summary"`):
			if e, err := unmarshalEntry(line); err == nil && info.Summary == "" {
				info.Summary = e.ExtraString("summary")
			}
		case bytesContains(line, `"type":"custom-title"`):
			if e, err := unmarshalEntry(line); err == nil {
				info.CustomTitle = e.ExtraString("customTitle")
			}
		case bytesContains(line, `"isSidechain":true`):
			info.IsSidechain = true
		case info.CWD == "" || info.FirstPrompt == "" || info.Created.IsZero():
			e, err := unmarshalEntry(line)
			if err != nil {
				continue
			}
			// Track the earliest timestamp: the session's creation time is the
			// first entry, and later entries can carry earlier stamps when a
			// turn was retried.
			if ts := parseISO(e.Timestamp); !ts.IsZero() && (info.Created.IsZero() || ts.Before(info.Created)) {
				info.Created = ts
			}
			if info.CWD == "" {
				info.CWD = e.CWD
			}
			if info.FirstPrompt == "" && e.Type == EntryUser {
				if txt := e.FirstUserText(); txt != "" {
					info.FirstPrompt = firstLine(txt)
				}
			}
		}
	}

	if info.Created.IsZero() {
		if st, err := f.Stat(); err == nil {
			info.Created = st.ModTime()
		}
	}
	return info
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func bytesContains(b []byte, sub string) bool {
	return len(b) >= len(sub) && indexBytes(b, sub) >= 0
}

func indexBytes(b []byte, sub string) int {
	return strings.Index(string(b), sub)
}

// ExtraString reads a string field out of Extra, returning "" when absent.
func (e *Entry) ExtraString(key string) string {
	if e.Extra == nil {
		return ""
	}
	s, _ := e.Extra[key].(string)
	return s
}

// FirstUserText returns the first text block of a user entry's message.
func (e *Entry) FirstUserText() string {
	switch m := e.Message.(type) {
	case string:
		return m
	case userMessage:
		return contentText(m.Content)
	case map[string]any:
		if s, ok := m["content"].(string); ok {
			return s
		}
		return contentText(m["content"])
	}
	return ""
}

func contentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, block := range c {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "text" {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// Chain is the conversation reconstructed from a transcript.
type Chain struct {
	// Entries are the transcript messages, in conversation order.
	Entries []*Entry
	// ByUUID indexes Entries for parent lookup.
	ByUUID map[string]*Entry
}

// BuildChain reconstructs the conversation from raw entries.
//
// Rules, in order:
//
//  1. Drop non-transcript entries. Progress is the case that matters; see
//     EntryProgress for what including it costs.
//  2. Drop sidechains. A subagent's turns are a separate conversation and
//     interleaving them into the main chain is what produces duplicated
//     messages on resume.
//  3. Follow ParentUUID from the newest leaf backwards, then reverse. The
//     alternative — trusting file order — breaks after a rewind, where the file
//     contains messages that were abandoned and then re-asked.
//
// An empty input yields an empty chain, not nil, so callers need no nil check.
func BuildChain(entries []*Entry) *Chain {
	byUUID := make(map[string]*Entry, len(entries))
	var candidates []*Entry

	for _, e := range entries {
		if e == nil || !IsTranscript(e.Type) || e.IsSidechain {
			continue
		}
		if e.UUID != "" {
			byUUID[e.UUID] = e
		}
		candidates = append(candidates, e)
	}

	if len(candidates) == 0 {
		return &Chain{ByUUID: byUUID}
	}

	// Find the leaf: the entry nothing else points at. Prefer the last such
	// entry in file order, since a compacted transcript may leave several
	// dangling tips.
	hasChild := make(map[string]bool, len(candidates))
	for _, e := range candidates {
		if e.ParentUUID != "" {
			hasChild[e.ParentUUID] = true
		}
	}
	leaf := candidates[len(candidates)-1]
	for i := len(candidates) - 1; i >= 0; i-- {
		if candidates[i].UUID != "" && !hasChild[candidates[i].UUID] {
			leaf = candidates[i]
			break
		}
	}

	// Walk up, guarding against a cycle. A cycle in the parent links means
	// the file was corrupted; stopping beats looping forever.
	chain := make([]*Entry, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for e := leaf; e != nil; {
		if e.UUID != "" {
			if seen[e.UUID] {
				break
			}
			seen[e.UUID] = true
		}
		chain = append(chain, e)
		if e.ParentUUID == "" {
			break
		}
		parent, ok := byUUID[e.ParentUUID]
		if !ok {
			// Parent was compacted away or lives in a dropped sidechain. The
			// chain starts here; that is correct, not an error.
			break
		}
		e = parent
	}

	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	out := &Chain{Entries: chain, ByUUID: make(map[string]*Entry, len(chain))}
	for _, e := range chain {
		out.ByUUID[e.UUID] = e
	}
	return out
}

// RemoveByUUID rewrites a transcript without the entry, leaving a tombstone in
// its place.
//
// The tombstone is the whole point: an append-only file cannot have a line
// deleted, and simply omitting the entry would break every parentUuid pointing
// at it, orphaning the rest of the conversation. Readers skip tombstones and
// bridge the gap.
//
// The rewrite is capped at MaxReadBytes. Past that, the file is too large to
// hold in memory and the caller has to decide what to lose.
func (s *Store) RemoveByUUID(path, uuid string) error {
	entries, err := s.Read(path)
	if err != nil {
		return err
	}

	found := false
	kept := make([]*Entry, 0, len(entries))
	for _, e := range entries {
		if e.UUID == uuid {
			found = true
			tomb := &Entry{
				Type:        e.Type,
				UUID:        e.UUID,
				ParentUUID:  e.ParentUUID,
				Timestamp:   e.Timestamp,
				SessionID:   e.SessionID,
				IsSidechain: e.IsSidechain,
				Extra:       map[string]any{"isTombstone": true},
			}
			kept = append(kept, tomb)
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return nil
	}

	tmp := path + ".rewrite"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("session: rewrite %s: %w", path, err)
	}
	w := bufio.NewWriterSize(f, 64*1024)
	for _, e := range kept {
		data, err := marshalEntry(e)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("session: encode entry %s: %w", e.UUID, err)
		}
		w.Write(data)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("session: rewrite %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("session: rewrite %s: %w", path, err)
	}

	// The open handle points at the old inode. Close it before replacing, or
	// the next Append lands in a file nothing reads.
	s.mu.Lock()
	if h, ok := s.handles[path]; ok {
		_ = h.f.Close()
		delete(s.handles, path)
	}
	s.mu.Unlock()

	if err := os.Rename(tmp, path); err != nil {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			os.Remove(tmp)
			return fmt.Errorf("session: replace %s: %w", path, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("session: replace %s: %w", path, err)
		}
	}
	return nil
}

// IsTombstone reports whether an entry is a removal marker.
func (e *Entry) IsTombstone() bool {
	v, ok := e.Extra["isTombstone"]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// ---------------------------------------------------------------------------
// Prompt history
// ---------------------------------------------------------------------------

// MaxHistoryItems bounds how much history the picker will read.
//
// 100 is what the TypeScript side keeps, and the reason is context: this feeds
// the up-arrow picker, and a user scrolling past 100 prompts is looking for
// something they can name, not something they can remember.
const MaxHistoryItems = 100

// HistoryEntry is one prompt from history.jsonl.
type HistoryEntry struct {
	Display   string `json:"display"`
	Project   string `json:"project"`
	Timestamp int64  `json:"timestamp"`
	SessionID string `json:"sessionId,omitempty"`
}

// AppendHistory adds a prompt to the global history.
func (s *Store) AppendHistory(entry HistoryEntry) error {
	path := s.HistoryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("session: create history directory: %w", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session: encode history entry: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("session: write %s: %w", path, err)
	}
	return nil
}

// History returns the most recent prompts, newest first.
//
// A malformed line is skipped rather than fatal: history is a convenience, and
// one bad line must not stop the up-arrow from working. The scan stops at
// MaxHistoryItems, so the cost is bounded even in a file with years of prompts.
func (s *Store) History(cwd string) ([]HistoryEntry, error) {
	f, err := os.Open(s.HistoryPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: open history: %w", err)
	}
	defer f.Close()

	scanned, err := readAllLines(f, 4*1024*1024)
	if err != nil {
		return nil, err
	}

	out := make([]HistoryEntry, 0, MaxHistoryItems)
	// Newest first, so the most recent prompts are the ones that survive the
	// cap. The picker wants recency.
	for i := len(scanned) - 1; i >= 0 && len(out) < MaxHistoryItems; i-- {
		var e HistoryEntry
		if err := json.Unmarshal(scanned[i], &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// readAllLines reads a file's lines, capped at maxLine bytes each.
func readAllLines(r io.Reader, maxLine int) ([][]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	var out [][]byte
	for scanner.Scan() {
		line := bytesTrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		out = append(out, line)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return out, err
	}
	return out, nil
}
