package models

// write.go — the models.json writer (docs/开发计划/新功能计划/custom-model-plan.md
// P1): the disk half of the desktop shell's add-custom-model flow.
//
// # Why a struct round-trip is safe here (plan D5)
//
// The schema is additionalProperties:false at every level, and
// ProviderConfig / ModelDefinition carry exactly the properties the
// schema declares — no field can enter the struct that the schema would
// reject, and none the schema allows is missing. So marshalling a
// decoded Config loses nothing, and the self-check below (marshal →
// Parse) is a genuine guard rather than a ritual: a Config that somehow
// encodes an invalid document is refused before it reaches the disk.
//
// (The credential files this used to sit next to were the opposite case: their
// reader dropped oauth entries, so they needed a raw-preserving writer.)

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Save writes cfg to path as models.json, atomically.
//
// The self-check comes first: the document is marshalled and pushed
// through Parse — the same validation Load applies — and a document
// that fails never reaches the disk, so a bad Config is a returned
// error with the file left untouched, not a warning on the next launch.
//
// The write is MkdirAll(dir) → temp file in the same directory → rename
// onto the path. The rename is the atomic step: a concurrent reader
// (loadModelSnapshot at startup) sees the old file or the new one, never
// half of either.
//
// Permissions follow the plan: 0600 when any provider carries a literal
// apiKey (the file then holds a credential), 0644 otherwise. The mode
// applies on creation and is re-asserted with chmod on every write, so a
// tmp left over by a crashed writer cannot keep stale bits.
func Save(path string, cfg Config) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("models: save: empty path")
	}
	// A nil map marshals as null, which the schema rejects ("providers"
	// is a plain object). Normalise so an empty config writes
	// {"providers": {}} and round-trips through Load unchanged.
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("models: encode models.json: %w", err)
	}
	// The TypeScript writers terminate the file with a newline; matching
	// them keeps hand-diffs between implementations clean.
	raw = append(raw, '\n')
	if _, err := Parse(raw); err != nil {
		return fmt.Errorf("models: refusing to write an invalid document:\n%w", err)
	}

	perm := os.FileMode(0o644)
	if configHasAPIKey(cfg) {
		perm = 0o600
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("models: create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".models-*.tmp")
	if err != nil {
		return fmt.Errorf("models: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort: after a successful rename this finds nothing to
	// remove; after a failed one it must not leave the tmp behind.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("models: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("models: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("models: chmod %s: %w", tmpName, err)
	}
	if err := replaceFile(tmpName, path); err != nil {
		return fmt.Errorf("models: replace %s: %w", path, err)
	}
	return nil
}

// configHasAPIKey reports whether any provider entry carries a literal
// apiKey — the condition for the 0600 mode (plan P1).
func configHasAPIKey(cfg Config) bool {
	for _, p := range cfg.Providers {
		if p.APIKey != "" {
			return true
		}
	}
	return false
}

// renameAttempts and renameBackoff bound the Windows retry. The facts
// were probed on this machine and read out of Go's own source: os.Rename is
// MoveFileEx(MOVEFILE_REPLACE_EXISTING), so replacing an existing file
// is one atomic syscall — no remove-then-rename dance is needed — and
// the same rename onto a file a reader still holds fails with
// ERROR_ACCESS_DENIED (fs.ErrPermission). A reader holds models.json for
// microseconds (os.ReadFile), so the schedule — 2+4+…+512ms, about a
// second — clears the ordinary reader a thousand times over. A holder
// that holds longer is a real lock, and the last real error is what
// Save reports.
//
// Kept here rather than shared: this package must not depend on a caller's
// writer, and the helper is twenty lines of syscall facts, not policy.
const (
	renameAttempts = 10
	renameBackoff  = 2 * time.Millisecond
)

// replaceFile moves tmp onto path, retrying the Windows held-file case.
// Anything that is not permission-ish (a cross-device rename) fails fast
// with the target untouched.
//
// errors.Is(err, fs.ErrPermission) covers both platforms the retry needs:
// Windows ERROR_ACCESS_DENIED maps to it, and POSIX EACCES/EPERM always
// did.
func replaceFile(tmp, path string) error {
	var lastErr error
	for attempt := range renameAttempts {
		err := os.Rename(tmp, path)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrPermission) {
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
