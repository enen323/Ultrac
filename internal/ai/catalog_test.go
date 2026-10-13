package ai

import (
	"strings"
	"testing"
)

func TestCatalogLookupAndDefaults(t *testing.T) {
	m, ok := Lookup(ProviderAnthropic, "claude-sonnet-4-5")
	if !ok {
		t.Fatal("claude-sonnet-4-5 missing from the catalog")
	}
	if m.API != APIAnthropicMessages {
		t.Errorf("api = %q", m.API)
	}
	if m.BaseURL != "https://api.anthropic.com" {
		t.Errorf("baseUrl = %q", m.BaseURL)
	}
	if m.Cost.Input != 3.0 {
		t.Errorf("input rate = %v", m.Cost.Input)
	}

	if _, ok := Lookup(ProviderAnthropic, "no-such-model"); ok {
		t.Error("an unknown id must not resolve")
	}

	def, ok := DefaultModel(ProviderDeepseek)
	if !ok || def.ID != "deepseek-chat" {
		t.Errorf("deepseek default = %+v ok=%v", def, ok)
	}
}

// TestWithDefaultsFillsProviderShape: a settings file naming only a provider
// and an id must still produce a usable model.
func TestWithDefaultsFillsProviderShape(t *testing.T) {
	m := WithDefaults(Model{Provider: ProviderDeepseek, ID: "deepseek-chat"})
	if m.BaseURL != "https://api.deepseek.com" {
		t.Errorf("baseUrl = %q", m.BaseURL)
	}
	if m.API != APIOpenAICompletions {
		t.Errorf("api = %q", m.API)
	}
	if len(m.Input) != 1 || m.Input[0] != ModalityText {
		t.Errorf("input = %v", m.Input)
	}
	if m.Name != "deepseek-chat" {
		t.Errorf("name = %q", m.Name)
	}

	// The same model id under the other dialect keeps its own base URL:
	// dialect is what selects the adapter, and DeepSeek's anthropic
	// endpoint is a different URL.
	m2 := WithDefaults(Model{Provider: ProviderDeepseek, ID: "deepseek-chat", API: APIAnthropicMessages})
	if m2.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("anthropic-dialect baseUrl = %q", m2.BaseURL)
	}
}

func TestCatalogEntriesAreWellFormed(t *testing.T) {
	for _, m := range Catalog() {
		if m.ID == "" || m.Provider == "" || m.API == "" || m.BaseURL == "" {
			t.Errorf("entry %+v is missing identity fields", m)
		}
		if m.MaxTokens <= 0 || m.ContextWindow <= 0 {
			t.Errorf("entry %s has no usable limits", m.ID)
		}
		if m.Cost.Input <= 0 || m.Cost.Output <= 0 {
			t.Errorf("entry %s has no pricing (cost would always be zero)", m.ID)
		}
		if !strings.HasPrefix(m.BaseURL, "https://") {
			t.Errorf("entry %s baseUrl is not https: %q", m.ID, m.BaseURL)
		}
	}
}
