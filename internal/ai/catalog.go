package ai

import "sort"

// Built-in model catalog.
//
// A hand-maintained starter set, not a generated mirror: pi's catalog is
// produced by scripts/generate-models.ts from provider data JSONs that are
// not part of this checkout, so transcribing it wholesale was not possible.
// What is here covers the endpoints this project verifies against (Anthropic
// first-party and DeepSeek, both dialects) plus the models an agent actually
// needs. Prices are per million tokens from the providers' public docs; they
// only feed Usage.Cost, so a stale rate shows up as wrong accounting, never
// as a failed request.
//
// The catalog is a source of defaults, not a gate: an unknown model id still
// works — Lookup reports false and the caller sends the id with the
// provider's base URL and no cost rates (cost stays zero).

// builtinCatalog is the starter set. Order within a provider is the
// preference order DefaultModel uses.
var builtinCatalog = []Model{
	// --- Anthropic (anthropic-messages dialect) ---
	{
		ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5",
		API: APIAnthropicMessages, Provider: ProviderAnthropic,
		BaseURL:       "https://api.anthropic.com",
		Input:         []ModelInputModality{ModalityText, ModalityImage},
		Cost:          ModelCost{Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
		Reasoning:     true,
		ContextWindow: 200000, MaxTokens: 64000,
	},
	{
		ID: "claude-opus-4-1", Name: "Claude Opus 4.1",
		API: APIAnthropicMessages, Provider: ProviderAnthropic,
		BaseURL:       "https://api.anthropic.com",
		Input:         []ModelInputModality{ModalityText, ModalityImage},
		Cost:          ModelCost{Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
		Reasoning:     true,
		ContextWindow: 200000, MaxTokens: 32000,
	},
	{
		ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5",
		API: APIAnthropicMessages, Provider: ProviderAnthropic,
		BaseURL:       "https://api.anthropic.com",
		Input:         []ModelInputModality{ModalityText, ModalityImage},
		Cost:          ModelCost{Input: 1.0, Output: 5.0, CacheRead: 0.10, CacheWrite: 1.25},
		Reasoning:     false,
		ContextWindow: 200000, MaxTokens: 64000,
	},
	// --- DeepSeek (openai-completions dialect) ---
	{
		ID: "deepseek-chat", Name: "DeepSeek Chat",
		API: APIOpenAICompletions, Provider: ProviderDeepseek,
		BaseURL:       "https://api.deepseek.com",
		Input:         []ModelInputModality{ModalityText},
		Cost:          ModelCost{Input: 0.27, Output: 1.10, CacheRead: 0.07, CacheWrite: 0.27},
		Reasoning:     false,
		ContextWindow: 128000, MaxTokens: 8192,
	},
	{
		ID: "deepseek-reasoner", Name: "DeepSeek Reasoner",
		API: APIOpenAICompletions, Provider: ProviderDeepseek,
		BaseURL:       "https://api.deepseek.com",
		Input:         []ModelInputModality{ModalityText},
		Cost:          ModelCost{Input: 0.55, Output: 2.19, CacheRead: 0.14, CacheWrite: 0.55},
		Reasoning:     true,
		ContextWindow: 128000, MaxTokens: 65536,
	},
	// --- DeepSeek's Anthropic-compatible endpoint ---
	//
	// The same models behind /anthropic: a provider can expose the same
	// catalog entry under two dialects, and the dialect is what picks the
	// adapter. This is the shape ~/.ultrac/settings.json already uses
	// (ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic).
	{
		ID: "deepseek-chat", Name: "DeepSeek Chat (Anthropic API)",
		API: APIAnthropicMessages, Provider: ProviderDeepseek,
		BaseURL:       "https://api.deepseek.com/anthropic",
		Input:         []ModelInputModality{ModalityText},
		Cost:          ModelCost{Input: 0.27, Output: 1.10, CacheRead: 0.07, CacheWrite: 0.27},
		Reasoning:     true,
		ContextWindow: 128000, MaxTokens: 8192,
	},
}

// Catalog returns the built-in models in declaration order.
func Catalog() []Model {
	out := make([]Model, len(builtinCatalog))
	copy(out, builtinCatalog)
	return out
}

// Lookup finds a catalog entry by provider and model id. The second result
// reports whether the pair was found; a false with a nil model means "send
// the id as-is".
func Lookup(provider ProviderId, id string) (Model, bool) {
	for _, m := range builtinCatalog {
		if m.Provider == provider && m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// DefaultModel returns the first catalog entry for a provider, which is the
// declaration-order preference. The bool reports whether the provider has
// any entry at all.
func DefaultModel(provider ProviderId) (Model, bool) {
	for _, m := range builtinCatalog {
		if m.Provider == provider {
			return m, true
		}
	}
	return Model{}, false
}

// Providers lists the providers that have at least one catalog entry, in
// lexical order.
func Providers() []ProviderId {
	seen := make(map[ProviderId]struct{}, len(builtinCatalog))
	var out []ProviderId
	for _, m := range builtinCatalog {
		if _, dup := seen[m.Provider]; dup {
			continue
		}
		seen[m.Provider] = struct{}{}
		out = append(out, m.Provider)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// WithDefaults fills the zero-valued fields a caller usually leaves unset:
// the provider's base URL, the api dialect of the provider's other entries,
// and a text-only input modality. It exists so a settings file that names
// just a provider and a model id produces a usable Model — the same job
// pi's models-store does when it merges user overrides onto catalog
// entries (models-store.ts).
//
// The base URL is filled from the entry matching both provider and dialect
// when one exists: the same provider can expose the same model under two
// dialects at two URLs (DeepSeek's /anthropic endpoint), and picking the
// wrong one sends the request to an endpoint that speaks a different
// protocol.
func WithDefaults(m Model) Model {
	if m.Provider == "" && m.API != "" {
		// Infer the provider from the api when only the dialect is known.
		for _, entry := range builtinCatalog {
			if entry.API == m.API {
				m.Provider = entry.Provider
				break
			}
		}
	}
	if m.BaseURL == "" {
		if entry, ok := matchEntry(m.Provider, m.API); ok {
			m.BaseURL = entry.BaseURL
		}
	}
	if m.API == "" {
		if def, ok := DefaultModel(m.Provider); ok {
			m.API = def.API
		}
	}
	if len(m.Input) == 0 {
		m.Input = []ModelInputModality{ModalityText}
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	return m
}

// matchEntry finds the catalog entry that best describes the pair: exact
// provider+dialect first, then provider, then dialect.
func matchEntry(provider ProviderId, api Api) (Model, bool) {
	var byProvider, byAPI Model
	haveProvider, haveAPI := false, false
	for _, entry := range builtinCatalog {
		if entry.Provider == provider && entry.API == api {
			return entry, true
		}
		if entry.Provider == provider && !haveProvider {
			byProvider, haveProvider = entry, true
		}
		if entry.API == api && !haveAPI {
			byAPI, haveAPI = entry, true
		}
	}
	if haveProvider {
		return byProvider, true
	}
	return byAPI, haveAPI
}
