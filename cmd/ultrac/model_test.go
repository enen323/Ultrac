package main

// Tests for the settings→Model resolution (P5).
//
// The interesting cases are the ambiguous ones: a provider that exposes the
// same model under two dialects, a base URL whose host names no catalog
// provider, and a flag that carries only half the identity.

import (
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/config/auth"
	"ultrac/internal/models"
)

func envOf(kv map[string]string) map[string]string { return kv }

func TestResolveModelFromSettings(t *testing.T) {
	// The shape ~/.ultrac/settings.json actually has today: DeepSeek behind
	// its Anthropic-compatible endpoint.
	env := envOf(map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.deepseek.com/anthropic",
		"ANTHROPIC_AUTH_TOKEN": "sk-test",
		"ANTHROPIC_MODEL":      "deepseek-flash",
	})
	m, err := resolveModel(nil, "", "", env)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.ID != "deepseek-flash" {
		t.Errorf("id = %q, want deepseek-flash", m.ID)
	}
	if m.API != ai.APIAnthropicMessages {
		t.Errorf("api = %q, want anthropic-messages (the ANTHROPIC_* convention)", m.API)
	}
	if m.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("baseUrl = %q, want the settings URL", m.BaseURL)
	}
	if m.Provider != ai.ProviderDeepseek {
		t.Errorf("provider = %q, want deepseek (host inference)", m.Provider)
	}
	if m.Name == "" {
		t.Error("WithDefaults should have filled the display name")
	}
}

func TestResolveModelFlagBeatsSettings(t *testing.T) {
	env := envOf(map[string]string{
		"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
		"ANTHROPIC_MODEL":    "deepseek-flash",
	})
	m, err := resolveModel(nil, "deepseek/deepseek-chat", "", env)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.ID != "deepseek-chat" {
		t.Errorf("id = %q, want deepseek-chat (flag wins)", m.ID)
	}
	if m.Provider != ai.ProviderDeepseek {
		t.Errorf("provider = %q, want deepseek", m.Provider)
	}
	// The catalog entry's rates must survive even though the dialect was
	// forced: cost accounting is the only thing rates feed.
	if m.Cost.Input != 0.27 {
		t.Errorf("cost.input = %v, want 0.27 from the catalog entry", m.Cost.Input)
	}
	// The settings base URL still wins over the entry's.
	if m.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("baseUrl = %q", m.BaseURL)
	}
}

func TestResolveModelBareFlagUsesCatalogDialect(t *testing.T) {
	// A bare model id with no settings at all: the catalog entry is the only
	// signal, and its dialect survives — with no ANTHROPIC_* family in play
	// there is nothing to force the anthropic default (api precedence rule
	// 3, provider-config-plan.md §4.3 rule 6). The entry's own dialect and
	// its own base URL are what reach the wire.
	m, err := resolveModel(nil, "deepseek-chat", "", nil)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.API != ai.APIOpenAICompletions {
		t.Errorf("api = %q, want the catalog entry's dialect (openai-completions)", m.API)
	}
	if m.BaseURL != "https://api.deepseek.com" {
		t.Errorf("baseUrl = %q, want the openai-dialect entry URL", m.BaseURL)
	}
	// The settings convention still forces the dialect when it is in play:
	// the same bare flag with ANTHROPIC_BASE_URL set resolves to the
	// anthropic-dialect entry (P5 contract, unchanged).
	env := envOf(map[string]string{"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic"})
	m2, err := resolveModel(nil, "deepseek-chat", "", env)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m2.API != ai.APIAnthropicMessages {
		t.Errorf("api = %q, want anthropic-messages under the settings convention", m2.API)
	}
	if m2.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("baseUrl = %q, want the settings URL", m2.BaseURL)
	}
}

// TestResolveModelUsesMergedCatalog — a models.json provider resolves like
// a built-in one: the merged snapshot carries the entry, and the
// provider/id flag finds it with its declared dialect, base URL and rates.
func TestResolveModelUsesMergedCatalog(t *testing.T) {
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
	// No ANTHROPIC_* settings in play: the entry's declared dialect is the
	// one that resolves — the "直接可用" shape from the plan's goal.
	m, err := resolveModel(snap, "my-proxy/my-model", "", nil)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.Provider != ai.ProviderId("my-proxy") {
		t.Errorf("provider = %q, want my-proxy", m.Provider)
	}
	if m.API != ai.APIOpenAICompletions {
		t.Errorf("api = %q, want the models.json entry's dialect", m.API)
	}
	if m.BaseURL != "https://proxy.internal.example.com/v1" {
		t.Errorf("baseUrl = %q, want the provider entry's", m.BaseURL)
	}
	if m.Name != "My Model" {
		t.Errorf("name = %q, want My Model", m.Name)
	}
	if m.Cost.Input != 1.5 || m.Cost.Output != 3.0 {
		t.Errorf("cost = %+v, want the models.json rates", m.Cost)
	}
	// env still wins over models.json (the documented precedence).
	env := envOf(map[string]string{"ANTHROPIC_BASE_URL": "https://env.example.com/v1"})
	m2, err := resolveModel(snap, "my-proxy/my-model", "", env)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m2.BaseURL != "https://env.example.com/v1" {
		t.Errorf("baseUrl = %q, want the env URL to win", m2.BaseURL)
	}
}

// TestResolveModelReportsBrokenProvider — an empty provider entry fails to
// compose (merge.go's pi rule). The snapshot keeps serving every other
// provider, but selecting the broken one is a hard error rather than a
// request that fails at the endpoint with a confusing message.
func TestResolveModelReportsBrokenProvider(t *testing.T) {
	snap := models.Build(models.Config{Providers: map[string]models.ProviderConfig{
		"broken": {},
	}})
	_, err := resolveModel(snap, "broken/whatever", "", nil)
	if err == nil {
		t.Fatal("selecting a provider whose entry failed to compose must error")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error = %v, want it to name the broken provider", err)
	}
	// An unrelated provider is unaffected.
	if _, err := resolveModel(snap, "deepseek/deepseek-chat", "", nil); err != nil {
		t.Errorf("an unrelated provider must still resolve: %v", err)
	}
}

// TestResolveModelNilSnapshotMeansBuiltin — a nil snapshot means "no
// models.json loaded": the built-in catalog alone, exactly models.Build()
// with no configs.
func TestResolveModelNilSnapshotMeansBuiltin(t *testing.T) {
	fromNil, err := resolveModel(nil, "deepseek/deepseek-chat", "", nil)
	if err != nil {
		t.Fatalf("resolveModel(nil): %v", err)
	}
	fromBuiltin, err := resolveModel(models.Build(), "deepseek/deepseek-chat", "", nil)
	if err != nil {
		t.Fatalf("resolveModel(Build()): %v", err)
	}
	if fromNil.ID != fromBuiltin.ID || fromNil.Provider != fromBuiltin.Provider ||
		fromNil.API != fromBuiltin.API || fromNil.BaseURL != fromBuiltin.BaseURL ||
		fromNil.Cost.Input != fromBuiltin.Cost.Input || fromNil.ContextWindow != fromBuiltin.ContextWindow {
		t.Errorf("nil snapshot = %+v, want the built-in view %+v", fromNil, fromBuiltin)
	}
}

func TestResolveModelAPIOverride(t *testing.T) {
	env := envOf(map[string]string{"ANTHROPIC_MODEL": "deepseek-chat"})
	m, err := resolveModel(nil, "", "openai-completions", env)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.API != ai.APIOpenAICompletions {
		t.Errorf("api = %q, want openai-completions (--api override)", m.API)
	}
	if m.BaseURL != "https://api.deepseek.com" {
		t.Errorf("baseUrl = %q, want the openai-dialect entry URL", m.BaseURL)
	}
}

func TestResolveModelUnknownHostKeepsWorking(t *testing.T) {
	// A proxy or gateway host the catalog does not know: the request must
	// still be sendable, with no invented rates. The provider label falls
	// back to the dialect inference (WithDefaults), which for the anthropic
	// default means "anthropic" — pi's models-store behaves the same.
	env := envOf(map[string]string{
		"ANTHROPIC_BASE_URL": "https://llm.internal.example.com/v1",
		"ANTHROPIC_MODEL":    "some-model",
	})
	m, err := resolveModel(nil, "", "", env)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if m.ID != "some-model" {
		t.Errorf("id = %q", m.ID)
	}
	if m.Provider != ai.ProviderAnthropic {
		t.Errorf("provider = %q, want anthropic (dialect inference)", m.Provider)
	}
	if m.BaseURL != "https://llm.internal.example.com/v1" {
		t.Errorf("baseUrl = %q", m.BaseURL)
	}
	if m.Cost.Input != 0 || m.Cost.Output != 0 {
		t.Errorf("cost should stay zero without a catalog entry: %+v", m.Cost)
	}
}

func TestResolveModelNoModelIsError(t *testing.T) {
	if _, err := resolveModel(nil, "", "", nil); err == nil {
		t.Fatal("expected an error when no model is configured anywhere")
	}
	// A provider-only flag still falls back to the settings model id, but
	// there is none here, so it is still an error.
	if _, err := resolveModel(nil, "deepseek/", "", nil); err == nil {
		t.Fatal("expected an error for a provider-only flag with no settings model")
	}
}

func TestResolveModelColonSuffixStripped(t *testing.T) {
	// pi's "provider/model:thinking" syntax: this port has no thinking flag,
	// so the suffix must not leak into the model id.
	m, err := resolveModel(nil, "deepseek/deepseek-chat:high", "", nil)
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

func TestProviderFromBaseURL(t *testing.T) {
	cases := map[string]ai.ProviderId{
		"":                                   "",
		"not a url":                          "",
		"https://api.deepseek.com/anthropic": ai.ProviderDeepseek,
		"https://api.anthropic.com":          ai.ProviderAnthropic,
		"https://API.ANTHROPIC.COM/v1":       ai.ProviderAnthropic,
		"https://api.moonshot.cn/v1":         ai.ProviderMoonshotAICN,
		"https://llm.internal.example.com":   "",
		"https://api.groq.com/openai/v1":     ai.ProviderGroq,
	}
	for base, want := range cases {
		if got := providerFromBaseURL(base); got != want {
			t.Errorf("providerFromBaseURL(%q) = %q, want %q", base, got, want)
		}
	}
}

// TestResolveCredential — the per-provider credential chain (plan §4.6,
// P3): models.json's apiKey first, auth.json's api_key second, nothing
// third. The env block is deliberately not a step of the chain (D6); it
// rides outside as the harness's last-resort fallback (settingsEnvToken),
// which the harness tests cover.
func TestResolveCredential(t *testing.T) {
	snap := models.Build(models.Config{Providers: map[string]models.ProviderConfig{
		"from-models": {
			BaseURL: "https://a.example.com/v1",
			API:     "openai-completions",
			APIKey:  "sk-models",
			Models: []models.ModelDefinition{{
				ID:   "m1",
				Cost: &models.ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1},
			}},
		},
		"both": {
			BaseURL: "https://b.example.com/v1",
			API:     "openai-completions",
			APIKey:  "sk-models-wins",
			Models: []models.ModelDefinition{{
				ID:   "m2",
				Cost: &models.ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1},
			}},
		},
	}})
	stack := auth.Build(auth.File{Entries: map[string]auth.Entry{
		"from-auth": {Type: auth.EntryTypeAPIKey, Key: "sk-auth"},
		"both":      {Type: auth.EntryTypeAPIKey, Key: "sk-auth-loses"},
	}})

	cases := []struct {
		provider string
		want     string
	}{
		// Step 1: models.json's apiKey.
		{"from-models", "sk-models"},
		// Step 2: auth.json's entry.
		{"from-auth", "sk-auth"},
		// Both declare one: models.json wins (the chain's order).
		{"both", "sk-models-wins"},
		// A built-in provider with nothing stored: no credential.
		{"deepseek", ""},
		// An unknown provider: no credential, not an error.
		{"nobody", ""},
	}
	for _, tc := range cases {
		if got := resolveCredential(snap, stack, tc.provider); got != tc.want {
			t.Errorf("resolveCredential(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}

	// Nil views mean "no files", the normal case: the chain resolves to
	// nothing rather than panicking.
	if got := resolveCredential(nil, nil, "from-models"); got != "" {
		t.Errorf("resolveCredential(nil, nil) = %q, want empty", got)
	}
}

// TestSettingsEnvToken — the settings env block's credential, kept as the
// harness's last-resort fallback (D6: that path is unaffected by the
// chain). Auth token wins over API key — the precedence the Anthropic SDK
// documents and the old CLI used.
func TestSettingsEnvToken(t *testing.T) {
	env := envOf(map[string]string{
		"ANTHROPIC_API_KEY":    "sk-key",
		"ANTHROPIC_AUTH_TOKEN": "sk-token",
	})
	if got := settingsEnvToken(env); got != "sk-token" {
		t.Errorf("settingsEnvToken = %q, want sk-token", got)
	}
	if got := settingsEnvToken(envOf(map[string]string{"ANTHROPIC_API_KEY": "sk-key"})); got != "sk-key" {
		t.Errorf("settingsEnvToken = %q, want sk-key", got)
	}
	if got := settingsEnvToken(nil); got != "" {
		t.Errorf("settingsEnvToken = %q, want empty", got)
	}
}
