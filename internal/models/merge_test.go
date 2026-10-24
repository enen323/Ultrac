package models

// merge_test.go — the merge layer (P1): pi's applyModelsJson rules,
// verified one at a time.
//
// The cases that matter are the ones with a wrong answer that still
// looks plausible: a replaced entry silently inheriting the old cost, an
// override repointing the baseUrl, a dialect switch keeping the other
// dialect's URL, a broken entry taking the built-in catalog down with it.

import (
	"strings"
	"testing"

	"ultrac/internal/ai"
)

func ptr[T any](v T) *T { return &v }

func TestBuildAppendsNewProviderModels(t *testing.T) {
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.example.com/v1",
			API:     "openai-completions",
			Models: []ModelDefinition{
				{ID: "my-model", Name: "My Model", Cost: &ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0.2}},
			},
		},
	}})

	m, ok := snap.Lookup(ai.ProviderId("my-proxy"), "my-model")
	if !ok {
		t.Fatal("my-proxy/my-model not in the merged catalog")
	}
	if m.API != ai.APIOpenAICompletions {
		t.Errorf("api = %q, want the provider-level dialect", m.API)
	}
	if m.BaseURL != "https://proxy.example.com/v1" {
		t.Errorf("baseUrl = %q, want the provider-level URL", m.BaseURL)
	}
	if m.Cost.Input != 1 || m.Cost.Output != 2 {
		t.Errorf("cost = %+v, want the definition's rates", m.Cost)
	}
	// pi's defaults for unset numeric fields.
	if m.ContextWindow != 128000 || m.MaxTokens != 16384 {
		t.Errorf("window/maxTokens = %d/%d, want 128000/16384", m.ContextWindow, m.MaxTokens)
	}
	if m.Name != "My Model" || m.Reasoning {
		t.Errorf("name/reasoning = %q/%v", m.Name, m.Reasoning)
	}
	// The built-in catalog is untouched.
	if _, ok := snap.Lookup(ai.ProviderDeepseek, "deepseek-chat"); !ok {
		t.Error("the built-in deepseek entry must survive a user merge")
	}
	if len(snap.Catalog()) != len(ai.Catalog())+1 {
		t.Errorf("catalog size = %d, want builtin+1", len(snap.Catalog()))
	}
}

func TestBuildReplacesEntryWholesale(t *testing.T) {
	// A definition with a matching id replaces the entry: the old cost,
	// window and limits are NOT inherited (pi builds a fresh model).
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"deepseek": {
			Models: []ModelDefinition{{ID: "deepseek-chat"}},
		},
	}})

	m, ok := snap.Lookup(ai.ProviderDeepseek, "deepseek-chat")
	if !ok {
		t.Fatal("deepseek-chat disappeared")
	}
	if m.Cost.Input != 0 {
		t.Errorf("cost.input = %v, want 0 — a replaced entry starts fresh", m.Cost.Input)
	}
	if m.ContextWindow != 128000 || m.MaxTokens != 16384 {
		t.Errorf("window/maxTokens = %d/%d, want the built-in defaults", m.ContextWindow, m.MaxTokens)
	}
	// api and baseUrl fall back to the replaced entry.
	if m.API != ai.APIOpenAICompletions {
		t.Errorf("api = %q, want the replaced entry's dialect", m.API)
	}
	if m.BaseURL != "https://api.deepseek.com" {
		t.Errorf("baseUrl = %q, want the replaced entry's URL", m.BaseURL)
	}
}

func TestBuildBaseURLOverridesEveryEntryOfTheProvider(t *testing.T) {
	// DeepSeek exposes the same model under two dialects at two URLs; a
	// provider baseUrl repoints both.
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"deepseek": {BaseURL: "https://proxy.internal/v1"},
	}})
	for _, id := range []string{"deepseek-chat", "deepseek-reasoner"} {
		m, _ := snap.Lookup(ai.ProviderDeepseek, id)
		if m.BaseURL != "https://proxy.internal/v1" {
			t.Errorf("%s baseUrl = %q, want the provider override", id, m.BaseURL)
		}
	}
	if m, _ := snap.Lookup(ai.ProviderDeepseek, "deepseek-chat"); m.API != ai.APIOpenAICompletions {
		t.Errorf("api = %q: a baseUrl override must not touch the dialect", m.API)
	}
}

func TestBuildModelOverrides(t *testing.T) {
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"deepseek": {ModelOverrides: map[string]ModelOverride{
			"deepseek-chat": {
				Name:          "Patched Chat",
				Reasoning:     ptr(true),
				MaxTokens:     ptr(16384),
				Cost:          &ModelCostOverride{Input: ptr(0.5)},
				Headers:       map[string]string{"X-Patched": "yes"},
				ContextWindow: ptr(64000),
			},
		}},
	}})
	m, ok := snap.Lookup(ai.ProviderDeepseek, "deepseek-chat")
	if !ok {
		t.Fatal("deepseek-chat disappeared")
	}
	if m.Name != "Patched Chat" {
		t.Errorf("name = %q, want the override", m.Name)
	}
	if !m.Reasoning {
		t.Error("reasoning should be forced true by the override")
	}
	if m.MaxTokens != 16384 || m.ContextWindow != 64000 {
		t.Errorf("maxTokens/window = %d/%d", m.MaxTokens, m.ContextWindow)
	}
	// Partial cost merge: input patched, the rest inherited.
	if m.Cost.Input != 0.5 || m.Cost.Output != 1.10 {
		t.Errorf("cost = %+v, want input patched and output inherited", m.Cost)
	}
	if m.Headers["X-Patched"] != "yes" {
		t.Errorf("headers = %v, want the override header", m.Headers)
	}
	// The sibling entry is untouched.
	if sib, _ := snap.Lookup(ai.ProviderDeepseek, "deepseek-reasoner"); sib.Name != "DeepSeek Reasoner" {
		t.Errorf("sibling name = %q, want untouched", sib.Name)
	}
}

func TestBuildOverrideTargetsUpsertedModel(t *testing.T) {
	// pi applies modelOverrides after the models[] upserts, so an override
	// can patch a model the same file just added.
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.example.com/v1",
			API:     "openai-completions",
			Models:  []ModelDefinition{{ID: "my-model", Cost: &ModelCost{Input: 1, Output: 2}}},
			ModelOverrides: map[string]ModelOverride{
				"my-model": {MaxTokens: ptr(4096)},
			},
		},
	}})
	m, ok := snap.Lookup(ai.ProviderId("my-proxy"), "my-model")
	if !ok {
		t.Fatal("my-model missing")
	}
	if m.MaxTokens != 4096 {
		t.Errorf("maxTokens = %d, want the override applied to the upserted entry", m.MaxTokens)
	}
	if m.Cost.Input != 1 {
		t.Errorf("cost.input = %v, want the upsert's rates kept", m.Cost.Input)
	}
}

func TestBuildOverrideForUnknownModelIsNoOp(t *testing.T) {
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"deepseek": {ModelOverrides: map[string]ModelOverride{"no-such-model": {MaxTokens: ptr(1)}}},
	}})
	if len(snap.Catalog()) != len(ai.Catalog()) {
		t.Errorf("catalog size = %d, want unchanged", len(snap.Catalog()))
	}
	if err := snap.Problem(ai.ProviderDeepseek); err != nil {
		t.Errorf("an unmatched override is not an error: %v", err)
	}
}

func TestBuildProviderHeadersReachEveryEntry(t *testing.T) {
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.example.com/v1",
			API:     "openai-completions",
			Headers: map[string]string{"X-Provider": "yes", "X-Both": "provider"},
			Models: []ModelDefinition{
				{ID: "a", Headers: map[string]string{"X-Both": "model"}},
				{ID: "b"},
			},
		},
	}})
	a, _ := snap.Lookup(ai.ProviderId("my-proxy"), "a")
	if a.Headers["X-Provider"] != "yes" {
		t.Errorf("a headers = %v, want the provider headers baked in", a.Headers)
	}
	if a.Headers["X-Both"] != "model" {
		t.Errorf("a headers = %v, want the model header to win per key", a.Headers)
	}
	b, _ := snap.Lookup(ai.ProviderId("my-proxy"), "b")
	if b.Headers["X-Provider"] != "yes" {
		t.Errorf("b headers = %v, want the provider headers", b.Headers)
	}
}

func TestBuildEmptyEntryIsAnError(t *testing.T) {
	// pi's empty-entry rule: an entry that specifies nothing is an error,
	// recorded per provider. The built-in catalog keeps working.
	snap := Build(Config{Providers: map[string]ProviderConfig{"ghost": {}}})
	err := snap.Problem(ai.ProviderId("ghost"))
	if err == nil {
		t.Fatal("an empty entry must be an error")
	}
	if !strings.Contains(err.Error(), "must specify") {
		t.Errorf("error = %v, want the pi empty-entry message", err)
	}
	if len(snap.Problems()) != 1 {
		t.Errorf("Problems() = %v, want exactly one", snap.Problems())
	}
	if len(snap.Catalog()) != len(ai.Catalog()) {
		t.Error("the built-in catalog must be unaffected")
	}
	// A name-only entry is empty too (pi's check does not count name).
	if err := Build(Config{Providers: map[string]ProviderConfig{"ghost": {Name: "Ghost"}}}).Problem(ai.ProviderId("ghost")); err == nil {
		t.Error("a name-only entry must be an error")
	}
}

func TestBuildMissingAPIIsAnError(t *testing.T) {
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.example.com/v1",
			Models:  []ModelDefinition{{ID: "my-model"}},
		},
	}})
	err := snap.Problem(ai.ProviderId("my-proxy"))
	if err == nil {
		t.Fatal("a new model with no api anywhere must be an error")
	}
	if !strings.Contains(err.Error(), `no "api" specified`) {
		t.Errorf("error = %v, want pi's api message", err)
	}
	if _, ok := snap.Lookup(ai.ProviderId("my-proxy"), "my-model"); ok {
		t.Error("a rejected definition must not land in the catalog")
	}
}

func TestBuildMissingBaseURLIsAnError(t *testing.T) {
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"my-proxy": {
			API:    "openai-completions",
			Models: []ModelDefinition{{ID: "my-model"}},
		},
	}})
	err := snap.Problem(ai.ProviderId("my-proxy"))
	if err == nil {
		t.Fatal("a custom model with no baseUrl anywhere must be an error")
	}
	if !strings.Contains(err.Error(), `"baseUrl" is required`) {
		t.Errorf("error = %v, want pi's baseUrl message", err)
	}
}

func TestBuildProjectLevelOverridesConfigHome(t *testing.T) {
	// D1: two levels, project over ConfigHome, per provider id.
	home := Config{Providers: map[string]ProviderConfig{
		"my-proxy": {BaseURL: "https://home.example.com/v1", API: "openai-completions", Models: []ModelDefinition{{ID: "m"}}},
		"shared":   {BaseURL: "https://home.example.com/v1", API: "openai-completions", Models: []ModelDefinition{{ID: "m"}}},
	}}
	project := Config{Providers: map[string]ProviderConfig{
		"shared": {BaseURL: "https://project.example.com/v1", API: "openai-completions", Models: []ModelDefinition{{ID: "m"}}},
	}}
	snap := Build(home, project)

	if m, _ := snap.Lookup(ai.ProviderId("my-proxy"), "m"); m.BaseURL != "https://home.example.com/v1" {
		t.Errorf("my-proxy baseUrl = %q, want the ConfigHome value", m.BaseURL)
	}
	if m, _ := snap.Lookup(ai.ProviderId("shared"), "m"); m.BaseURL != "https://project.example.com/v1" {
		t.Errorf("shared baseUrl = %q, want the project value (wholesale replace)", m.BaseURL)
	}
	if cfg, ok := snap.ProviderConfig(ai.ProviderId("shared")); !ok || cfg.BaseURL != "https://project.example.com/v1" {
		t.Errorf("ProviderConfig(shared) = %+v/%v, want the project entry", cfg, ok)
	}
}

func TestBuildProjectLevelHealsBrokenConfigHomeEntry(t *testing.T) {
	home := Config{Providers: map[string]ProviderConfig{"my-proxy": {}}}
	project := Config{Providers: map[string]ProviderConfig{
		"my-proxy": {BaseURL: "https://project.example.com/v1", API: "openai-completions", Models: []ModelDefinition{{ID: "m"}}},
	}}
	snap := Build(home, project)
	if err := snap.Problem(ai.ProviderId("my-proxy")); err != nil {
		t.Errorf("a good project entry should clear the ConfigHome problem: %v", err)
	}
	if _, ok := snap.Lookup(ai.ProviderId("my-proxy"), "m"); !ok {
		t.Error("the healed provider should resolve")
	}
}

func TestBuildOAuthFieldIsIgnored(t *testing.T) {
	// D4: oauth is accepted (it keeps the entry non-empty) and then
	// dropped — it changes nothing about the merged entry.
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.example.com/v1",
			API:     "openai-completions",
			OAuth:   []byte(`{"kind":"radius"}`),
			Models:  []ModelDefinition{{ID: "m"}},
		},
	}})
	if err := snap.Problem(ai.ProviderId("my-proxy")); err != nil {
		t.Fatalf("an oauth-carrying entry should merge cleanly: %v", err)
	}
	if m, _ := snap.Lookup(ai.ProviderId("my-proxy"), "m"); m.BaseURL != "https://proxy.example.com/v1" {
		t.Errorf("baseUrl = %q, want the entry's own URL", m.BaseURL)
	}
}

func TestSnapshotAccessorsReturnCopies(t *testing.T) {
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.example.com/v1",
			API:     "openai-completions",
			Headers: map[string]string{"X-K": "v"},
			Models:  []ModelDefinition{{ID: "m", Cost: &ModelCost{Input: 1, Output: 2, Tiers: []ai.ModelCostTier{{InputTokensAbove: 1, Input: 1}}}}},
		},
	}})

	first, _ := snap.Lookup(ai.ProviderId("my-proxy"), "m")
	first.Headers["X-K"] = "mutated"
	first.Cost.Tiers[0].Input = 99
	first.ContextWindow = 1

	second, _ := snap.Lookup(ai.ProviderId("my-proxy"), "m")
	if second.Headers["X-K"] != "v" {
		t.Errorf("headers leaked the mutation: %v", second.Headers)
	}
	if second.Cost.Tiers[0].Input != 1 {
		t.Errorf("tiers leaked the mutation: %v", second.Cost.Tiers)
	}
	if second.ContextWindow == 1 {
		t.Error("scalar fields leaked the mutation")
	}

	cat := snap.Catalog()
	for i := range cat {
		cat[i].ID = "wrecked"
	}
	if m, _ := snap.Lookup(ai.ProviderId("my-proxy"), "m"); m.ID != "m" {
		t.Error("Catalog() leaked its backing array")
	}
}

func TestSnapshotProvidersIncludesUserProviders(t *testing.T) {
	snap := Build(Config{Providers: map[string]ProviderConfig{
		"zz-proxy": {BaseURL: "https://zz.example.com/v1", API: "openai-completions", Models: []ModelDefinition{{ID: "m"}}},
	}})
	got := snap.Providers()
	if len(got) == 0 || got[len(got)-1] != "zz-proxy" {
		t.Fatalf("Providers() = %v, want the user provider last (lexical)", got)
	}
	for _, p := range got {
		if p == ai.ProviderDeepseek {
			return // built-ins still listed
		}
	}
	t.Error("Providers() lost the built-in providers")
}

func TestBuildEmptyConfigIsBuiltinOnly(t *testing.T) {
	snap := Build()
	if len(snap.Catalog()) != len(ai.Catalog()) {
		t.Errorf("catalog size = %d, want the builtin set", len(snap.Catalog()))
	}
	if err := snap.Problem(ai.ProviderDeepseek); err != nil {
		t.Errorf("no problems expected: %v", err)
	}
	if m, ok := snap.DefaultModel(ai.ProviderDeepseek); !ok || m.ID != "deepseek-chat" {
		t.Errorf("DefaultModel(deepseek) = %+v/%v, want the first entry", m, ok)
	}
}
