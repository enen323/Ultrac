package main

// model.go — resolving the --model flag and the merged provider catalog into
// the ai.Model the agent loop needs, plus the per-provider credential lookup
// that authenticates it.
//
// # Where a model comes from
//
// Exactly two places, and neither is the settings env block:
//
//	--model provider/id        the command line
//	session seed               the model a resumed session last ran with
//
// A model that no settings `providers` entry (or legacy models.json entry)
// describes is not a model: the catalog is the source of truth for what exists.
// "I have no model" is a state the caller must surface, not something to guess
// at — see errNoModelConfigured.
//
// # Why ANTHROPIC_* is gone (settings-unify-plan.md D11)
//
// ANTHROPIC_MODEL / ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN used to be a
// second, parallel configuration system: a model id, an endpoint and one
// credential shared by every provider. Two sources for the same fact meant the
// env block could silently win over a provider's own entry, which is exactly
// how one provider's key ended up at another vendor's endpoint ("Your api key
// ... is invalid" while the key was perfectly good). There is now one source:
// the provider entry. A settings file that still carries the ANTHROPIC_* keys
// is read for nothing — they are ignored, and `ultrac config` says so.

import (
	"errors"
	"fmt"
	"strings"

	"ultrac/internal/ai"
	"ultrac/internal/models"
)

// errNoModelConfigured is "nothing named a model" — distinct from a
// configuration error.
//
// The distinction matters for the RPC mode: a client that can still show a
// model picker treats this as a state (answer "no model", let the user pick),
// while a broken provider definition is a hard failure. `errors.Is` on this
// sentinel is how a caller tells them apart.
var errNoModelConfigured = errors.New("no model configured")

// resolveModel turns the flag into a Model.
//
// Precedence, in order:
//
//  1. id: the --model flag ("provider/id", or a bare id the catalog knows).
//     There is no env fallback — ANTHROPIC_MODEL is ignored (D11).
//  2. provider: the flag's "provider/" part, else the catalog entry for the
//     id. A bare id that nothing declares is an error: guessing a provider
//     is how a request reaches an endpoint nobody chose.
//  3. api: the --api flag, else the entry's own declared dialect. The
//     ANTHROPIC_* convention that used to sit between them is gone (D11).
//  4. baseUrl: the entry's own, else ai.WithDefaults' provider default. The
//     merged catalog already encodes provider-level overrides (merge.go rule
//     2), so this is the whole chain.
//
// A merged-catalog entry for (provider, id) is adopted wholesale for its cost
// rates, context window and limits — but the resolved api dialect is forced
// back onto it, because one provider can expose the same model under two
// dialects at two URLs (DeepSeek's /anthropic vs /chat/completions). When the
// chosen dialect differs from the entry's, its baseUrl is dropped: that URL
// speaks the other protocol, and ai.WithDefaults fills in the one that
// matches.
func resolveModel(snap *models.Snapshot, flagModel, flagAPI string) (ai.Model, error) {
	if snap == nil {
		snap = models.Build()
	}
	providerName, id := splitModelFlag(flagModel)
	if id == "" {
		return ai.Model{}, errNoModelConfigured
	}

	var provider ai.ProviderId
	if providerName != "" {
		provider = ai.ProviderId(providerName)
	} else if entry, ok := catalogEntryByID(snap, id); ok {
		provider = entry.Provider
	}
	if provider == "" {
		return ai.Model{}, fmt.Errorf(
			"model %q: no provider declares it; use --model provider/id", id)
	}

	// A provider whose settings entry failed to compose must not resolve
	// silently: the snapshot dropped its models, and the fallbacks below would
	// produce a request that fails at the endpoint with a confusing error. pi
	// fails at selection time too (the models store throws for a provider
	// whose composition failed).
	if err := snap.Problem(provider); err != nil {
		return ai.Model{}, err
	}

	api := ai.APIAnthropicMessages
	entry, hasEntry := snap.Lookup(provider, id)
	switch {
	case flagAPI != "":
		api = ai.Api(flagAPI)
	case hasEntry && entry.API != "":
		api = entry.API
	}

	m := ai.Model{ID: id, Provider: provider, API: api}
	if hasEntry {
		m = entry
		m.API = api
		if entry.API != api {
			// The entry describes the other dialect of the same model, and
			// its base URL speaks that dialect.
			m.BaseURL = ""
		}
	}
	return ai.WithDefaults(m), nil
}

// splitModelFlag parses pi's "provider/id" model syntax. A bare id returns
// ("", id) so the catalog can name the provider; a flag with no id part
// returns ("provider", "") and the caller reports "no model configured"
// rather than silently using a default.
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

// catalogEntryByID finds the first merged-catalog entry with this model id,
// whatever its provider. It exists for the "user typed a model id, nothing
// else" case: the entry names the provider and the dialect.
func catalogEntryByID(snap *models.Snapshot, id string) (ai.Model, bool) {
	for _, m := range snap.Catalog() {
		if m.ID == id {
			return m, true
		}
	}
	return ai.Model{}, false
}

// resolveCredential returns a provider's credential from the resolved
// settings, or "" when it declares none.
//
// One step, no fallback (settings-unify-plan.md D11): the key lives in the
// provider entry it belongs to, next to the baseUrl it is valid for. The old
// env-block fallback meant a run could authenticate with a token that had
// nothing to do with the endpoint it was talking to; a provider without a key
// is now a configuration error with a name in it, not a silently borrowed
// credential.
func resolveCredential(snap *models.Snapshot, provider string) string {
	if snap == nil {
		return ""
	}
	if cfg, ok := snap.ProviderConfig(ai.ProviderId(provider)); ok {
		return cfg.APIKey
	}
	return ""
}
