package mcp

// cache.go — the persisted handshake snapshot
// (docs/开发计划/新功能计划/mcp-plan.md §1.5, D4).
//
// # What it buys
//
// A cold stdio handshake costs hundreds of milliseconds to seconds per server
// — spawning a subprocess, and for anything reached through `npx -y`,
// resolving and possibly downloading a package. At boot that cost is paid
// while the user is waiting to type. So the tool list a handshake produced is
// written to disk, and the next launch registers those tools from the file
// instead of from the server.
//
// # Why the cache is the design and not an optimisation
//
// The agent's tool set is built once and cannot be replaced (see the package
// doc), and the provider's tools array is part of the prompt-prefix the
// provider caches. Registering a server's tools twice — once from the cache,
// once from the server — would invalidate that prefix and cost a 10x miss on
// the next request. So the snapshot is not merely faster, it is what makes a
// stable tool list possible at all.
//
// # What the key hashes, and what it does not
//
// The key is built from the load-bearing, non-secret fields of a Spec. Secret
// VALUES are excluded on purpose: rotating a credential must not force a
// re-handshake, and a digest computed over a token would put a hash of it on
// disk. Their key NAMES are included, because adding or removing a variable
// the server reads does change what it will do.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"ultrac/internal/config"
)

// cacheVersion bumps whenever CachedSchema's shape changes incompatibly. A
// file written by a build that used another layout is a miss rather than a
// crash.
const cacheVersion = 1

// CachedSchema is the persisted snapshot of one server's handshake.
type CachedSchema struct {
	Version int `json:"version"`
	// CacheKey gates reuse: the tools are only trusted when they match the
	// key computed from the Spec being loaded right now.
	CacheKey string `json:"specHash"`
	// Instructions is the server's own description of what it is for, kept
	// so a configured-but-unspawned server can still say.
	Instructions string       `json:"instructions,omitempty"`
	Tools        []CachedTool `json:"tools"`
	// LastValidated is when a real handshake last produced this. It is
	// diagnostic: how stale the file is, when the tool list looks wrong.
	LastValidated time.Time `json:"lastValidated"`
}

// CachedTool is the subset of a tool definition needed to register a
// placeholder before the server has been contacted.
type CachedTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Schema is omitted when a tool declares none, so an absent schema and
	// a null one are the same thing on disk. A tool with no schema is
	// legitimate (its arguments are passed through), and a null in this slot
	// would otherwise load as the four bytes "null" — a non-empty schema
	// that is not an object.
	Schema json.RawMessage `json:"schema,omitempty"`
}

// CacheDir is where the snapshots live: <ConfigHome>/cache/mcp.
//
// Not in the project directory — a cache file committed to git is noise that
// shows up in every diff — and not beside settings.json, because a schema
// snapshot is derived data, not configuration (plan D4).
func CacheDir() string {
	home := config.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "cache", "mcp")
}

// SchemaCacheKey hashes the load-bearing, non-secret parts of a Spec.
//
// Every field is written with explicit separators so no two field boundaries
// can be confused by concatenation ("command"+"foo" versus "comm"+"andfoo").
func SchemaCacheKey(s Spec) string {
	h := sha256.New()
	writeField(h, "name", s.Name)
	writeField(h, "type", s.Type)
	writeField(h, "command", s.Command)
	for _, a := range s.Args {
		writeField(h, "arg", a)
	}
	for _, name := range s.DisabledTools {
		writeField(h, "disabled_tool", name)
	}
	// Key names only, sorted. Values are excluded (see the file comment) and
	// sorting keeps the digest independent of Go's randomised map order.
	writeKeys(h, "env", s.Env)
	return hex.EncodeToString(h.Sum(nil))
}

func writeField(h io.Writer, key, val string) {
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(val))
	_, _ = h.Write([]byte{1})
}

func writeKeys(h io.Writer, key string, m map[string]string) {
	if len(m) == 0 {
		writeField(h, key, "")
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		writeField(h, key+"."+k, "present")
	}
}

// ---------------------------------------------------------------------------
// Read / write
// ---------------------------------------------------------------------------

// LoadCachedSchema returns the snapshot for name if it exists, parses, is a
// version this build understands, was produced by the same configuration, and
// still carries at least one usable tool.
//
// Any of those failing is a miss. It is best-effort by design: a corrupt or
// stale file means one re-handshake, and returning an error would only invite
// callers to log it on every launch.
//
// An empty tool list is treated as a miss on purpose. A snapshot that has
// lost its tools would otherwise be trusted and register a server with none —
// which presents to the user as "my server does nothing" with no cause. A
// genuinely tool-less server pays a boot handshake instead, which is the
// cheaper mistake.
func LoadCachedSchema(name, wantKey string) (*CachedSchema, bool) {
	cs, ok := loadCachedSchemaAny(name)
	if !ok {
		return nil, false
	}
	if cs.CacheKey != wantKey {
		return nil, false
	}
	cs.Tools = usableCachedTools(cs.Tools)
	if len(cs.Tools) == 0 {
		return nil, false
	}
	return cs, true
}

func loadCachedSchemaAny(name string) (*CachedSchema, bool) {
	p := cachePath(name)
	if p == "" {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var out CachedSchema
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false
	}
	if out.Version != cacheVersion {
		return nil, false
	}
	return &out, true
}

// usableCachedTools drops entries a tool cannot be built from. One bad entry
// must not cost the others.
//
// A schema is required to be a JSON object when it is present: a tool's
// inputSchema is an object by definition, and something else in that slot
// would fail at argument-validation time on every call — better to notice it
// here, where losing it costs one tool instead of a tool that never works.
func usableCachedTools(tools []CachedTool) []CachedTool {
	out := make([]CachedTool, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		if len(t.Schema) > 0 {
			if !json.Valid(t.Schema) {
				continue
			}
			trimmed := bytes.TrimSpace(t.Schema)
			// A null written by an older build is read as absent, not as a
			// broken schema.
			if string(trimmed) == "null" {
				t.Schema = nil
			} else if len(trimmed) == 0 || trimmed[0] != '{' {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// SaveCachedSchema writes a snapshot for name. It is best-effort: a cache
// that cannot be written costs a re-handshake, not a boot.
func SaveCachedSchema(name string, cs CachedSchema) error {
	p := cachePath(name)
	if p == "" {
		return nil
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	cs.Version = cacheVersion
	if cs.LastValidated.IsZero() {
		cs.LastValidated = time.Now().UTC()
	}
	b, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".mcp-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort: if the rename succeeded there is nothing to remove.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// The rename is the whole atomic-replacement story: on Windows it is
	// MoveFileEx(MOVEFILE_REPLACE_EXISTING), which overwrites the target in
	// one step.
	//
	// Do NOT "fix" a failure here with remove-then-rename. Removing a file
	// someone else holds open fails with a sharing violation on Windows,
	// and a successful removal opens a window in which a concurrent reader
	// sees the path gone — the reader that was trying to load this very
	// cache. A failed rename is a cache miss, and that is a fine outcome.
	return os.Rename(tmpName, p)
}

// cachePath is <CacheDir>/<slug(name)>.json.
func cachePath(name string) string {
	base := CacheDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, slug(name)+".json")
}

var slugReplace = regexp.MustCompile(`[^a-z0-9_-]+`)

// windowsReserved are DOS device names Windows reserves as file stems, with or
// without an extension. A server called "con" would otherwise name the
// console, not a file.
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true,
	"com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true,
	"lpt9": true,
}

// slug sanitises a server name for use as a filename.
//
// A name that sanitisation changed gets a hash suffix, so two servers whose
// names differ only in characters the filesystem strips cannot share a cache
// file. Ordinary safe names stay byte-identical.
func slug(name string) string {
	s := strings.Trim(slugReplace.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "_"
	}
	if s != name || windowsReserved[s] {
		sum := sha256.Sum256([]byte(name))
		s += "-" + hex.EncodeToString(sum[:6])
	}
	return s
}
