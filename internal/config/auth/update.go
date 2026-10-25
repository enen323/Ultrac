package auth

// update.go — the single-provider writer for auth.json
// (docs/开发计划/新功能计划/custom-model-plan.md D6).
//
// # Why not the File round-trip
//
// Load keeps only api_key entries: an oauth block (or anything else it
// cannot use) lands in Problems and is dropped from the decoded File.
// Marshalling that File back would erase every credential this build
// cannot read — the user's oauth login, gone, because they added an
// api_key for a proxy. So Update never decodes into File: it reads the
// document as map[string]json.RawMessage, replaces (or removes) exactly
// one provider's raw entry, and writes everything else back byte-for-
// byte (modulo the encoder's whitespace normalisation).
//
// The write itself is WriteFile's — MkdirAll(0700), tmp at 0600, rename
// with the Windows shadow retry — so the atomicity and permission rules
// stay in one place.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Update inserts, replaces or removes one provider's credential in the
// auth.json at path.
//
// With remove false, key must be non-empty: an api_key entry without a
// key is valid but useless (Load reports "" for it), and writing one
// silently is how a typo becomes a mystery. With remove true the key is
// ignored and an absent provider is a successful no-op.
//
// A file that cannot be parsed is refused rather than rewritten —
// clobbering it would destroy whatever the user has there. ENOENT is not
// a failure: most machines have no auth.json, and the first Update
// creates it.
func Update(path string, provider, key string, remove bool) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("auth: update: empty path")
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("auth: update: empty provider")
	}
	if !remove && key == "" {
		return fmt.Errorf("auth: update: empty key for provider %q", provider)
	}

	doc := map[string]json.RawMessage{}
	raw, err := readFile(path)
	switch {
	case err == nil:
		raw = stripBOM(raw)
		if err := json.Unmarshal(raw, &doc); err != nil {
			// Not an object, or malformed JSON: refuse to touch it. The
			// caller's guidance is `ultrac config`, which reports the
			// same parse failure as a warning.
			return &Error{Path: path, Op: "parse", Err: err}
		}
	case errors.Is(err, fs.ErrNotExist):
		// The normal first-write case: start from an empty document.
	default:
		return &Error{Path: path, Op: "read", Err: err}
	}

	if remove {
		delete(doc, provider)
	} else {
		entry, err := json.Marshal(Entry{Type: EntryTypeAPIKey, Key: key})
		if err != nil {
			return fmt.Errorf("auth: encode entry for provider %q: %w", provider, err)
		}
		doc[provider] = entry
	}

	// Map marshalling sorts the keys, so the file's order is stable
	// across writes; the entries themselves keep their exact content.
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: encode %s: %w", path, err)
	}
	out = append(out, '\n')
	return WriteFile(path, out)
}
