package main

// model.go — resolving the settings env block, the merged models.json
// catalog and the --model flag into the ai.Model the agent loop needs
// (P5, P1).
//
// # Why this exists
//
// The settings file speaks the Anthropic env-var dialect
// (ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / ANTHROPIC_MODEL) — that is
// what ~/.ultrac/settings.json has carried since T0, and the pi layer speaks
// Model structs keyed by provider and api dialect. Something has to translate,
// and it is this file. The translation rules below are the whole contract;
// they are written out because "which endpoint did it pick" is the first
// question whenever a run talks to the wrong server.

import (
	"fmt"
	"net/url"
	"strings"

	"ultrac/internal/ai"
	"ultrac/internal/config"
	"ultrac/internal/models"
)

// resolveModel turns the flag and the settings env into a Model.
//
// Precedence, in order:
//
//  1. id: the --model flag (bare or "provider/id") beats ANTHROPIC_MODEL.
//  2. provider: the flag's "provider/" part, else the merged-catalog entry
//     for the id, else the host of ANTHROPIC_BASE_URL, else "". With no
//     signal at all, ai.WithDefaults infers the provider from the dialect
//     — pi's models-store does the same, and for the anthropic default
//     that means "anthropic", which is the honest label for a request
//     aimed at an Anthropic-dialect endpoint we could not otherwise
//     identify.
//  3. api: the --api flag, else the ANTHROPIC_* convention, else the
//     merged-catalog entry's own dialect. The three-way rule is §4.3 rule
//     6 of the provider-config plan ("env 指定哪个 dialect 就用哪个")
//     made precise: the anthropic default exists to interpret the
//     settings block — ANTHROPIC_MODEL or ANTHROPIC_BASE_URL being set
//     means that family is in play, and its dialect wins (the P5
//     contract, byte for byte). With neither set nothing forces the
//     dialect, so the entry's own api stands — a models.json declaration
//     of "openai-completions" reaches the wire instead of being
//     overwritten by the anthropic default and sent to the wrong
//     endpoint. anthropic-messages remains the last resort for an id no
//     entry describes; --api always wins over all of it.
//  4. baseUrl: ANTHROPIC_BASE_URL beats the merged catalog's, which beats
//     the provider default (ai.WithDefaults' fallback chain). "The merged
//     catalog's" already encodes the models.json order: a provider-level
//     baseUrl overrides the models[] entries of that provider, which
//     override the built-in entry (merge.go rule 2).
//
// A merged-catalog entry for (provider, id) is adopted wholesale for its
// cost rates, context window and limits — but the resolved api dialect is
// forced back onto it, because one provider can expose the same model
// under two dialects at two URLs (DeepSeek's /anthropic vs
// /chat/completions) and the settings block says which one is in play.
//
// The snapshot is the merged view (built-in catalog + both models.json
// layers); a nil snapshot means "no models.json", which is exactly
// models.Build() with no configs.
func resolveModel(snap *models.Snapshot, flagModel, flagAPI string, env map[string]string) (ai.Model, error) {
	if snap == nil {
		snap = models.Build()
	}
	providerName, id := splitModelFlag(flagModel)
	if id == "" {
		id = strings.TrimSpace(env[config.EnvModel])
	}
	if id == "" {
		return ai.Model{}, fmt.Errorf("no model configured: set ANTHROPIC_MODEL in %s or pass --model",
			config.ConfigHome())
	}

	var provider ai.ProviderId
	if providerName != "" {
		provider = ai.ProviderId(providerName)
	}
	if provider == "" {
		if entry, ok := catalogEntryByID(snap, id); ok {
			provider = entry.Provider
		}
	}
	if provider == "" {
		provider = providerFromBaseURL(env[config.EnvBaseURL])
	}
	// A provider whose models.json entry failed to compose must not
	// resolve silently: the snapshot dropped its models, and the bare
	// fallbacks below would produce a request that fails at the endpoint
	// with a confusing error. pi fails at selection time too (the models
	// store throws for a provider whose composition failed).
	if provider != "" {
		if err := snap.Problem(provider); err != nil {
			return ai.Model{}, err
		}
	}

	// The dialect decision, in precedence order (see the doc comment):
	// an explicit --api flag, then the ANTHROPIC_* settings convention
	// when that family is in play, then the entry's own api.
	api := ai.APIAnthropicMessages
	entry, hasEntry := snap.Lookup(provider, id)
	switch {
	case flagAPI != "":
		api = ai.Api(flagAPI)
	case settingsSpeakAnthropic(env):
		// The settings block names the model or the endpoint: its
		// dialect is the one in play (P5).
	case hasEntry && entry.API != "":
		api = entry.API
	}

	m := ai.Model{ID: id, Provider: provider, API: api}
	if hasEntry {
		m = entry
		m.API = api
		if entry.API != api {
			// The entry describes the other dialect of the same model, and
			// its base URL speaks that dialect. Drop it so WithDefaults
			// picks the URL of the entry matching the resolved dialect
			// (DeepSeek: /chat/completions vs /anthropic).
			m.BaseURL = ""
		}
	}
	if base := strings.TrimSpace(env[config.EnvBaseURL]); base != "" {
		m.BaseURL = base
	}
	return ai.WithDefaults(m), nil
}

// settingsSpeakAnthropic reports whether the ANTHROPIC_* settings family is
// in play for this resolution: it names the model, or it names the
// endpoint. Either one means the settings block is choosing in the
// Anthropic dialect by convention (the api precedence in resolveModel,
// rule 2).
func settingsSpeakAnthropic(env map[string]string) bool {
	return strings.TrimSpace(env[config.EnvModel]) != "" ||
		strings.TrimSpace(env[config.EnvBaseURL]) != ""
}

// splitModelFlag parses pi's "provider/id" model syntax. A bare id returns
// ("", id); a flag with no id part returns ("provider", "") so the caller
// falls back to the settings model id while keeping the provider hint.
func splitModelFlag(flag string) (provider, id string) {
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return "", ""
	}
	// pi also supports a ":<thinking>" suffix on the pattern; this port has
	// no CLI thinking flag yet, so it is rejected rather than silently
	// folded into the model id.
	if i := strings.IndexByte(flag, ':'); i >= 0 {
		flag = flag[:i]
	}
	if i := strings.IndexByte(flag, '/'); i >= 0 {
		return flag[:i], flag[i+1:]
	}
	return "", flag
}

// catalogEntryByID finds the first merged-catalog entry with this model
// id, whatever its provider. It exists for the "user typed a model id,
// nothing else" case: the entry names the provider and the dialect, which
// is the best guess available.
func catalogEntryByID(snap *models.Snapshot, id string) (ai.Model, bool) {
	for _, m := range snap.Catalog() {
		if m.ID == id {
			return m, true
		}
	}
	return ai.Model{}, false
}

// hostProviders maps known API hosts to provider ids. The host is the only
// provider signal a bare base URL carries.
var hostProviders = map[string]ai.ProviderId{
	"api.anthropic.com": ai.ProviderAnthropic,
	"api.deepseek.com":  ai.ProviderDeepseek,
	"api.moonshot.cn":   ai.ProviderMoonshotAICN,
	"api.moonshot.ai":   ai.ProviderMoonshotAI,
	"open.bigmodel.cn":  ai.ProviderZaiCodingCN,
	"api.z.ai":          ai.ProviderZai,
	"api.groq.com":      ai.ProviderGroq,
	"api.x.ai":          ai.ProviderXAI,
	"api.mistral.ai":    ai.ProviderMistral,
	"api.minimax.chat":  ai.ProviderMinimax,
	"api.openai.com":    ai.ProviderOpenAI,
	"api.together.xyz":  ai.ProviderTogether,
}

// providerFromBaseURL infers the provider from a base URL's host. An unknown
// host yields "" — the request still goes out, it just carries no catalog
// rates. Guessing a provider from a host we do not know would attach another
// provider's prices to the usage report.
func providerFromBaseURL(base string) ai.ProviderId {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if p, ok := hostProviders[host]; ok {
		return p
	}
	// "api.<provider>.com" is the common shape for hosts not in the table;
	// it only fires for hosts we can map to a real catalog provider id, so a
	// proxy at proxy.internal.example.com yields "" rather than a fantasy.
	if rest, ok := strings.CutPrefix(host, "api."); ok {
		candidate := strings.TrimSuffix(rest, ".com")
		candidate = strings.TrimSuffix(candidate, ".cn")
		for _, p := range ai.Providers() {
			if string(p) == candidate {
				return p
			}
		}
	}
	return ""
}

// apiKeyFor picks the credential the way the provider clients expect:
// ANTHROPIC_AUTH_TOKEN wins over ANTHROPIC_API_KEY (the same precedence the
// old CLI used, and the one the Anthropic SDK documents).
func apiKeyFor(env map[string]string) string {
	if k := strings.TrimSpace(env[config.EnvAuthToken]); k != "" {
		return k
	}
	return strings.TrimSpace(env[config.EnvAPIKey])
}
