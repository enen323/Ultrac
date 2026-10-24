// Package auth reads and writes the user's auth.json credential store
// (docs/开发计划/新功能计划/provider-config-plan.md P2).
//
// # Provenance
//
// A port of pi's credential-storage layer:
//
//   - packages/coding-agent/src/core/auth-storage.ts:49
//     (FileAuthStorageBackend — the file, the modes, the lock) and :203
//     (ReadOnlyAuthStorage — the load and validation rules).
//
// Three deviations from pi, all decided in the plan:
//
//   - No file lock (§4.5). Nothing in this plan writes auth.json — it is
//     a hand-edited file like settings.json — so there are no competing
//     writers to serialise, and the atomic replace (tmp + rename) is the
//     stronger guarantee anyway: a reader sees the old file or the new
//     one, never half of either. (On Windows the replace casts a
//     microsecond shadow on a third reader's open; both sides retry it
//     — see readFile and replaceFile.) The lock design is archived in
//     §4.5 for the day a writer appears (`ultrac auth set`, an oauth
//     login flow).
//   - An oauth credential is rejected explicitly (D4), where pi accepts
//     it: Ultrac has no oauth flow yet, and a stored token nobody can
//     refresh is a confusing way to learn that.
//   - A read failure degrades per provider instead of throwing (§4.5):
//     an editor's half-save, or one malformed entry, must not take the
//     other providers' credentials down with it.
//
// # The key is a literal
//
// There is no value-resolution layer (D5/D6): no $VAR expansion, no
// !command execution. What the file says is what is sent. pi's
// resolveConfigValue design is archived in the plan's §2 for the day it
// is needed.
//
// # The two layers
//
// Like models.json, auth.json exists at both <ConfigHome>/auth.json and
// <cwd>/.ultrac/auth.json, project winning per provider id wholesale (D1).
// The caller names the paths; this package never computes them, so the
// layering policy stays where the settings layering already lives.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// EntryTypeAPIKey is the only credential type Ultrac accepts. It is a
// constant rather than a bare string because P3's credential chain
// switches on it, and a typo there is a silent auth failure.
const EntryTypeAPIKey = "api_key"

// Entry is one provider's credential, exactly as the file spells it.
type Entry struct {
	// Type is EntryTypeAPIKey — the only accepted value.
	Type string `json:"type"`
	// Key is the literal credential: read as-is, sent as-is (D5/D6).
	// An api_key entry without a key is valid but useless; APIKey
	// reports "" for it and the caller falls through its chain.
	Key string `json:"key"`
}

// File is one decoded auth.json document.
//
// Entries holds the providers whose credential is usable; Problems holds
// the ones that are not, with the reason. The split is the degradation
// rule: a file with problems still loads, so one malformed entry (or an
// oauth block this build cannot use) costs exactly one provider.
type File struct {
	Entries  map[string]Entry
	Problems map[string]error
}

// Stack is the merged view over the loaded layers: built-in-free (auth
// has no built-in entries), project winning per provider id. It is
// immutable once built; every accessor is a plain read.
type Stack struct {
	entries  map[string]Entry
	problems map[string]error
}

// Build merges files over each other, lowest precedence first (the
// ConfigHome layer before the project layer). A later file's provider
// entry — or provider problem — replaces whatever an earlier file said
// about that provider, wholesale (D1): a good project entry heals a
// broken ConfigHome one, and a rejecting project entry shadows a
// ConfigHome credential. There is no built-in layer to seed from: auth
// has no defaults.
func Build(files ...File) *Stack {
	s := &Stack{
		entries:  make(map[string]Entry),
		problems: make(map[string]error),
	}
	for _, f := range files {
		for id, e := range f.Entries {
			delete(s.problems, id)
			s.entries[id] = e
		}
		for id, p := range f.Problems {
			delete(s.entries, id)
			s.problems[id] = p
		}
	}
	return s
}

// LoadStack loads every path in order and merges the results. A path
// that fails to load is reported through warn (when non-nil) and
// contributes nothing — the same "a broken file is a warning, not a
// stop" rule the settings loader applies (config/loader.go:75). ENOENT
// is not a failure: most machines have no auth.json at all.
func LoadStack(paths []string, warn func(error)) *Stack {
	files := make([]File, 0, len(paths))
	for _, path := range paths {
		f, err := Load(path)
		if err != nil {
			if warn != nil {
				warn(err)
			}
			continue
		}
		files = append(files, f)
	}
	return Build(files...)
}

// Load reads one auth.json file.
//
// The semantics mirror the models.json loader (and pi's
// model-config.ts:124):
//
//   - an empty path or ENOENT yields an empty file and no error — "the
//     user has no auth.json" is the normal case;
//   - a read failure, or a document that is not a JSON object, yields an
//     error naming the file. The caller downgrades it to a warning;
//   - anything wrong with an individual entry is a per-provider problem
//     in the returned File, not a file-level error, so the other
//     providers survive.
//
// A UTF-8 BOM is stripped before parsing (editors on Windows write one,
// and encoding/json rejects it with a confusing "invalid character").
func Load(path string) (File, error) {
	if strings.TrimSpace(path) == "" {
		return File{}, nil
	}
	raw, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return File{}, nil
		}
		return File{}, &Error{Path: path, Op: "read", Err: err}
	}
	raw = stripBOM(raw)

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A truncated save lands here: the file is unusable, so it
		// degrades to "no credentials" with a warning (§4.5).
		return File{}, &Error{Path: path, Op: "parse", Err: err}
	}

	f := File{
		Entries:  make(map[string]Entry, len(doc)),
		Problems: make(map[string]error),
	}
	for _, id := range slices.Sorted(maps.Keys(doc)) {
		var raw rawEntry
		if err := json.Unmarshal(doc[id], &raw); err != nil {
			// Not an object, or a non-string "type". Either way the entry
			// cannot name a credential shape.
			f.Problems[id] = fmt.Errorf("auth.json: provider %q: credential must be an object with a string \"type\"", id)
			continue
		}
		switch {
		case raw.Type == EntryTypeAPIKey:
			key, err := raw.keyValue()
			if err != nil {
				f.Problems[id] = fmt.Errorf("auth.json: provider %q: %w", id, err)
				continue
			}
			f.Entries[id] = Entry{Type: EntryTypeAPIKey, Key: key}
		case raw.Type == "oauth":
			// D4: accepted by the schema, refused by the credential
			// chain. pi stores these; Ultrac cannot use them yet.
			f.Problems[id] = &OAuthUnsupportedError{Provider: id}
		case raw.Type == "":
			f.Problems[id] = fmt.Errorf("auth.json: provider %q: credential has no \"type\" (expected %q)", id, EntryTypeAPIKey)
		default:
			f.Problems[id] = fmt.Errorf("auth.json: provider %q: unsupported credential type %q (only %q is supported)", id, raw.Type, EntryTypeAPIKey)
		}
	}
	return f, nil
}

// rawEntry is the on-disk shape before validation. Key stays raw so a
// non-string key (a number, an object) is a named problem instead of a
// decode failure that blames the whole entry.
type rawEntry struct {
	Type string          `json:"type"`
	Key  json.RawMessage `json:"key"`
}

// keyValue reports the literal key. Absent is fine (an api_key entry
// with no key is valid but useless — APIKey reports "" for it); a
// non-string is a problem, because the key is a literal and a number
// here is a mistake worth naming.
func (r rawEntry) keyValue() (string, error) {
	if len(r.Key) == 0 || string(r.Key) == "null" {
		return "", nil
	}
	var key string
	if err := json.Unmarshal(r.Key, &key); err != nil {
		return "", fmt.Errorf("key must be a string (literal values only)")
	}
	return key, nil
}

// Providers lists the provider ids with a usable credential, in lexical
// order. Rejected providers are not listed; Problems reports them.
func (s *Stack) Providers() []string {
	return slices.Sorted(maps.Keys(s.entries))
}

// Entry returns the stored credential for a provider. The bool is false
// when the provider has no entry — including when it has a problem,
// which Problem explains.
func (s *Stack) Entry(provider string) (Entry, bool) {
	e, ok := s.entries[provider]
	return e, ok
}

// APIKey returns the literal key for a provider, or "" when there is
// none: no entry, an entry without a key, or a rejected entry all read
// as "no credential here". The credential chain (P3) distinguishes the
// cases with Entry and Problem.
func (s *Stack) APIKey(provider string) string {
	return s.entries[provider].Key
}

// Problem returns why a provider's credential is unusable, or nil. A
// provider can have at most one of an entry and a problem.
func (s *Stack) Problem(provider string) error {
	return s.problems[provider]
}

// Problems lists every per-provider problem, ordered by provider id.
func (s *Stack) Problems() []error {
	out := make([]error, 0, len(s.problems))
	for _, id := range slices.Sorted(maps.Keys(s.problems)) {
		out = append(out, s.problems[id])
	}
	return out
}

// OAuthUnsupportedError reports an auth.json oauth credential. pi
// accepts these; this build cannot use them (no oauth flow yet — plan
// §1.2), so the entry is refused rather than stored and ignored the way
// models.json's oauth field is (D4 draws exactly that line).
type OAuthUnsupportedError struct {
	Provider string
}

func (e *OAuthUnsupportedError) Error() string {
	return fmt.Sprintf("auth.json: provider %q declares an oauth credential, which Ultrac does not support yet (only %q credentials work; oauth login is planned for a later phase)", e.Provider, EntryTypeAPIKey)
}

// Error is a file-level failure. It names the file because two layers
// exist and "failed to load auth.json" without a path is unactionable.
type Error struct {
	Path string
	Op   string
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("auth.json: %s %s: %v", e.Op, e.Path, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// WriteFile atomically replaces path with data.
//
// The write is: MkdirAll(dir, 0700) → write the sibling tmp at 0600 →
// rename onto the path. The rename is the atomic step — a concurrent
// reader sees the old file or the new one, never half of either (§4.4).
// The mode applies on creation; an administrator-set ACL on an existing
// file is left alone, the way pi's writer documents it.
//
// Nothing in this plan calls WriteFile: it exists for the tests here and
// for the writers P3+ will add (`ultrac auth set`, an oauth login flow).
// It is exercised, not speculative.
func WriteFile(path string, data []byte) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("auth: write: empty path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return &Error{Path: path, Op: "create directory", Err: err}
	}

	// A fixed sibling name, as the plan prescribes (§4.4). No writer
	// exists to race another writer for it; when one appears, this is
	// where the lock from §4.5 plugs in.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return &Error{Path: path, Op: "create temp file", Err: err}
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return &Error{Path: path, Op: "write", Err: err}
	}
	// The mode argument above only applies when the file is created; a
	// tmp left over by a crashed writer keeps its old bits. Chmod makes
	// 0600 true on every write (Unix; Windows has no mode bits beyond
	// read-only, which 0600 leaves clear).
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return &Error{Path: path, Op: "close temp file", Err: err}
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return &Error{Path: path, Op: "chmod temp file", Err: err}
	}

	if err := replaceFile(tmp, path); err != nil {
		os.Remove(tmp)
		return &Error{Path: path, Op: "replace", Err: err}
	}
	return nil
}

// readAttempts and readBackoff bound the reader-side retry of the
// Windows replace shadow. The shadow is microseconds wide (see
// readFile), so 1+2+4ms is a thousand times generous; a file that stays
// unreadable longer than that is locked for real and the real error is
// what Load reports.
const (
	readAttempts = 4
	readBackoff  = time.Millisecond
)

// readFile reads path, retrying the shadow a concurrent replace casts.
//
// On Windows, replacing a file a reader holds is not transparent to a
// third reader: MoveFileEx(MOVEFILE_REPLACE_EXISTING) — what os.Rename
// is — opens a microsecond transaction on the target, and an open that
// lands inside it fails with ERROR_SHARING_VIOLATION or
// ERROR_ACCESS_DENIED. Probed on this machine under a 1ms-polling
// hammer: ~0.5% of reads hit the shadow, zero torn reads (the rename
// itself is atomic — the reader gets the old inode or the new one). The
// package contract — "a reader sees the old file or the new one" — is
// only literally true if the read path retries the shadow, so it does.
// On POSIX the rename is transparent and the retry never fires.
//
// ERROR_SHARING_VIOLATION is errno 32; the syscall package neither
// exports it nor maps it to fs.ErrPermission, hence the literal.
func readFile(path string) ([]byte, error) {
	var lastErr error
	for attempt := range readAttempts {
		raw, err := os.ReadFile(path)
		if err == nil {
			return raw, nil
		}
		if !replaceShadow(err) {
			return nil, err
		}
		lastErr = err
		if attempt+1 == readAttempts {
			break
		}
		time.Sleep(readBackoff << attempt)
	}
	return nil, lastErr
}

// replaceShadow reports whether err is the transient failure a
// concurrent replace casts on Windows — the family both sides of the
// replace retry: the writer's rename onto a held file, and a reader's
// open inside the replace transaction.
func replaceShadow(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true // ERROR_ACCESS_DENIED (5) and POSIX EACCES/EPERM
	}
	var errno syscall.Errno
	return runtime.GOOS == "windows" && errors.As(err, &errno) && errno == 32 // ERROR_SHARING_VIOLATION
}

// renameAttempts and renameBackoff bound the Windows retry. A reader
// holds auth.json for microseconds (os.ReadFile opens, reads, closes),
// so the full schedule — 2+4+…+512ms, about a second — clears the
// ordinary reader a thousand times over and even a scanner that holds
// the file for a few hundred milliseconds. A holder that holds longer
// than that is the §4.5 lock's problem, not a longer backoff's.
const (
	renameAttempts = 10
	renameBackoff  = 2 * time.Millisecond
)

// replaceFile moves tmp onto path, retrying the Windows held-file case.
//
// The facts come from probing this machine and from the Go runtime's
// own source, not from guessing:
//
//   - os.Rename is MoveFileEx(MOVEFILE_REPLACE_EXISTING)
//     (internal/syscall/windows:357), so replacing an existing file is
//     one atomic syscall: no remove-then-rename dance is needed when
//     nobody holds the target;
//   - the same rename onto a file a reader still holds fails with
//     ERROR_ACCESS_DENIED, which errors.Is reports as fs.ErrPermission;
//   - removing the target first cannot help in that case — the remove
//     fails with ERROR_SHARING_VIOLATION — and when it does succeed it
//     is worse than useless: between the delete and the rename the path
//     is missing or pending-delete, and a concurrent reader's open
//     fails with ERROR_ACCESS_DENIED. (The repo's older writers do
//     that dance — config/loader.go:537, session/store.go:754 — where
//     no reader is ever concurrent, so it costs them nothing; auth's
//     acceptance test reads while the writer writes, so it must not.)
//
// So: retry the plain rename with backoff until the reader lets go. A
// reader sees the old file or the new one — never half of either, never
// a missing path. Anything that is not permission-ish (a cross-device
// rename) fails fast with the target untouched, and the error reported
// is the last real one, not a wrapped-up summary.
func replaceFile(tmp, path string) error {
	var lastErr error
	for attempt := range renameAttempts {
		err := os.Rename(tmp, path)
		if err == nil {
			return nil
		}
		if !replaceShadow(err) {
			return err
		}
		lastErr = err
		if attempt+1 == renameAttempts {
			break
		}
		time.Sleep(renameBackoff << attempt)
	}
	return lastErr
}

// stripBOM drops a leading UTF-8 byte-order mark.
func stripBOM(b []byte) []byte {
	const bom = "\xef\xbb\xbf"
	if len(b) >= len(bom) && string(b[:len(bom)]) == bom {
		return b[len(bom):]
	}
	return b
}
