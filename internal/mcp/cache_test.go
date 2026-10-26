package mcp

// cache_test.go — the persisted handshake snapshot
// (docs/开发计划/新功能计划/mcp-plan.md D4).
//
// The property that makes the cache worth having is that its key is stable
// across the changes that do not matter and unstable across the ones that do.
// A key that changes when a credential is rotated makes the cache useless; a
// key that does not change when the command does serves a server's old tools
// under its new name.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A credential rotation must not invalidate the snapshot: the tools a server
// offers do not depend on the token it was started with, and a re-handshake
// on every rotation would defeat the cache.
func TestSchemaCacheKeyIgnoresSecretValues(t *testing.T) {
	a := Spec{
		Name:    "fs",
		Type:    TransportStdio,
		Command: "npx",
		Args:    []string{"-y", "server-fs"},
		Env:     map[string]string{"TOKEN": "old"},
	}
	b := a
	b.Env = map[string]string{"TOKEN": "new"}

	if got, want := SchemaCacheKey(a), SchemaCacheKey(b); got != want {
		t.Errorf("rotating a secret changed the key: %s vs %s", got, want)
	}
}

// But adding or removing a variable DOES change what the server will do, so
// the key changes — by key name, never by value.
func TestSchemaCacheKeyChangesWhenTheEnvironmentShapeChanges(t *testing.T) {
	base := Spec{
		Name:    "fs",
		Type:    TransportStdio,
		Command: "npx",
		Env:     map[string]string{"TOKEN": "x"},
	}
	added := base
	added.Env = map[string]string{"TOKEN": "x", "EXTRA": "y"}
	if SchemaCacheKey(base) == SchemaCacheKey(added) {
		t.Error("adding an env key did not change the key")
	}

	renamed := base
	renamed.Env = map[string]string{"OTHER": "x"}
	if SchemaCacheKey(base) == SchemaCacheKey(renamed) {
		t.Error("renaming an env key did not change the key")
	}
}

func TestSchemaCacheKeyChangesWithLoadBearingFields(t *testing.T) {
	base := Spec{Name: "fs", Type: TransportStdio, Command: "npx", Args: []string{"-y", "server-fs"}}

	cases := map[string]Spec{
		"name":     {Name: "other", Type: TransportStdio, Command: "npx", Args: base.Args},
		"command":  {Name: "fs", Type: TransportStdio, Command: "node", Args: base.Args},
		"args":     {Name: "fs", Type: TransportStdio, Command: "npx", Args: []string{"-y", "other"}},
		"no args":  {Name: "fs", Type: TransportStdio, Command: "npx"},
		"disabled": {Name: "fs", Type: TransportStdio, Command: "npx", Args: base.Args, DisabledTools: []string{"write_file"}},
	}
	for label, other := range cases {
		if SchemaCacheKey(base) == SchemaCacheKey(other) {
			t.Errorf("changing %s did not change the key", label)
		}
	}
}

// The key must not depend on map iteration order, or two identical
// configurations would hash differently on two different runs.
func TestSchemaCacheKeyIsStableAcrossMapOrder(t *testing.T) {
	env := map[string]string{}
	for _, k := range []string{"d", "c", "b", "a"} {
		env[k] = "1"
	}
	want := SchemaCacheKey(Spec{Name: "fs", Command: "npx", Env: env})
	for i := 0; i < 20; i++ {
		if got := SchemaCacheKey(Spec{Name: "fs", Command: "npx", Env: env}); got != want {
			t.Fatalf("the key is not stable: %s vs %s", got, want)
		}
	}
}

// Separators, so no two field boundaries can be confused by concatenation.
func TestSchemaCacheKeyFieldsCannotCollide(t *testing.T) {
	a := SchemaCacheKey(Spec{Name: "ab", Type: TransportStdio, Command: "c"})
	b := SchemaCacheKey(Spec{Name: "a", Type: TransportStdio, Command: "bc"})
	if a == b {
		t.Error("two different specs share a key: the fields are not separated")
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	cacheHome(t)

	spec := Spec{Name: "fs", Type: TransportStdio, Command: "npx", Args: []string{"-y", "server-fs"}}
	key := SchemaCacheKey(spec)
	if err := SaveCachedSchema("fs", CachedSchema{
		CacheKey:     key,
		Instructions: "a filesystem server",
		Tools: []CachedTool{
			{Name: "read_file", Description: "reads", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "write_file", Description: "writes", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	cs, ok := LoadCachedSchema("fs", key)
	if !ok {
		t.Fatal("the snapshot was not loaded")
	}
	if len(cs.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(cs.Tools))
	}
	if cs.Tools[0].Name != "read_file" {
		t.Errorf("first tool = %q", cs.Tools[0].Name)
	}
	if cs.Instructions != "a filesystem server" {
		t.Errorf("instructions = %q", cs.Instructions)
	}
	if cs.LastValidated.IsZero() {
		t.Error("lastValidated was not set")
	}
}

func TestLoadRejectsAStaleKey(t *testing.T) {
	cacheHome(t)

	spec := Spec{Name: "fs", Type: TransportStdio, Command: "npx"}
	if err := SaveCachedSchema("fs", CachedSchema{
		CacheKey: SchemaCacheKey(spec),
		Tools:    []CachedTool{{Name: "read_file"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The command changed: the snapshot describes a different server now.
	moved := spec
	moved.Command = "node"
	if _, ok := LoadCachedSchema("fs", SchemaCacheKey(moved)); ok {
		t.Error("a stale snapshot was trusted")
	}
}

// A snapshot that has lost its tools must not be trusted: it would register a
// server with no tools, which presents as "my server does nothing" and has
// nothing pointing at the cache.
func TestLoadRejectsAnEmptyToolList(t *testing.T) {
	cacheHome(t)

	spec := Spec{Name: "fs", Type: TransportStdio, Command: "npx"}
	if err := SaveCachedSchema("fs", CachedSchema{
		CacheKey: SchemaCacheKey(spec),
		Tools:    []CachedTool{},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, ok := LoadCachedSchema("fs", SchemaCacheKey(spec)); ok {
		t.Error("an empty snapshot was trusted")
	}
}

// One bad entry must not cost the others: a corrupt schema in the middle of a
// good list is a cache hit with one fewer tool, not a miss.
func TestLoadDropsUnusableEntriesAndKeepsTheRest(t *testing.T) {
	cacheHome(t)

	spec := Spec{Name: "fs", Type: TransportStdio, Command: "npx"}
	if err := SaveCachedSchema("fs", CachedSchema{
		CacheKey: SchemaCacheKey(spec),
		Tools: []CachedTool{
			{Name: "read_file", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "  ", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "broken", Schema: json.RawMessage(`[]`)},
			{Name: "scalar", Schema: json.RawMessage(`"not an object"`)},
			{Name: "write_file", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	cs, ok := LoadCachedSchema("fs", SchemaCacheKey(spec))
	if !ok {
		t.Fatal("the snapshot was rejected outright")
	}
	if len(cs.Tools) != 2 {
		t.Fatalf("tools = %+v, want the two usable ones", cs.Tools)
	}
	if cs.Tools[0].Name != "read_file" || cs.Tools[1].Name != "write_file" {
		t.Errorf("tools = %s, %s", cs.Tools[0].Name, cs.Tools[1].Name)
	}
}

// Saving twice is a replace, and the file must never be unreadable while it
// is being replaced — another process may be loading it at that moment.
func TestSaveReplacesThePreviousSnapshot(t *testing.T) {
	cacheHome(t)

	spec := Spec{Name: "fs", Type: TransportStdio, Command: "npx"}
	key := SchemaCacheKey(spec)
	if err := SaveCachedSchema("fs", CachedSchema{
		CacheKey: key,
		Tools:    []CachedTool{{Name: "read_file", Schema: json.RawMessage(`{"type":"object"}`)}},
	}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := SaveCachedSchema("fs", CachedSchema{
		CacheKey: key,
		Tools:    []CachedTool{{Name: "other_tool", Schema: json.RawMessage(`{"type":"object"}`)}},
	}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	cs, ok := LoadCachedSchema("fs", key)
	if !ok {
		t.Fatal("the snapshot was not loaded after two saves")
	}
	if len(cs.Tools) != 1 || cs.Tools[0].Name != "other_tool" {
		t.Fatalf("tools = %+v, want the second snapshot's", cs.Tools)
	}

	// No temp files were left behind by either write.
	entries, err := os.ReadDir(CacheDir())
	if err != nil {
		t.Fatalf("read the cache dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mcp-") {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
}

func TestSlugSanitizesNames(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"codegraph", "codegraph"},
		{"fs", "fs"},
	}
	for _, c := range cases {
		if got := slug(c.name); got != c.want {
			t.Errorf("slug(%q) = %q, want %q", c.name, got, c.want)
		}
	}

	// Names sanitisation changed get a hash suffix, so two servers whose
	// names differ only in stripped characters cannot share a cache file.
	other := []string{"My Server", "my/server", "con", "COM1", "nul", "a b", "a.b"}
	seen := map[string]bool{}
	for _, name := range other {
		got := slug(name)
		if got == name {
			t.Errorf("slug(%q) = %q, want a sanitized name", name, got)
		}
		if !strings.Contains(got, "-") && !strings.Contains(got, "_") {
			t.Errorf("slug(%q) = %q, want a hash suffix", name, got)
		}
		if seen[got] {
			t.Errorf("slug(%q) = %q, which collides with an earlier one", name, got)
		}
		seen[got] = true
	}
}

// Two servers whose names differ only in characters the filesystem strips must
// not share a cache file.
func TestSlugKeepsSimilarNamesApart(t *testing.T) {
	a := slug("read write")
	b := slug("read/writ.e")
	if a == b {
		t.Errorf("two different servers share the slug %q", a)
	}
}

func TestCachePathIsUnderTheConfigHome(t *testing.T) {
	home := cacheHome(t)

	want := filepath.Join(home, "cache", "mcp", "fs.json")
	if got := cachePath("fs"); got != want {
		t.Errorf("cachePath = %q, want %q", got, want)
	}
}

// A snapshot written by a build that used another layout is a miss, not a
// crash.
func TestLoadRejectsAnotherCacheVersion(t *testing.T) {
	home := cacheHome(t)

	spec := Spec{Name: "fs", Type: TransportStdio, Command: "npx"}
	key := SchemaCacheKey(spec)
	if err := SaveCachedSchema("fs", CachedSchema{
		CacheKey: key,
		Tools:    []CachedTool{{Name: "read_file"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Rewrite it as a future version: a downgrade must not read it.
	b, _ := json.Marshal(CachedSchema{
		Version:  cacheVersion + 1,
		CacheKey: key,
		Tools:    []CachedTool{{Name: "read_file"}},
	})
	if err := os.WriteFile(filepath.Join(home, "cache", "mcp", "fs.json"), b, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, ok := LoadCachedSchema("fs", key); ok {
		t.Error("a snapshot from another cache version was trusted")
	}
}

// The snapshot records when the tools were last confirmed by a real
// handshake, which is the first thing to look at when a tool list looks stale.
func TestSaveRecordsWhenItWasValidated(t *testing.T) {
	cacheHome(t)

	before := time.Now().UTC().Add(-time.Second)
	spec := Spec{Name: "fs", Type: TransportStdio, Command: "npx"}
	key := SchemaCacheKey(spec)
	if err := SaveCachedSchema("fs", CachedSchema{
		CacheKey: key,
		Tools:    []CachedTool{{Name: "read_file"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	cs, ok := LoadCachedSchema("fs", key)
	if !ok {
		t.Fatal("not loaded")
	}
	if cs.LastValidated.Before(before) {
		t.Errorf("lastValidated = %s, want about now", cs.LastValidated)
	}
}
