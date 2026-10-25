package config

// providers_test.go — the typed "providers" section
// (docs/开发计划/新功能计划/settings-unify-plan.md P1).
//
// Four properties are load-bearing and each has a test here:
//
//	round-trip   a settings document with providers survives read → write
//	             with its other sections byte-identical (R3)
//	parking      a malformed providers section is preserved, not dropped
//	wholesale    same provider id replaces the whole entry (D2)
//	user-only    a non-user layer's providers are ignored and reported (D4)

import (
	"encoding/json"
	"strings"
	"testing"

	"ultrac/internal/models"
)

// providersDoc is a realistic settings file: the two sections this package
// already typed, one it does not (hooks, so Extra is exercised), and the new
// providers section.
const providersDoc = `{
  "env": {"ANTHROPIC_MODEL": "deepseek-flash"},
  "hooks": {"Stop": [{"command": "echo done"}]},
  "providers": {
    "api-stepfun-com": {
      "baseUrl": "https://api.stepfun.com/step_plan/v1",
      "api": "openai-completions",
      "apiKey": "6CRS-secret-key",
      "models": [{"id": "step-5-preview", "contextWindow": 128000, "maxTokens": 8192}]
    }
  }
}`

func TestProvidersRoundTripPreservesOtherSections(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(providersDoc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s.Providers) != 1 {
		t.Fatalf("providers = %d entries, want 1", len(s.Providers))
	}
	pc := s.Providers["api-stepfun-com"]
	if pc.BaseURL != "https://api.stepfun.com/step_plan/v1" {
		t.Errorf("baseUrl = %q", pc.BaseURL)
	}
	if pc.API != "openai-completions" {
		t.Errorf("api = %q", pc.API)
	}
	if pc.APIKey != "6CRS-secret-key" {
		t.Errorf("apiKey = %q", pc.APIKey)
	}
	if len(pc.Models) != 1 || pc.Models[0].ID != "step-5-preview" {
		t.Fatalf("models = %+v", pc.Models)
	}

	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Settings
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	// The typed fields survive.
	if len(back.Providers) != 1 || back.Providers["api-stepfun-com"].APIKey != "6CRS-secret-key" {
		t.Errorf("providers lost through round-trip: %+v", back.Providers)
	}
	if back.Env["ANTHROPIC_MODEL"] != "deepseek-flash" {
		t.Errorf("env lost through round-trip: %+v", back.Env)
	}
	// And the section this package does not model is still there verbatim.
	if _, ok := back.Extra["hooks"]; !ok {
		t.Errorf("hooks section dropped by a write that only touched providers: %s", out)
	}
	// The parked-raw copy must not coexist with the typed one, or the key
	// would be written twice under two shapes.
	if _, parked := back.Extra["providers"]; parked {
		t.Errorf("providers written both typed and parked: %s", out)
	}
}

func TestProvidersMalformedSectionParkedInExtra(t *testing.T) {
	// "providers" as an array is not decodable into the typed map. It must
	// survive a write so the user's broken edit is still there to fix.
	const doc = `{"providers": [{"baseUrl": "x"}], "env": {"A": "b"}}`
	var s Settings
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Providers != nil {
		t.Fatalf("malformed providers decoded into the typed field: %+v", s.Providers)
	}
	if _, ok := s.Extra["providers"]; !ok {
		t.Fatal("malformed providers not parked in Extra")
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"providers":[{"baseUrl":"x"}]`) {
		t.Errorf("parked raw was lost on write: %s", out)
	}
}

func TestProvidersEmptyObjectDecodesToNil(t *testing.T) {
	// "declared but empty" and "not declared" must look the same downstream,
	// or a resolved view reports a provider stack that has nothing in it.
	var s Settings
	if err := json.Unmarshal([]byte(`{"providers": {}}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Providers != nil {
		t.Errorf("empty providers object = %+v, want nil", s.Providers)
	}
}

func TestMergeSettingsReplacesProviderWholesale(t *testing.T) {
	// A base layer with key + baseUrl; an overlay that only names a
	// different baseUrl. The overlay's entry must NOT inherit the key: a
	// field-level merge would manufacture a provider combination nobody
	// configured (D2).
	base := Settings{Providers: map[string]models.ProviderConfig{
		"p": {BaseURL: "https://base.example/v1", API: "anthropic-messages", APIKey: "sk-base"},
	}}
	over := Settings{Providers: map[string]models.ProviderConfig{
		"p": {BaseURL: "https://over.example/v1"},
	}}

	out := MergeSettings(base, over)
	got, ok := out.Providers["p"]
	if !ok {
		t.Fatal("provider p missing after merge")
	}
	if got.BaseURL != "https://over.example/v1" {
		t.Errorf("baseUrl = %q, want the overlay's", got.BaseURL)
	}
	if got.APIKey != "" {
		t.Errorf("apiKey = %q — a wholesale replace must not inherit the base layer's key", got.APIKey)
	}
	if got.API != "" {
		t.Errorf("api = %q — a wholesale replace must not inherit the base layer's dialect", got.API)
	}
}

func TestMergeSettingsUnionsDistinctProviders(t *testing.T) {
	base := Settings{Providers: map[string]models.ProviderConfig{
		"a": {BaseURL: "https://a.example/v1"},
	}}
	over := Settings{Providers: map[string]models.ProviderConfig{
		"b": {BaseURL: "https://b.example/v1"},
	}}
	out := MergeSettings(base, over)
	if len(out.Providers) != 2 {
		t.Fatalf("providers = %d, want 2 (%+v)", len(out.Providers), out.Providers)
	}
}

func TestMergeLayersIgnoresProvidersFromNonUserLayers(t *testing.T) {
	providers := func(url string) Settings {
		return Settings{Providers: map[string]models.ProviderConfig{
			"leaky": {BaseURL: url, APIKey: "secret"},
		}}
	}

	layers := []Layer{
		{Source: SourceUser, Path: "/home/u/.ultrac/settings.json", Settings: providers("https://user.example/v1")},
		{Source: SourceProject, Path: "/repo/.ultrac/settings.json", Settings: providers("https://project.example/v1")},
		{Source: SourceLocal, Path: "/repo/.ultrac/settings.local.json", Settings: providers("https://local.example/v1")},
	}

	merged, problems := MergeLayers(layers)

	// The user layer's entry is the only one that counts.
	if got := merged.Providers["leaky"].BaseURL; got != "https://user.example/v1" {
		t.Errorf("baseUrl = %q, want the user layer's — a project file must not win here", got)
	}
	// Both non-user layers are reported, with their paths.
	if len(problems) != 2 {
		t.Fatalf("problems = %d, want 2 (%+v)", len(problems), problems)
	}
	paths := []string{problems[0].Path, problems[1].Path}
	for _, want := range []string{"/repo/.ultrac/settings.json", "/repo/.ultrac/settings.local.json"} {
		if !strings.Contains(strings.Join(paths, ","), want) {
			t.Errorf("problem paths %v missing %s", paths, want)
		}
	}
	if err := ProviderProblemsError(problems); err == nil {
		t.Error("ProviderProblemsError returned nil for two problems")
	} else if !strings.Contains(err.Error(), "user settings layer") {
		t.Errorf("error text does not say where providers belong: %v", err)
	}
}

func TestMergeLayersIgnoresPolicyProvidersSilently(t *testing.T) {
	// An enterprise policy declaring providers is dropped without a warning:
	// it is a deliberate admin act this build does not support yet, and
	// warning on every launch is noise (D4).
	layers := []Layer{{
		Source: SourcePolicy,
		Path:   "/etc/ClaudeCode/managed-settings.json",
		Settings: Settings{Providers: map[string]models.ProviderConfig{
			"p": {BaseURL: "https://policy.example/v1"},
		}},
	}}
	merged, problems := MergeLayers(layers)
	if len(merged.Providers) != 0 {
		t.Errorf("policy providers were honoured: %+v", merged.Providers)
	}
	if len(problems) != 0 {
		t.Errorf("policy providers produced warnings: %+v", problems)
	}
}

func TestMergeLayersKeepsEnvFromEveryLayer(t *testing.T) {
	// The providers rule is a carve-out, not a general "only user settings
	// count" rule: env still merges from all layers.
	layers := []Layer{
		{Source: SourceUser, Settings: Settings{Env: map[string]string{"A": "1", "B": "user"}}},
		{Source: SourceProject, Settings: Settings{Env: map[string]string{"B": "project"}}},
	}
	merged, _ := MergeLayers(layers)
	if merged.Env["A"] != "1" {
		t.Errorf("A = %q, want 1", merged.Env["A"])
	}
	if merged.Env["B"] != "project" {
		t.Errorf("B = %q, want project (project layer still wins for env)", merged.Env["B"])
	}
}
