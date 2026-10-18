// pi_format.go — the pi-shaped session file: one header line, then one entry
// per line, forming an append-only tree (docs/pi-core-harness-plan.md §3.5 P4,
// §6.1).
//
// # The two vocabularies in this package
//
// This file adds pi's format alongside the Claude Code shaped Entry/Store API
// in entry.go and store.go. The old API is deprecated: its last writer (the
// pre-port engine) was deleted in P7, so it now only reads legacy transcripts
// under ~/.ultrac/projects. The new API is what internal/agent's callers and
// the CLI use.
//
// # What "aligned with pi" means concretely
//
// Every line is JSON with the same keys pi's session-manager.ts writes:
//
//	{"type":"session","version":3,"id":"<uuidv7>","timestamp":"...","cwd":"..."}
//	{"type":"message","id":"a1b2c3d4","parentId":null,"timestamp":"...","message":{...}}
//
// The header is exactly pi's SessionHeader (session-manager.ts:43-50); entries
// carry SessionEntryBase's id/parentId/timestamp (ibid.:57-62) plus the
// type-specific fields. parentId is always present — null for a root entry —
// because pi's buildSessionPath walks it (ibid.:408-416).
//
// # Unknown fields survive
//
// Same rule as config.Settings and the old Entry: fields this version does not
// model are preserved verbatim in Extra and written back. A session file is
// shared with pi, and dropping a field pi understands would silently change
// the conversation on resume.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"ultrac/internal/ai"
	"ultrac/internal/config"
)

// CurrentSessionVersion is pi's CURRENT_SESSION_VERSION
// (session-manager.ts:41). Written into every header this package creates;
// files with a lower version are still read (see MigrateEntries).
const CurrentSessionVersion = 3

// Entry type discriminators — pi's SessionEntry union (session-manager.ts:
// 183-194). Only the four this version writes are modelled as typed fields;
// the rest round-trip through Extra and are interpreted by the context
// projection in pi_context.go.
const (
	TypeMessage             = "message"
	TypeThinkingLevelChange = "thinking_level_change"
	TypeModelChange         = "model_change"
	TypeUsage               = "usage"
	TypeCompaction          = "compaction"
	TypeBranchSummary       = "branch_summary"
	TypeCustom              = "custom"
	TypeCustomMessage       = "custom_message"
	TypeContextEdit         = "context_edit"
	TypeLabel               = "label"
	TypeSessionInfo         = "session_info"
)

// Header is the first line of a session file.
//
// pi: packages/coding-agent/src/core/session-manager.ts:43-50. version is
// absent in v1 files, hence omitempty on write but still decoded.
type Header struct {
	Type          string `json:"type"`
	Version       int    `json:"version,omitempty"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

// SessionEntry is one non-header line: a message or a piece of session state.
//
// The typed fields cover the entry types this version appends (message,
// model_change, thinking_level_change, session_info). Everything else the file
// carries lands in Extra verbatim — including the bodies of compaction,
// branch_summary, custom, label and context_edit entries, which the reader
// interprets but never writes.
//
// pi: packages/ai/src/core/session-manager.ts:57-194 (SessionEntryBase plus
// the union members).
type SessionEntry struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`

	// Message is the raw provider message of a TypeMessage entry, kept raw on
	// purpose: the transcript records what was sent and received, and
	// re-serialising through a Go struct would drop fields the model sent.
	// Decode it with EntryMessage.
	Message json.RawMessage `json:"message,omitempty"`

	// ThinkingLevel is set on TypeThinkingLevelChange entries.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	// Provider and ModelID are set on TypeModelChange entries.
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`
	// Name is set on TypeSessionInfo entries (the display title).
	Name string `json:"name,omitempty"`

	// Extra preserves every field this version does not model, same reasoning
	// as the old Entry: the file is shared with pi.
	Extra map[string]json.RawMessage `json:"-"`
}

// ExtraString reads a string field out of Extra, "" when absent or not a
// string.
func (e *SessionEntry) ExtraString(key string) string {
	raw, ok := e.Extra[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// EntryMessage decodes the entry's message as a pi-shaped ai.Message. It
// reports false for entries that carry no message.
//
// The decode is total: a line written by an older pi version (or by hand)
// that carries a message shape this build predates fails the whole entry
// rather than half-decoding it — the caller drops the entry, matching pi's
// loadEntriesFromFile tolerance for malformed lines (session-manager.ts:
// 616-624).
func EntryMessage(e *SessionEntry) (ai.Message, bool) {
	if e == nil || len(e.Message) == 0 {
		return ai.Message{}, false
	}
	var m ai.Message
	if err := json.Unmarshal(e.Message, &m); err != nil {
		return ai.Message{}, false
	}
	return m, true
}

// EntryMessageRole reports the role of an entry's message without a full
// decode, for the cheap checks (hasConversation, session-info scanning).
func EntryMessageRole(e *SessionEntry) string {
	if e == nil || len(e.Message) == 0 {
		return ""
	}
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(e.Message, &probe); err != nil {
		return ""
	}
	return probe.Role
}

// knownEntryKeys is the set SessionEntry models directly. Everything else in
// the line goes to Extra.
var knownEntryKeys = map[string]bool{
	"type": true, "id": true, "parentId": true, "timestamp": true,
	"message": true, "thinkingLevel": true, "provider": true,
	"modelId": true, "name": true,
}

// ParseLine decodes one JSONL line into a header or a session entry. It
// returns (nil, nil) for a blank or malformed line — the same tolerance pi's
// parseSessionEntryLine applies (session-manager.ts:616-624): one corrupt line
// must not cost the whole session.
func ParseLine(line []byte) (any, error) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil, nil
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, nil // malformed line: skipped, not fatal
	}
	if probe.Type == "session" {
		var h Header
		if err := json.Unmarshal([]byte(trimmed), &h); err != nil {
			return nil, nil
		}
		if h.ID == "" {
			return nil, nil // a header without an id is not a session
		}
		return &h, nil
	}
	return parseSessionEntry([]byte(trimmed))
}

func parseSessionEntry(line []byte) (*SessionEntry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, nil
	}
	e := &SessionEntry{}
	_ = json.Unmarshal(raw["type"], &e.Type)
	_ = json.Unmarshal(raw["id"], &e.ID)
	_ = json.Unmarshal(raw["timestamp"], &e.Timestamp)
	_ = json.Unmarshal(raw["message"], &e.Message)
	_ = json.Unmarshal(raw["thinkingLevel"], &e.ThinkingLevel)
	_ = json.Unmarshal(raw["provider"], &e.Provider)
	_ = json.Unmarshal(raw["modelId"], &e.ModelID)
	_ = json.Unmarshal(raw["name"], &e.Name)
	// parentId is modelled as *string so "absent" and "null" both decode to
	// nil, and a missing key on an old file does not panic.
	if pid, ok := raw["parentId"]; ok && string(pid) != "null" {
		var s string
		if json.Unmarshal(pid, &s) == nil {
			e.ParentID = &s
		}
	}
	for k, v := range raw {
		if knownEntryKeys[k] {
			continue
		}
		var anyVal any
		if err := json.Unmarshal(v, &anyVal); err != nil {
			continue
		}
		if e.Extra == nil {
			e.Extra = make(map[string]json.RawMessage, len(raw))
		}
		e.Extra[k] = v
	}
	return e, nil
}

// MarshalHeader encodes the header line (with its trailing newline).
func MarshalHeader(h *Header) ([]byte, error) {
	h.Type = "session"
	data, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("session: encode header %s: %w", h.ID, err)
	}
	return append(data, '\n'), nil
}

// MarshalEntry encodes one entry line (with its trailing newline).
//
// Two-pass, like the old marshalEntry: the modelled fields are written first
// under their canonical names, then Extra fills in everything else. A struct
// with an inline Extra map would make the two sets collide, and the loser
// would depend on map iteration order — a file that round-trips differently
// on two runs.
func MarshalEntry(e *SessionEntry) ([]byte, error) {
	type alias SessionEntry
	data, err := json.Marshal((*alias)(e))
	if err != nil {
		return nil, fmt.Errorf("session: encode entry %s: %w", e.ID, err)
	}
	if len(e.Extra) == 0 {
		return append(data, '\n'), nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for k, v := range e.Extra {
		if _, taken := merged[k]; taken {
			continue
		}
		merged[k] = v
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("session: encode entry %s extra: %w", e.ID, err)
	}
	return append(out, '\n'), nil
}

// NewHeader builds the header for a new session. id defaults to a fresh
// uuidv7; parentSession records the file this session was forked from.
func NewHeader(id, cwd, parentSession string) *Header {
	if id == "" {
		id = NewSessionID()
	}
	return &Header{
		Type:          "session",
		Version:       CurrentSessionVersion,
		ID:            id,
		Timestamp:     NowISO(),
		CWD:           cwd,
		ParentSession: parentSession,
	}
}

// newEntryID returns 8 hex characters not present in taken.
//
// pi's generateId (session-manager.ts:277-284): 8 hex chars from a random
// UUID, retried up to 100 times against the ids already in the session, then
// a full UUID as the fallback. The 8-char form is what makes session files
// readable by hand; the fallback keeps the id unique when it stops being.
func newEntryID(taken map[string]bool) string {
	for i := 0; i < 100; i++ {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			// crypto/rand failing means the OS entropy source is gone;
			// colliding entry ids would corrupt the parent chain, so panic
			// is the honest outcome (same reasoning as NewUUID).
			panic("session: crypto/rand unavailable: " + err.Error())
		}
		id := hex.EncodeToString(b[:])
		if !taken[id] {
			return id
		}
	}
	return NewUUID()
}

// sessionIDPattern is pi's assertValidSessionId (session-manager.ts:268-274):
// non-empty, alphanumeric plus '-', '_', '.', starting and ending alphanumeric.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// ValidSessionID reports whether id is acceptable as a session id.
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

var (
	uuid7Mu          sync.Mutex
	uuid7LastMilliTS int64
)

// NewSessionID returns a time-ordered UUIDv7 string.
//
// pi's createSessionId is uuidv7() (session-manager.ts:264-266,
// packages/ai/src/utils/uuid.ts). The layout is RFC 9562's: 48 bits of
// millisecond timestamp, version nibble 7, variant 10. The timestamp makes
// session ids sort by creation, which is what the file-name ordering and
// "most recent session" discovery rely on.
//
// pi clamps an ordinary call to at least the last timestamp it handed out
// (Math.max(requestedTimestamp, lastOrdinaryTimestamp), uuid.ts:15-16); the
// same clamp lives here, guarded by a mutex because time.Now has millisecond
// resolution and a burst of NewSessionID calls inside one millisecond would
// otherwise regress the clock.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("session: crypto/rand unavailable: " + err.Error())
	}

	uuid7Mu.Lock()
	milli := time.Now().UTC().UnixMilli()
	if milli < uuid7LastMilliTS {
		milli = uuid7LastMilliTS
	}
	uuid7LastMilliTS = milli
	uuid7Mu.Unlock()

	b[0] = byte(milli >> 40)
	b[1] = byte(milli >> 32)
	b[2] = byte(milli >> 24)
	b[3] = byte(milli >> 16)
	b[4] = byte(milli >> 8)
	b[5] = byte(milli)
	b[6] = 0x70 | (b[6] & 0x0f) // version 7
	b[8] = 0x80 | (b[8] & 0x3f) // variant 10x

	buf := make([]byte, 36)
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf)
}

// EncodeCWD maps a working directory to its session directory name.
//
// This is pi's getDefaultSessionDirPath safe-path algorithm
// (session-manager.ts:589-594) and it must stay byte-identical: a different
// encoding splits one project's sessions across two directories, and neither
// implementation can see the other's files.
//
//	resolvedCwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")
//
// One leading separator is stripped (not all of them — the regex has no +
// quantifier), then every slash, backslash and colon becomes a hyphen, and
// the result is wrapped in -- --. "D:\develop\Project\Ultrac" becomes
// "--D-develop-Project-Ultrac--"; "/home/me/repo" becomes "--home-me-repo--".
//
// Note what is NOT done: no lowercasing (pi does not), no symlink resolution
// (pi's resolvePath normalizes and absolutizes only), no hash suffix for long
// paths (pi has no truncation here either — the OS path limit applies).
func EncodeCWD(cwd string) string {
	resolved := resolveSessionPath(cwd)
	trimmed := resolved
	if len(trimmed) > 0 && (trimmed[0] == '/' || trimmed[0] == '\\') {
		trimmed = trimmed[1:]
	}
	var b strings.Builder
	b.Grow(len(trimmed) + 4)
	b.WriteString("--")
	for i := 0; i < len(trimmed); i++ {
		switch trimmed[i] {
		case '/', '\\', ':':
			b.WriteByte('-')
		default:
			b.WriteByte(trimmed[i])
		}
	}
	b.WriteString("--")
	return b.String()
}

// SessionDirFor returns the directory holding a cwd's session files:
// ~/.ultrac/sessions/<encoded-cwd>. The encoding is pi's
// getDefaultSessionDirPath algorithm, kept verbatim because it is a data
// layout with tests behind it — the root is ours.
func SessionDirFor(cwd string) string {
	return filepath.Join(config.SessionsDir(), EncodeCWD(cwd))
}

// SessionFileName builds the file name for a session: the header timestamp
// with ':' and '.' replaced by '-' (filesystem-safe), then the session id.
//
// pi: session-manager.ts:1079-1080, `timestamp.replace(/[:.]/g, "-")`.
// "2026-10-15T08:40:50.123Z" becomes "2026-10-15T08-40-50-123Z".
func SessionFileName(timestamp, sessionID string) string {
	safe := strings.NewReplacer(":", "-", ".", "-").Replace(timestamp)
	return safe + "_" + sessionID + ".jsonl"
}

// resolveSessionPath absolutizes and cleans a path without resolving
// symlinks — pi's resolvePath (utils/paths.ts:103-107) does the same, and the
// session directory of a symlinked checkout must not depend on the link
// target or the two implementations disagree.
func resolveSessionPath(p string) string {
	if p == "" {
		if wd, err := os.Getwd(); err == nil {
			return filepath.Clean(wd)
		}
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Clean(abs)
}
