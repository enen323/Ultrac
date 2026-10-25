package auth

// update_test.go — the single-provider writer (custom-model-plan.md D6).
//
// What is under test: Update's preservation promise — everything the
// loader cannot use (an oauth block, a malformed entry) survives a write
// that only meant to touch one provider — plus its refusals (a file it
// cannot parse is never rewritten) and its no-op cases.

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateInsertsIntoMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json") // does not exist yet
	if err := Update(path, "my-proxy", "sk-new", false); err != nil {
		t.Fatalf("Update: %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Entries["my-proxy"].Key; got != "sk-new" {
		t.Errorf("key = %q, want sk-new", got)
	}
	if f.Entries["my-proxy"].Type != EntryTypeAPIKey {
		t.Errorf("type = %q, want %q", f.Entries["my-proxy"].Type, EntryTypeAPIKey)
	}
	// The file holds a credential: WriteFile's 0600 applies.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("mode = %o, want 600", mode)
		}
	}
}

func TestUpdateReplacesExistingKey(t *testing.T) {
	path := write(t, `{
  "keep": {"type": "api_key", "key": "sk-keep"},
  "my-proxy": {"type": "api_key", "key": "sk-old"}
}`)
	if err := Update(path, "my-proxy", "sk-new", false); err != nil {
		t.Fatalf("Update: %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Entries["my-proxy"].Key; got != "sk-new" {
		t.Errorf("key = %q, want sk-new (replaced)", got)
	}
	if got := f.Entries["keep"].Key; got != "sk-keep" {
		t.Errorf("keep = %q, want the untouched neighbour", got)
	}
}

// TestUpdatePreservesOAuthEntry — D6's whole point: Load drops oauth
// entries (they land in Problems), so a File round-trip would erase one.
// Update rewrites the document raw, so the oauth block survives — still
// rejected by the loader, but still THERE for the day oauth lands.
func TestUpdatePreservesOAuthEntry(t *testing.T) {
	path := write(t, `{
  "oauth-provider": {
    "type": "oauth",
    "access": "tok-access",
    "refresh": "tok-refresh",
    "expires": 1790000000000
  },
  "my-proxy": {"type": "api_key", "key": "sk-old"}
}`)
	if err := Update(path, "my-proxy", "sk-new", false); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// The loader's view: the new key resolves, the oauth block is still
	// a problem (not an entry) — it was not silently promoted or lost.
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Entries["my-proxy"].Key; got != "sk-new" {
		t.Errorf("key = %q, want sk-new", got)
	}
	var oauthErr *OAuthUnsupportedError
	if err := f.Problems["oauth-provider"]; !errors.As(err, &oauthErr) {
		t.Fatalf("oauth-provider problem = %v, want the oauth rejection (the block survived)", err)
	}

	// And the bytes themselves still carry the block.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"oauth"`, "tok-access", "tok-refresh", "1790000000000"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("file lost %q after the update:\n%s", want, raw)
		}
	}
}

// TestUpdatePreservesMalformedEntry — a provider whose entry is not even
// an object is kept as-is: Update's job is one provider, not a repair
// shop. (The loader will keep reporting it as a problem, which is the
// honest outcome.)
func TestUpdatePreservesMalformedEntry(t *testing.T) {
	path := write(t, `{
  "weird": 42,
  "my-proxy": {"type": "api_key", "key": "sk-old"}
}`)
	if err := Update(path, "my-proxy", "sk-new", false); err != nil {
		t.Fatalf("Update: %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Problems["weird"] == nil {
		t.Error("the malformed entry must survive as a problem, not vanish")
	}
	if got := f.Entries["my-proxy"].Key; got != "sk-new" {
		t.Errorf("key = %q, want sk-new", got)
	}
}

func TestUpdateRemovesEntry(t *testing.T) {
	path := write(t, `{
  "keep": {"type": "api_key", "key": "sk-keep"},
  "gone": {"type": "api_key", "key": "sk-gone"}
}`)
	if err := Update(path, "gone", "", true); err != nil {
		t.Fatalf("Update(remove): %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := f.Entries["gone"]; ok {
		t.Error("the removed provider must not resolve")
	}
	if got := f.Entries["keep"].Key; got != "sk-keep" {
		t.Errorf("keep = %q, want the untouched neighbour", got)
	}
}

func TestUpdateRemoveAbsentIsNoOp(t *testing.T) {
	original := `{"keep": {"type": "api_key", "key": "sk-keep"}}`
	path := write(t, original)
	if err := Update(path, "never-was-there", "", true); err != nil {
		t.Fatalf("Update(remove absent): %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Entries) != 1 || f.Entries["keep"].Key != "sk-keep" {
		t.Errorf("entries = %v, want keep untouched", f.Entries)
	}
}

// TestUpdateRefusesUnparseableFile — a document we cannot read is a
// document we must not rewrite: the update fails and the file keeps
// whatever the user has (Load reports the same parse failure as a
// warning, so the user learns about it either way).
func TestUpdateRefusesUnparseableFile(t *testing.T) {
	original := `{"keep": {"type": "api_key", "key": "sk-keep"` // truncated
	path := write(t, original)
	err := Update(path, "my-proxy", "sk-new", false)
	if err == nil {
		t.Fatal("Update must refuse a file it cannot parse")
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != original {
		t.Errorf("file = %q, want the original untouched", raw)
	}
}

// TestUpdateRefusesNonObjectDocument — an array (or a bare string) is
// not an auth.json; the same refusal applies.
func TestUpdateRefusesNonObjectDocument(t *testing.T) {
	path := write(t, `["not", "an", "object"]`)
	if err := Update(path, "my-proxy", "sk-new", false); err == nil {
		t.Fatal("Update must refuse a non-object document")
	}
}

func TestUpdateRejectsBadArguments(t *testing.T) {
	path := write(t, `{}`)
	if err := Update(path, "", "sk-new", false); err == nil {
		t.Error("an empty provider must be refused")
	}
	if err := Update(path, "my-proxy", "", false); err == nil {
		t.Error("an empty key (without remove) must be refused")
	}
	if err := Update("   ", "my-proxy", "sk-new", false); err == nil {
		t.Error("an empty path must be refused")
	}
}
