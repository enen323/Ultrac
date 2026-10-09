// Package session stores conversation transcripts as JSONL.
//
// # File format compatibility is the whole point
//
// ~/.ultrac/projects/<sanitized-cwd>/<session-id>.jsonl is read by the
// TypeScript implementation, by --resume, and by hand when a session goes
// wrong. Every entry here is append-only, one JSON object per line, and every
// unknown field is preserved. A rewrite that "improved" the shape would strand
// every existing session, so the shapes below are dictated by the files, not
// chosen for elegance.
//
// # Why JSONL and not a database
//
// Three reasons, all practical. A crashed session leaves a truncated last line
// instead of a corrupt database. Appending is a single write syscall, which is
// what makes per-turn durability cheap. And `tail` works, which is how these
// files get debugged at 2am.
//
// # Layering
//
// Part of the capability layer (L2). It imports internal/logging for error
// types and internal/config for the config directory, both below it. It imports
// nothing from internal/engine.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// EntryType discriminates a transcript entry.
//
// These strings are on-disk values. The set is open — an entry type this
// version has never heard of still round-trips through Extra — but the four
// transcript types below are the ones the chain logic branches on.
type EntryType string

const (
	// EntryUser is a human turn.
	EntryUser EntryType = "user"
	// EntryAssistant is a model turn.
	EntryAssistant EntryType = "assistant"
	// EntryAttachment is non-conversational injected content.
	EntryAttachment EntryType = "attachment"
	// EntrySystem is a system notice recorded in the transcript.
	EntrySystem EntryType = "system"

	// EntryProgress is tool progress. NOT a transcript message.
	//
	// Progress is ephemeral UI state. It is written to the file but excluded
	// from the conversation chain, because including it forked the chain and
	// orphaned real messages on resume. This is the single most load-bearing
	// distinction in the format.
	EntryProgress EntryType = "progress"

	// EntrySummary records a compaction summary.
	EntrySummary EntryType = "summary"
	// EntryLastPrompt records the most recent prompt, for the session list.
	EntryLastPrompt EntryType = "last-prompt"
	// EntryCustomTitle records a user-set session title.
	EntryCustomTitle EntryType = "custom-title"
	// EntryQueueOperation records an enqueue/dequeue of a queued message.
	EntryQueueOperation EntryType = "queue-operation"
)

// IsTranscript reports whether an entry participates in the conversation.
//
// The test is the single source of truth for what loadable messages are.
// Progress is the exclusion that matters; see EntryProgress.
func IsTranscript(t EntryType) bool {
	switch t {
	case EntryUser, EntryAssistant, EntryAttachment, EntrySystem:
		return true
	default:
		return false
	}
}

// IsChainParticipant reports whether an entry is assigned a parentUuid.
//
// Progress is written to the file but nothing chains *to* it. Old transcripts
// written before that rule existed still contain progress entries with a
// parentUuid, and the reader bridges across them.
func IsChainParticipant(t EntryType) bool { return t != EntryProgress }

// Entry is one line of a transcript.
//
// UUID and ParentUUID form the chain: ParentUUID points at the entry that
// preceded this one in the conversation. A sidechain entry (a subagent) has
// IsSidechain set, which is what keeps a subagent's turns from interleaving
// into the main chain and producing a fork.
//
// Message carries the provider-native message. It is left as raw JSON on
// purpose: the transcript records what was sent and received, so re-serialising
// it through a Go struct would lose fields the model sent and quietly change
// the conversation on resume.
type Entry struct {
	Type           EntryType `json:"type"`
	UUID           string    `json:"uuid"`
	ParentUUID     string    `json:"parentUuid"`
	Timestamp      string    `json:"timestamp"`
	Message        any       `json:"message,omitempty"`
	IsSidechain    bool      `json:"isSidechain"`
	SessionID      string    `json:"sessionId,omitempty"`
	CWD            string    `json:"cwd,omitempty"`
	Version        string    `json:"version,omitempty"`
	UserType       string    `json:"userType,omitempty"`
	Entrypoint     string    `json:"entrypoint,omitempty"`
	PermissionMode string    `json:"permissionMode,omitempty"`
	GitBranch      string    `json:"gitBranch,omitempty"`
	PromptID       string    `json:"promptId,omitempty"`

	// LastPrompt is set on EntryLastPrompt entries.
	LastPrompt string `json:"lastPrompt,omitempty"`
	// Operation is set on EntryQueueOperation entries: enqueue or dequeue.
	Operation string `json:"operation,omitempty"`
	// Content is the queued text on EntryQueueOperation entries.
	Content string `json:"content,omitempty"`

	// Extra preserves every field this version does not model. Same reasoning
	// as config.Settings: a transcript is shared with the TypeScript reader,
	// and dropping a field it understands would change the conversation on
	// resume.
	Extra map[string]any `json:"-"`
}

// userMessage is the on-disk shape of a user entry's message.
type userMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// NewUserEntry builds a user entry with a fresh uuid.
func NewUserEntry(content any, sessionID, cwd string) *Entry {
	return &Entry{
		Type:      EntryUser,
		UUID:      NewUUID(),
		Timestamp: NowISO(),
		Message:   userMessage{Role: "user", Content: content},
		SessionID: sessionID,
		CWD:       cwd,
	}
}

// NewAssistantEntry builds an assistant entry with a fresh uuid.
func NewAssistantEntry(message any, sessionID, cwd string) *Entry {
	return &Entry{
		Type:      EntryAssistant,
		UUID:      NewUUID(),
		Timestamp: NowISO(),
		Message:   message,
		SessionID: sessionID,
		CWD:       cwd,
	}
}

// NewUUID returns a random RFC 4122 version 4 UUID.
//
// The format is load-bearing: session file names are UUIDs, and the TypeScript
// side validates them with the same regex before it will open a file.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the OS entropy source is gone. Returning
		// zeros would produce colliding session ids that overwrite each
		// other's transcripts, so panic is the honest outcome: this process
		// cannot safely name a file.
		panic("session: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

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

var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidUUID reports whether s is a UUID, matching the TypeScript validator.
func ValidUUID(s string) bool { return uuidPattern.MatchString(s) }

// maxSanitizedLength bounds one path component.
//
// Most filesystems cap a component at 255 bytes. 200 leaves room for the hash
// suffix and its separator. This value must match the TypeScript side exactly:
// a different number means sessions saved under one implementation are
// invisible to the other for any path long enough to truncate.
const maxSanitizedLength = 200

// SanitizePath makes a path safe as a directory or file name.
//
// Every non-alphanumeric character becomes a hyphen, which also handles the
// Windows reserved set (colons, asterisks, question marks) for free.
//
// A path longer than 200 characters is truncated and given a hash suffix.
// Without the suffix, two long paths sharing a 200-character prefix would
// resolve to the same directory and their sessions would interleave — sessions
// from different repositories appearing in each other's history.
func SanitizePath(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	sanitized := b.String()
	if len(sanitized) <= maxSanitizedLength {
		return sanitized
	}
	// Hash the ORIGINAL, not the sanitized prefix: two paths that differ only
	// in a character the sanitiser collapsed must not collide.
	return sanitized[:maxSanitizedLength] + "-" + SimpleHash(name)
}

// SimpleHash is djb2 rendered base-36.
//
// djb2 rather than a cryptographic hash because this only needs to be stable
// and cheap, and because the TypeScript side uses the same algorithm — a
// different hash means a different directory name means orphaned sessions.
//
// Three details exist purely so the two implementations agree:
//
//   - The iteration is over UTF-16 code units, because that is what
//     `str.charCodeAt(i)` walks. Walking runes instead would hash a different
//     sequence for any non-ASCII path and produce a different directory name
//     for the same directory.
//   - The accumulator is int32. Go's int32 arithmetic already wraps on
//     overflow, which is what JavaScript's `| 0` coerces to, so no explicit
//     truncation is needed — the type is the truncation.
//   - A negative result is negated, matching `hash.ts`.
func SimpleHash(s string) string {
	var h int32
	for _, unit := range utf16.Encode([]rune(s)) {
		h = (h << 5) - h + int32(unit)
	}
	if h < 0 {
		h = -h
	}
	return strconv.FormatInt(int64(h), 36)
}
