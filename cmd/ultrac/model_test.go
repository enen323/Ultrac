package main

// Tests for the settings→Model resolution (P5).
//
// The interesting cases are the ambiguous ones: a provider that exposes the
// same model under two dialects, a base URL whose host names no catalog
// provider, and a flag that carries only half the identity.

import (
	"testing"

	"ultrac/internal/ai"
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
	m, err := resolveModel("", "", env)
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
	m, err := resolveModel("deepseek/deepseek-chat", "", env)
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
	// signal, and its dialect must survive when the env convention is absent.
	m, err := resolveModel("deepseek-chat", "", nil)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	// resolveModel forces the anthropic dialect (settings convention); the
	// base URL then comes from the anthropic-dialect entry of the same
	// provider, not from the openai one.
	if m.API != ai.APIAnthropicMessages {
		t.Errorf("api = %q, want anthropic-messages", m.API)
	}
	if m.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("baseUrl = %q, want the anthropic-dialect entry URL", m.BaseURL)
	}
}

func TestResolveModelAPIOverride(t *testing.T) {
	env := envOf(map[string]string{"ANTHROPIC_MODEL": "deepseek-chat"})
	m, err := resolveModel("", "openai-completions", env)
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
	m, err := resolveModel("", "", env)
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
	if _, err := resolveModel("", "", nil); err == nil {
		t.Fatal("expected an error when no model is configured anywhere")
	}
	// A provider-only flag still falls back to the settings model id, but
	// there is none here, so it is still an error.
	if _, err := resolveModel("deepseek/", "", nil); err == nil {
		t.Fatal("expected an error for a provider-only flag with no settings model")
	}
}

func TestResolveModelColonSuffixStripped(t *testing.T) {
	// pi's "provider/model:thinking" syntax: this port has no thinking flag,
	// so the suffix must not leak into the model id.
	m, err := resolveModel("deepseek/deepseek-chat:high", "", nil)
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

func TestAPIKeyFor(t *testing.T) {
	// Auth token wins over API key — the precedence the Anthropic SDK
	// documents and the old CLI used.
	env := envOf(map[string]string{
		"ANTHROPIC_API_KEY":    "sk-key",
		"ANTHROPIC_AUTH_TOKEN": "sk-token",
	})
	if got := apiKeyFor(env); got != "sk-token" {
		t.Errorf("apiKeyFor = %q, want sk-token", got)
	}
	if got := apiKeyFor(envOf(map[string]string{"ANTHROPIC_API_KEY": "sk-key"})); got != "sk-key" {
		t.Errorf("apiKeyFor = %q, want sk-key", got)
	}
	if got := apiKeyFor(nil); got != "" {
		t.Errorf("apiKeyFor = %q, want empty", got)
	}
}
