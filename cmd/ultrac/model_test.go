package main

// Tests for --model + provider-catalog resolution
// (settings-unify-plan.md D11: the settings env block is not a source).
//
// What changed and is now pinned by these tests:
//
//	ANTHROPIC_MODEL          no longer supplies a model id
//	ANTHROPIC_BASE_URL       no longer supplies or overrides an endpoint
//	ANTHROPIC_* dialect      no longer picks the dialect
//	provider inference       no longer guesses from a base URL's host
//
// A model therefore comes from --model (or a resumed session), and everything
// else comes from the provider entry. The tests assert the interesting
// corners: a bare id the catalog knows, a bare id it does not, --api forcing a
// dialect, and a provider whose entry failed to compose.

import (
	"errors"
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/models"
)

// TestResolveModelNoFlagIsErrNoModel — "nothing named a model" is a state, not
// a fallback to search for. A provider-only flag ("deepseek/") is the same
// state: there is no settings model id to complete it any more.
func TestResolveModelNoFlagIsErrNoModel(t *testing.T) {
	cases := []string{"", "deepseek/", "   "}
	for _, flag := range cases {
		_, err := resolveModel(nil, flag, "")
		if !errors.Is(err, errNoModelConfigured) {
			t.Errorf("resolveModel(%q) err = %v, want errNoModelConfigured", flag, err)
		}
	}
}

// TestResolveModelBareFlagUsesCatalogEntry — a bare id the catalog knows
// resolves completely from that entry: provider, dialect, base URL and rates.
func TestResolveModelBareFlagUsesCatalogEntry(t *testing.T) {
	m, err := resolveModel(nil, "deepseek-chat", "")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.Provider != ai.ProviderDeepseek {
		t.Errorf("provider = %q, want deepseek (named by the catalog entry)", m.Provider)
	}
	if m.API != ai.APIOpenAICompletions {
		t.Errorf("api = %q, want the catalog entry's dialect", m.API)
	}
	if m.BaseURL != "https://api.deepseek.com" {
		t.Errorf("baseUrl = %q, want the openai-dialect entry URL", m.BaseURL)
	}
	if m.Cost.Input != 0.27 {
		t.Errorf("cost.input = %v, want 0.27 from the catalog entry", m.Cost.Input)
	}
}

// TestResolveModelUnknownIDNeedsProvider — an id no entry declares cannot
// resolve: without a catalog entry there is no provider to attribute it to,
// and guessing one (by dialect, or by a base URL's host) is how a request
// reaches an endpoint nobody chose.
func TestResolveModelUnknownIDNeedsProvider(t *testing.T) {
	_, err := resolveModel(nil, "some-model-nobody-declares", "")
	if err == nil {
		t.Fatal("an unknown bare id must be refused")
	}
	if !strings.Contains(err.Error(), "provider/id") {
		t.Errorf("error = %v, want it to ask for provider/id", err)
	}
	// With the provider named, the same id resolves: the declaration is the
	// authority, not the catalog.
	snap := models.Build(models.Config{Providers: map[string]models.ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.internal.example.com/v1",
			API:     "openai-completions",
			APIKey:  "sk-custom",
			Models:  []models.ModelDefinition{{ID: "some-model-nobody-declares"}},
		},
	}})
	m, err := resolveModel(snap, "my-proxy/some-model-nobody-declares", "")
	if err != nil {
		t.Fatalf("resolveModel with a declared provider: %v", err)
	}
	if m.BaseURL != "https://proxy.internal.example.com/v1" {
		t.Errorf("baseUrl = %q, want the declared one", m.BaseURL)
	}
}

// TestResolveModelUsesDeclaredEntry — a declared provider's own dialect, base
// URL and rates are adopted wholesale.
func TestResolveModelUsesDeclaredEntry(t *testing.T) {
	snap := models.Build(models.Config{Providers: map[string]models.ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.internal.example.com/v1",
			API:     "openai-completions",
			Models: []models.ModelDefinition{{
				ID:   "my-model",
				Name: "My Model",
				Cost: &models.ModelCost{Input: 1.5, Output: 3.0, CacheRead: 0.15, CacheWrite: 1.5},
			}},
		},
	}})
	m, err := resolveModel(snap, "my-proxy/my-model", "")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.Provider != ai.ProviderId("my-proxy") {
		t.Errorf("provider = %q", m.Provider)
	}
	if m.API != ai.APIOpenAICompletions {
		t.Errorf("api = %q, want the entry's dialect", m.API)
	}
	if m.BaseURL != "https://proxy.internal.example.com/v1" {
		t.Errorf("baseUrl = %q, want the entry's", m.BaseURL)
	}
	if m.Name != "My Model" {
		t.Errorf("name = %q", m.Name)
	}
	if m.Cost.Input != 1.5 || m.Cost.Output != 3.0 {
		t.Errorf("cost = %+v, want the declared rates", m.Cost)
	}
}

// TestResolveModelAPIOverride — --api forces the dialect, and because the
// entry's base URL speaks the other protocol it is dropped so WithDefaults
// fills in the one that matches.
func TestResolveModelAPIOverride(t *testing.T) {
	openaiEntry := models.Build(models.Config{Providers: map[string]models.ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.internal.example.com/v1",
			API:     "openai-completions",
			Models:  []models.ModelDefinition{{ID: "m"}},
		},
	}})
	m, err := resolveModel(openaiEntry, "my-proxy/m", string(ai.APIAnthropicMessages))
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.API != ai.APIAnthropicMessages {
		t.Errorf("api = %q, want the --api override", m.API)
	}
	if m.BaseURL == "https://proxy.internal.example.com/v1" {
		t.Error("the openai-dialect baseUrl survived an anthropic override: it would 404")
	}

	// The same override on a built-in entry lands on that provider's
	// anthropic URL.
	m2, err := resolveModel(nil, "deepseek/deepseek-chat", string(ai.APIAnthropicMessages))
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m2.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("baseUrl = %q, want the anthropic-dialect entry URL", m2.BaseURL)
	}
}

// TestResolveModelReportsBrokenProvider — an empty provider entry fails to
// compose (merge.go's pi rule). The snapshot keeps serving every other
// provider, but selecting the broken one is a hard error rather than a request
// that fails at the endpoint with a confusing message.
func TestResolveModelReportsBrokenProvider(t *testing.T) {
	snap := models.Build(models.Config{Providers: map[string]models.ProviderConfig{
		"broken": {},
	}})
	_, err := resolveModel(snap, "broken/whatever", "")
	if err == nil {
		t.Fatal("selecting a provider whose entry failed to compose must error")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error = %v, want it to name the broken provider", err)
	}
	// An unrelated provider is unaffected.
	if _, err := resolveModel(snap, "deepseek/deepseek-chat", ""); err != nil {
		t.Errorf("an unrelated provider must still resolve: %v", err)
	}
}

// TestResolveModelNilSnapshotMeansBuiltin — a nil snapshot means "nothing
// loaded": the built-in catalog alone, exactly models.Build() with no configs.
func TestResolveModelNilSnapshotMeansBuiltin(t *testing.T) {
	fromNil, err := resolveModel(nil, "deepseek/deepseek-chat", "")
	if err != nil {
		t.Fatalf("resolveModel(nil): %v", err)
	}
	fromBuiltin, err := resolveModel(models.Build(), "deepseek/deepseek-chat", "")
	if err != nil {
		t.Fatalf("resolveModel(Build()): %v", err)
	}
	if fromNil.ID != fromBuiltin.ID || fromNil.Provider != fromBuiltin.Provider ||
		fromNil.API != fromBuiltin.API || fromNil.BaseURL != fromBuiltin.BaseURL ||
		fromNil.Cost.Input != fromBuiltin.Cost.Input || fromNil.ContextWindow != fromBuiltin.ContextWindow {
		t.Errorf("nil snapshot = %+v, want the built-in view %+v", fromNil, fromBuiltin)
	}
}

func TestResolveModelColonSuffixStripped(t *testing.T) {
	// pi's "provider/model:thinking" syntax: this port has no thinking flag,
	// so the suffix must not leak into the model id.
	m, err := resolveModel(nil, "deepseek/deepseek-chat:high", "")
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.ID != "deepseek-chat" {
		t.Errorf("id = %q, want deepseek-chat (suffix stripped)", m.ID)
	}
}

func TestSplitModelFlag(t *testing.T) {
	cases := []struct {
		flag         string
		wantProvider string
		wantID       string
	}{
		{"", "", ""},
		{"deepseek-chat", "", "deepseek-chat"},
		{"deepseek/deepseek-chat", "deepseek", "deepseek-chat"},
		{"deepseek/", "deepseek", ""},
		{"anthropic/claude-sonnet-4-5", "anthropic", "claude-sonnet-4-5"},
	}
	for _, tc := range cases {
		p, id := splitModelFlag(tc.flag)
		if p != tc.wantProvider || id != tc.wantID {
			t.Errorf("splitModelFlag(%q) = (%q, %q), want (%q, %q)",
				tc.flag, p, id, tc.wantProvider, tc.wantID)
		}
	}
}

// TestResolveCredential — one step, no fallback (D11): the provider's own
// apiKey, and nothing borrowed from the settings env block.
func TestResolveCredential(t *testing.T) {
	snap := models.Build(models.Config{Providers: map[string]models.ProviderConfig{
		"declared": {
			BaseURL: "https://a.example.com/v1",
			API:     "openai-completions",
			APIKey:  "sk-declared",
			Models: []models.ModelDefinition{{
				ID:   "m1",
				Cost: &models.ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1},
			}},
		},
		"declared-no-key": {
			BaseURL: "https://b.example.com/v1",
			API:     "openai-completions",
			Models: []models.ModelDefinition{{
				ID:   "m2",
				Cost: &models.ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1},
			}},
		},
	}})

	cases := []struct {
		provider string
		want     string
	}{
		{"declared", "sk-declared"},
		// Declared without a key: reports nothing rather than borrowing the
		// env block's token or another provider's.
		{"declared-no-key", ""},
		// A built-in provider with nothing stored.
		{"deepseek", ""},
		// An unknown provider: no credential, not an error.
		{"nobody", ""},
	}
	for _, tc := range cases {
		if got := resolveCredential(snap, tc.provider); got != tc.want {
			t.Errorf("resolveCredential(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}

	// A nil snapshot means "no settings loaded", the normal case: it resolves
	// to nothing rather than panicking.
	if got := resolveCredential(nil, "declared"); got != "" {
		t.Errorf("resolveCredential(nil) = %q, want empty", got)
	}
}
