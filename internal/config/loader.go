package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// FileResolver is the default Resolver. It reads the real files under
// ConfigHome() and the working directory.
//
// Every layer is cached, because the engine asks for settings many times per
// turn (each tool call re-reads them). The cache is invalidated on Update and
// by hand through Invalidate — the same shape as the TypeScript session cache,
// and for the same reason: a settings file changed on disk behind the process's
// back, and nothing else notices.
type FileResolver struct {
	mu    sync.RWMutex
	cache map[string]*cachedLayer

	// FlagPath is the file named by --settings. Empty means no flag layer.
	FlagPath string
	// FlagInline is the inline JSON given to --settings. It merges on top of
	// FlagPath, matching the TypeScript behaviour.
	FlagInline json.RawMessage
	// Now and ReadFile are injection points for tests.
	ReadFile func(string) ([]byte, error)
}

type cachedLayer struct {
	settings Settings
	err      error
}

// NewFileResolver returns a Resolver over the real filesystem.
func NewFileResolver() *FileResolver {
	return &FileResolver{
		cache:    make(map[string]*cachedLayer),
		ReadFile: os.ReadFile,
	}
}

func (r *FileResolver) cacheKey(src Source, cwd string) string {
	return string(rune(src)) + "|" + cwd
}

// Get resolves the whole stack for cwd.
//
// A non-nil Settings always comes back, even alongside an error. Layers reports
// broken files as warnings precisely so one typo cannot take the other four
// layers down with it, and that promise is worthless if the caller has to
// choose between the merged view and the diagnosis.
func (r *FileResolver) Get(ctx context.Context, cwd string) (*Settings, error) {
	layers, err := r.Layers(ctx, cwd)

	merged := Settings{}
	for _, l := range layers {
		merged = MergeSettings(merged, l.Settings)
	}
	if verr := validate(&merged); verr != nil {
		return &merged, verr
	}
	return &merged, err
}

// Layers loads each enabled source separately, in merge order.
func (r *FileResolver) Layers(ctx context.Context, cwd string) ([]Layer, error) {
	out := make([]Layer, 0, 5)
	var firstErr error

	for _, src := range EnabledSources() {
		if err := ctx.Err(); err != nil {
			return out, err
		}

		layer, err := r.load(ctx, src, cwd)
		if err != nil {
			// A broken file is a warning, not a stop. The other four layers
			// are still perfectly usable, and refusing to start because one
			// settings file has a typo is how a user ends up debugging their
			// JSON instead of their agent.
			if firstErr == nil {
				firstErr = err
			}
			layer = Layer{Source: src, Path: SourceFile(src, cwd)}
		}
		out = append(out, layer)
	}
	return out, firstErr
}

func (r *FileResolver) load(ctx context.Context, src Source, cwd string) (Layer, error) {
	key := r.cacheKey(src, cwd)

	r.mu.RLock()
	if hit, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return Layer{Source: src, Path: SourceFile(src, cwd), Settings: hit.settings}, hit.err
	}
	r.mu.RUnlock()

	layer := Layer{Source: src, Path: SourceFile(src, cwd)}
	var err error
	layer.Settings, err = r.read(ctx, src, cwd)

	r.mu.Lock()
	r.cache[key] = &cachedLayer{settings: layer.Settings, err: err}
	r.mu.Unlock()
	return layer, err
}

func (r *FileResolver) read(ctx context.Context, src Source, cwd string) (Settings, error) {
	switch src {
	case SourceFlag:
		return r.readFlag()
	case SourcePolicy:
		return ReadManagedSettings(r.ReadFile)
	}

	path := SourceFile(src, cwd)
	if path == "" {
		return Settings{}, nil
	}
	return r.readFile(path)
}

func (r *FileResolver) readFlag() (Settings, error) {
	var out Settings
	if r.FlagPath != "" {
		s, err := r.readFile(r.FlagPath)
		if err != nil {
			return out, err
		}
		out = s
	}
	if len(r.FlagInline) > 0 {
		var inline Settings
		if err := json.Unmarshal(r.FlagInline, &inline); err != nil {
			return out, &Error{Path: "--settings", Op: "parse inline settings", Err: err}
		}
		out = MergeSettings(out, inline)
	}
	return out, nil
}

func (r *FileResolver) readFile(path string) (Settings, error) {
	data, err := r.ReadFile(path)
	if err != nil {
		// An absent file is not an error. Every source is optional.
		if errors.Is(err, fs.ErrNotExist) {
			return Settings{}, nil
		}
		if errors.Is(err, fs.ErrPermission) {
			return Settings{}, &Error{Path: path, Op: "read", Err: err, Warning: true}
		}
		return Settings{}, &Error{Path: path, Op: "read", Err: err, Warning: true}
	}

	// An empty file is an empty object, not a parse error. Editors truncate
	// on save; a zero-byte settings.json should not take the session down.
	if len(strings.TrimSpace(string(data))) == 0 {
		return Settings{}, nil
	}

	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, &Error{Path: path, Op: "parse", Err: err, Warning: true}
	}
	return s, nil
}

// ReadManagedSettings resolves the policy layer.
//
// Policy is "first source wins", not "merge everything": remote settings, then
// MDM (HKLM / plist), then managed-settings.json plus its drop-ins, then HKCU.
// The ordering exists because these are administered by different teams with
// different deployment stories, and an org that pushed MDM settings does not
// want a stale file from a build pipeline quietly blending in.
//
// Remote and HKCU are not implemented here. Remote needs the network and the
// sync cache (T2); HKCU is Windows registry policy. Both slots are marked
// rather than silently absent, because a reader comparing behaviour with the
// TypeScript build will otherwise conclude the precedence chain is shorter
// than it is.
func ReadManagedSettings(readFile func(string) ([]byte, error)) (Settings, error) {
	merged := Settings{}
	found := false

	if readFile == nil {
		readFile = os.ReadFile
	}

	// 1. Base file.
	base := ManagedSettingsFile()
	if s, err := readOne(readFile, base); err != nil {
		return merged, err
	} else if !isEmpty(s) {
		merged = MergeSettings(merged, s)
		found = true
	}

	// 2. Drop-ins, lexical order. Later files win, which is what makes
	// "10-otel.json, 20-security.json" behave the way an admin expects.
	entries, err := os.ReadDir(ManagedDropInDir())
	if err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
				continue
			}
			if e.IsDir() {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			path := filepath.Join(ManagedDropInDir(), name)
			s, err := readOne(readFile, path)
			if err != nil {
				return merged, err
			}
			if !isEmpty(s) {
				merged = MergeSettings(merged, s)
				found = true
			}
		}
	} else if !isMissingDir(err) {
		return merged, &Error{Path: ManagedDropInDir(), Op: "read drop-ins", Err: err, Warning: true}
	}

	// 3. HKCU — Windows user-writable policy. Not implemented; see above.
	if !found {
		return Settings{}, nil
	}
	return merged, nil
}

func readOne(readFile func(string) ([]byte, error), path string) (Settings, error) {
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Settings{}, nil
		}
		return Settings{}, &Error{Path: path, Op: "read", Err: err, Warning: true}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Settings{}, nil
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, &Error{Path: path, Op: "parse", Err: err, Warning: true}
	}
	return s, nil
}

// isMissingDir reports whether err means "the directory is not there, or is not
// a directory". ENOTDIR shows up when a *file* sits where managed-settings.d
// should be, which is a misconfiguration rather than an absence — but either
// way there are no drop-ins to read, and neither is worth a warning.
func isMissingDir(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func isEmpty(s Settings) bool {
	return len(s.Env) == 0 && len(s.Extra) == 0
}

// MergeSettings deep-merges over into base, later winning.
//
// Array values concatenate and deduplicate rather than replacing. That is the
// TypeScript behaviour (settingsMergeCustomizer) and it is what makes
// permissions.add work: a user's allow list should extend the policy's, not
// overwrite it. Everything else is a last-writer-wins deep merge.
func MergeSettings(base, over Settings) Settings {
	out := Settings{Env: mergeStringMaps(base.Env, over.Env)}

	keys := make(map[string]struct{}, len(base.Extra)+len(over.Extra))
	for k := range base.Extra {
		keys[k] = struct{}{}
	}
	for k := range over.Extra {
		keys[k] = struct{}{}
	}
	out.Extra = make(map[string]json.RawMessage, len(keys))
	for k := range keys {
		bv, inBase := base.Extra[k]
		ov, inOver := over.Extra[k]
		switch {
		case inBase && inOver:
			out.Extra[k] = mergeJSON(bv, ov)
		case inOver:
			out.Extra[k] = ov
		default:
			out.Extra[k] = bv
		}
	}
	if len(out.Extra) == 0 {
		out.Extra = nil
	}
	return out
}

func mergeStringMaps(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// mergeJSON merges two raw JSON values with array-concat semantics.
func mergeJSON(a, b json.RawMessage) json.RawMessage {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return b
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return b
	}
	merged := mergeValue(av, bv)
	out, err := json.Marshal(merged)
	if err != nil {
		return b
	}
	return out
}

func mergeValue(a, b any) any {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		out := make(map[string]any, len(am)+len(bm))
		for k, v := range am {
			out[k] = v
		}
		for k, v := range bm {
			if existing, ok := out[k]; ok {
				out[k] = mergeValue(existing, v)
				continue
			}
			out[k] = v
		}
		return out
	}

	aa, aIsArr := a.([]any)
	ba, bIsArr := b.([]any)
	if aIsArr && bIsArr {
		seen := make(map[string]struct{}, len(aa)+len(ba))
		out := make([]any, 0, len(aa)+len(ba))
		for _, v := range append(append([]any{}, aa...), ba...) {
			enc, err := json.Marshal(v)
			if err != nil {
				continue
			}
			if _, dup := seen[string(enc)]; dup {
				continue
			}
			seen[string(enc)] = struct{}{}
			out = append(out, v)
		}
		return out
	}
	return b
}

// validate rejects settings that are dangerous regardless of who wrote them.
func validate(s *Settings) error {
	for key := range s.Extra {
		if IsDangerousShellSetting(key) {
			return fmt.Errorf("config: setting %q executes arbitrary shell commands and is not accepted", key)
		}
	}
	return nil
}

// Update applies mutate to one source and persists the result.
//
// mutate receives the file's current contents, not the merged view. That is the
// only correct choice: writing a merged snapshot into ~/.ultrac/settings.json
// would freeze whatever a project file or a policy happened to say at the
// moment of the write.
//
// An empty string in the mutated Env deletes that key rather than setting it to
// "". This is the semantic /provider depends on: blanking a field in the wizard
// means "remove this", and writing "" would leave a variable that shadows the
// built-in default with nothing.
func (r *FileResolver) Update(ctx context.Context, src Source, cwd string, mutate func(*Settings) error) error {
	if !Editable(src) {
		return fmt.Errorf("config: %s settings are read-only", LongDisplayName(src))
	}
	if src == SourceDefault {
		return errors.New("config: built-in defaults cannot be written")
	}

	path := SourceFile(src, cwd)
	if path == "" {
		return fmt.Errorf("config: no file backs %s settings", src)
	}

	current, err := r.read(ctx, src, cwd)
	if err != nil {
		// A file that does not parse must not be overwritten. Merging into an
		// empty object here would destroy whatever the user was trying to fix.
		var cfgErr *Error
		if errors.As(err, &cfgErr) && strings.Contains(cfgErr.Op, "parse") {
			return fmt.Errorf("config: refusing to write %s: %w", path, err)
		}
	}

	if err := mutate(&current); err != nil {
		return err
	}
	dropEmptyEnv(&current)

	if err := r.write(path, current); err != nil {
		return err
	}

	r.Invalidate(src)
	r.Invalidate(SourceFlag)
	return nil
}

// dropEmptyEnv removes keys whose value is empty, honouring the "blank means
// delete" contract.
func dropEmptyEnv(s *Settings) {
	for k, v := range s.Env {
		if v == "" {
			delete(s.Env, k)
		}
	}
	if len(s.Env) == 0 {
		s.Env = nil
	}
}

// write persists settings atomically.
//
// Write-to-temp, rename-into-place. A settings file is read by every process
// on the machine, so a crash mid-write must not leave a half-written document
// that fails to parse on the next launch.
func (r *FileResolver) write(path string, s Settings) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &Error{Path: path, Op: "create directory", Err: err}
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return &Error{Path: path, Op: "encode", Err: err}
	}
	// The TypeScript writer terminates the file with a newline. Matching it
	// keeps diffs between the two implementations clean.
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return &Error{Path: path, Op: "create temp file", Err: err}
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort: if the rename succeeded this finds nothing to remove.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return &Error{Path: path, Op: "write", Err: err}
	}
	if err := tmp.Close(); err != nil {
		return &Error{Path: path, Op: "close", Err: err}
	}

	// Windows rename fails onto an existing file, and it refuses when another
	// process holds the target open. Remove first — the temp file is already
	// fully written, so the window where the path does not exist is one
	// syscall wide.
	if err := os.Rename(tmpName, path); err != nil {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return &Error{Path: path, Op: "replace", Err: err}
		}
		if err := os.Rename(tmpName, path); err != nil {
			return &Error{Path: path, Op: "replace", Err: err}
		}
	}
	return nil
}

// Invalidate drops one source's cached layer.
func (r *FileResolver) Invalidate(src Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.cache {
		if k[0] == byte(src) {
			delete(r.cache, k)
		}
	}
}

// InvalidateAll drops every cached layer.
func (r *FileResolver) InvalidateAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.cache)
}
