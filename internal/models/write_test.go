package models

// write_test.go — the write layer (custom-model-plan.md P1).
//
// What is under test: Save's contract — schema self-check before the disk
// (an invalid document is refused with the target untouched), the atomic
// replace (Load sees exactly what Save wrote), the struct round-trip's
// fidelity (plan D5: nothing the schema allows is lost), and the
// permission rule (0600 when a literal apiKey rides along).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ultrac/internal/ai"
)

// richConfig is one provider carrying every optional field the schema
// allows — the D5 round-trip witness. If a future field addition breaks
// the marshal/unmarshal symmetry, this test is where it shows.
func richConfig() Config {
	cw, mt := 128000, 8192
	reasoning := true
	return Config{
		Providers: map[string]ProviderConfig{
			"my-proxy": {
				Name:    "My Proxy",
				BaseURL: "https://proxy.example.com/v1",
				API:     "openai-completions",
				Headers: map[string]string{"X-Custom": "value"},
				Models: []ModelDefinition{
					{
						ID:               "my-model",
						Name:             "My Model",
						API:              "openai-completions",
						BaseURL:          "https://proxy.example.com/v1",
						Reasoning:        &reasoning,
						ThinkingLevelMap: ai.ThinkingLevelMap{ai.ModelThinkHigh: strptr("high"), ai.ModelThinkXHigh: nil},
						Input:            []ai.ModelInputModality{ai.ModalityText, ai.ModalityImage},
						InputLimits: &ai.ModelInputLimits{
							MaxRequestBytes: intptr(1000000),
							Images: &ai.ModelImageInputLimits{
								Resize:        &ai.ModelImageResizeOptions{MaxWidth: intptr(1024), JPEGQuality: intptr(85)},
								MaxPerMessage: intptr(5),
							},
						},
						Cost: &ModelCost{
							Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0.3,
							Tiers: []ai.ModelCostTier{{InputTokensAbove: 100000, Input: 0.5, Output: 1}},
						},
						PromptCache:                   ai.ModelPromptCache{"short": 300},
						ContextWindow:                 &cw,
						MaxTokens:                     &mt,
						SamplingParams:                ai.SamplingParams{"temperature": 0.5},
						SamplingParamsByThinkingLevel: ai.SamplingParamsByThinkingLevel{ai.ModelThinkHigh: {"temperature": 0.5}},
						Headers:                       map[string]string{"X-Model": "yes"},
					},
				},
				ModelOverrides: map[string]ModelOverride{
					"my-model": {MaxTokens: &mt},
				},
			},
		},
	}
}

func strptr(s string) *string { return &s }
func intptr(n int) *int       { return &n }

func TestSaveNewProviderRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"custom": {
				BaseURL: "https://api.example.com/v1",
				API:     "openai-completions",
				Models: []ModelDefinition{
					{ID: "example-chat", Name: "Example Chat", ContextWindow: intptr(64000), MaxTokens: intptr(4096)},
				},
			},
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	entry, ok := got.Providers["custom"]
	if !ok {
		t.Fatalf("providers = %v, want the saved entry", got.Providers)
	}
	if entry.BaseURL != "https://api.example.com/v1" || entry.API != "openai-completions" {
		t.Errorf("entry = %+v, want the saved baseUrl/api", entry)
	}
	if len(entry.Models) != 1 || entry.Models[0].ID != "example-chat" {
		t.Fatalf("models = %+v, want one example-chat entry", entry.Models)
	}
	m := entry.Models[0]
	if m.Name != "Example Chat" || m.ContextWindow == nil || *m.ContextWindow != 64000 || m.MaxTokens == nil || *m.MaxTokens != 4096 {
		t.Errorf("model = %+v, want the saved fields", m)
	}
	// The snapshot must accept what Save wrote — the file is only useful
	// if it composes.
	snap := Build(got)
	if _, ok := snap.Lookup("custom", "example-chat"); !ok {
		t.Error("the saved provider does not compose into a snapshot")
	}
}

// TestSaveOverwritesSameProvider — the add flow's second save for the
// same id: the entry replaces wholesale (merge.go's rule), the old
// models[] is gone, and nothing else in the file moves.
func TestSaveOverwritesSameProvider(t *testing.T) {
	path := write(t, `{"providers": {
		"keep": {"baseUrl": "https://keep.example.com", "api": "openai-completions"},
		"custom": {"baseUrl": "https://old.example.com", "api": "openai-completions",
			"models": [{"id": "old-model"}]}
	}}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cw, mt := 128000, 8192
	cfg.Providers["custom"] = ProviderConfig{
		BaseURL: "https://new.example.com",
		API:     "anthropic-messages",
		Models:  []ModelDefinition{{ID: "new-model", ContextWindow: &cw, MaxTokens: &mt}},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if entry := got.Providers["custom"]; entry.BaseURL != "https://new.example.com" ||
		entry.API != "anthropic-messages" || len(entry.Models) != 1 || entry.Models[0].ID != "new-model" {
		t.Errorf("custom = %+v, want the replacement entry wholesale", entry)
	}
	if entry, ok := got.Providers["keep"]; !ok || entry.BaseURL != "https://keep.example.com" {
		t.Errorf("keep = %+v, want the untouched neighbour", entry)
	}
}

// TestSaveRefusesInvalidDocument — the self-check: a Config that encodes
// a schema violation (a model with no id) is refused, the error names the
// violation, and the file on disk is byte-for-byte what it was.
func TestSaveRefusesInvalidDocument(t *testing.T) {
	original := `{"providers": {"keep": {"baseUrl": "https://keep.example.com"}}}`
	path := write(t, original)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Providers["broken"] = ProviderConfig{
		BaseURL: "https://broken.example.com",
		Models:  []ModelDefinition{{Name: "no id"}}, // schema: id is required
	}
	err = Save(path, cfg)
	if err == nil {
		t.Fatal("Save must refuse a document that fails the schema")
	}
	if !strings.Contains(err.Error(), "providers.broken.models.0.id") || !strings.Contains(err.Error(), "minLength") {
		t.Errorf("error = %v, want the schema's id violation naming the path", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) != original {
		t.Errorf("file = %q, want the original untouched", raw)
	}
	// No temp file may survive a refusal.
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".models-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}
}

// TestSaveEmptyProviders — the empty document: a config with no providers
// writes {"providers": {}} and reads back empty (a nil map normalises to
// the same thing rather than writing null, which the schema rejects).
func TestSaveEmptyProviders(t *testing.T) {
	for name, cfg := range map[string]Config{
		"nil map":   {},
		"empty map": {Providers: map[string]ProviderConfig{}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "models.json")
			if err := Save(path, cfg); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(got.Providers) != 0 {
				t.Errorf("providers = %v, want none", got.Providers)
			}
		})
	}
}

// TestSaveRichConfigRoundTrip — D5's promise, pinned: every field the
// schema allows survives marshal → Parse → unmarshal unchanged. The
// comparison is on the re-encoded document, so pointer fields
// (contextWindow, cost) are compared by value.
func TestSaveRichConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := Save(path, richConfig()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want, err := Parse(mustMarshal(t, richConfig()))
	if err != nil {
		t.Fatalf("Parse of the re-marshalled config: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(got.Providers))
	}
	gotEntry := got.Providers["my-proxy"]
	wantEntry := want.Providers["my-proxy"]
	if gotEntry.OAuth != nil {
		t.Errorf("oauth = %s, want absent", gotEntry.OAuth)
	}
	if gotEntry.Models[0].ID != wantEntry.Models[0].ID ||
		gotEntry.Models[0].ContextWindow == nil || *gotEntry.Models[0].ContextWindow != 128000 ||
		gotEntry.Models[0].Cost == nil || gotEntry.Models[0].Cost.Input != 1 ||
		len(gotEntry.Models[0].Cost.Tiers) != 1 ||
		len(gotEntry.Models[0].ThinkingLevelMap) != 2 ||
		len(gotEntry.Models[0].Input) != 2 ||
		gotEntry.Models[0].InputLimits == nil || gotEntry.Models[0].InputLimits.Images == nil ||
		gotEntry.Models[0].InputLimits.Images.Resize == nil ||
		*gotEntry.Models[0].InputLimits.Images.Resize.MaxWidth != 1024 ||
		len(gotEntry.ModelOverrides) != 1 {
		t.Errorf("round-trip lost fields:\n got %+v\nwant %+v", gotEntry, wantEntry)
	}
}

// TestSaveCreatesNestedDirectory — the user layer may not exist yet on a
// fresh machine (no ~/.ultrac at all): MkdirAll brings it into being.
func TestSaveCreatesNestedDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ultrac", "models.json")
	cfg := Config{Providers: map[string]ProviderConfig{
		"custom": {BaseURL: "https://api.example.com", API: "openai-completions"},
	}}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestSavePermissionFollowsAPIKey — 0600 when the file carries a literal
// key, 0644 when it does not (plan P1). Windows has no mode bits beyond
// read-only, so the assertion is POSIX-only; the write itself is
// platform-independent.
func TestSavePermissionFollowsAPIKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX mode bits; the write is covered everywhere else")
	}
	withKey := filepath.Join(t.TempDir(), "models.json")
	if err := Save(withKey, Config{Providers: map[string]ProviderConfig{
		"custom": {BaseURL: "https://api.example.com", APIKey: "sk-literal"},
	}}); err != nil {
		t.Fatalf("Save with key: %v", err)
	}
	if mode := fileMode(t, withKey); mode != 0o600 {
		t.Errorf("mode = %o, want 600 (the file holds a credential)", mode)
	}
	withoutKey := filepath.Join(t.TempDir(), "models.json")
	if err := Save(withoutKey, Config{Providers: map[string]ProviderConfig{
		"custom": {BaseURL: "https://api.example.com"},
	}}); err != nil {
		t.Fatalf("Save without key: %v", err)
	}
	if mode := fileMode(t, withoutKey); mode != 0o644 {
		t.Errorf("mode = %o, want 644 (no credential in the file)", mode)
	}
}

// TestSaveRefusesEmptyPath — the guard the callers rely on: an empty
// path is a programming error, not "write to the current directory".
func TestSaveRefusesEmptyPath(t *testing.T) {
	if err := Save("  ", Config{}); err == nil {
		t.Fatal("Save must refuse an empty path")
	}
}

// fileMode is the POSIX permission bits of path (test-only; the mode
// assertions are POSIX-only for the same reason).
func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return info.Mode().Perm()
}

func mustMarshal(t *testing.T, cfg Config) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
