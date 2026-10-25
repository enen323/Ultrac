package main

// configfiles.go — the provider section of `ultrac config`
// (settings-unify-plan.md, superseding provider-config-plan.md P4).
//
// # What it answers
//
// "Why did my key not work" has exactly three answers — the provider declares
// no credential, it declares one in a place the run did not read, or the file
// is broken — and all three are visible here or nowhere: the settings layer
// that owns providers, every declared provider with its base URL, dialect,
// model count and where the credential comes from, the key masked, and the
// layer-ownership violations that made a provider invisible.
//
// It also reports the legacy models.json / auth.json files while they still
// exist, because their presence means the run is on the fallback path and the
// fix is one command.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"ultrac/internal/ai"
	"ultrac/internal/config"
	"ultrac/internal/models"
)

// printProviderConfigSection renders the section. cwd is the working directory
// the project layers hang off; layers is the stack the caller already loaded,
// so the section cannot disagree with what the command reported above it.
func printProviderConfigSection(stdout, stderr io.Writer, cwd string, layers []config.Layer) {
	merged, problems := config.MergeLayers(layers)
	providers := merged.Providers
	snap := snapshotFromSettings(merged)
	for _, p := range snap.Problems() {
		fmt.Fprintf(stderr, "warning: %v\n", p)
	}

	// -- providers (settings.json) -----------------------------------------
	fmt.Fprintln(stdout, "providers (settings.json):")
	userPath := filepath.Join(config.ConfigHome(), "settings.json")
	fmt.Fprintf(stdout, "  user layer:    %s (%s)\n", userPath, existsLabel(userPath))

	declared := declaredProviders(providers)
	if len(declared) == 0 {
		fmt.Fprintln(stdout, "  providers: (none declared)")
	} else {
		for _, id := range declared {
			cfg, _ := snap.ProviderConfig(ai.ProviderId(id))
			fmt.Fprintf(stdout, "    %s: baseUrl=%s, api=%s, models=%d, credential=%s, key=%s\n",
				id,
				providerBaseURL(snap, ai.ProviderId(id), cfg),
				providerAPI(snap, ai.ProviderId(id), cfg),
				providerModelCount(snap, ai.ProviderId(id)),
				credentialSource(cfg),
				credentialKey(cfg))
		}
	}

	// -- layer ownership violations ----------------------------------------
	//
	// A non-user layer that declared providers had them ignored (plan D4).
	// Saying so is the whole point: otherwise the provider is simply missing
	// with no explanation.
	if err := config.ProviderProblemsError(problems); err != nil {
		fmt.Fprintf(stdout, "  ignored (providers must live in the user layer):\n")
		for _, p := range problems {
			fmt.Fprintf(stdout, "    %s: %s\n", p.Path, joinWithComma(p.Providers))
		}
	}

	// -- env keys that are no longer read (D11) ----------------------------
	//
	// A settings file that still carries the ANTHROPIC_* family is not an
	// error — it is simply not a source any more. Printing the keys answers
	// the one question the block above cannot: "I set ANTHROPIC_BASE_URL,
	// why is nothing using it?"
	effective := config.EffectiveEnv(layers)
	if ignored := ignoredEnvKeys(effective); len(ignored) > 0 {
		fmt.Fprintln(stdout, "  ignored env keys (not read for model or credential selection):")
		for _, k := range ignored {
			fmt.Fprintf(stdout, "    %s=%s\n", k, maskIfSecret(k, effective[k]))
		}
	}

}

// joinWithComma renders a provider id list for a diagnostic line.
func joinWithComma(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	out := ids[0]
	for _, id := range ids[1:] {
		out += ", " + id
	}
	return out
}

// ignoredEnvKeys lists the settings env keys that used to configure model
// resolution and no longer do (settings-unify-plan.md D11), in a stable order.
//
// They are reported rather than rejected: a settings file is shared with other
// tools and hand-edited, and refusing to start over a stale key would be worse
// than saying "not read".
func ignoredEnvKeys(env map[string]string) []string {
	var out []string
	for _, k := range []string{config.EnvModel, config.EnvBaseURL, config.EnvAuthToken, config.EnvAPIKey} {
		if _, ok := env[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// existsLabel reports whether a layer file is there. ENOENT is the normal case
// and says so plainly.
func existsLabel(path string) string {
	if fileExists(path) {
		return "found"
	}
	return "not found"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// declaredProviders lists, in lexical order, the providers the settings section
// declares — including a key-only entry that declares no models[], which the
// merged catalog alone could never show. Built-in providers with no
// declaration are not listed: the catalog is not what this section is about.
func declaredProviders(providers map[string]models.ProviderConfig) []string {
	out := make([]string, 0, len(providers))
	for id := range providers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// providerBaseURL is the URL a run would use for this provider: the entry's own
// baseUrl when it declares one, else the first (default) catalog entry's.
func providerBaseURL(snap *models.Snapshot, provider ai.ProviderId, cfg models.ProviderConfig) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	if m, ok := snap.DefaultModel(provider); ok && m.BaseURL != "" {
		return m.BaseURL
	}
	return "-"
}

// providerModelCount counts the merged catalog entries for a provider (built-in
// plus the declaration's upserts).
func providerModelCount(snap *models.Snapshot, provider ai.ProviderId) int {
	n := 0
	for _, m := range snap.Catalog() {
		if m.Provider == provider {
			n++
		}
	}
	return n
}

// providerAPI names the dialect in play for a declared provider: the entry's
// own api when it sets one, else the first catalog entry's — the same fallback
// chain modelFromJSON resolved with.
func providerAPI(snap *models.Snapshot, provider ai.ProviderId, cfg models.ProviderConfig) string {
	if cfg.API != "" {
		return cfg.API
	}
	if m, ok := snap.DefaultModel(provider); ok {
		return string(m.API)
	}
	return "-"
}

// providerModelIDs lists the merged catalog ids for a provider — what the
// picker will actually offer for it.
func providerModelIDs(snap *models.Snapshot, provider ai.ProviderId) []string {
	var out []string
	for _, m := range snap.Catalog() {
		if m.Provider == provider {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// credentialSource names whether the provider carries its own key. There is no
// second source to report (D11): a provider without one fails fast at startup,
// so "none" here is always an actionable answer.
func credentialSource(cfg models.ProviderConfig) string {
	if cfg.APIKey != "" {
		return "settings"
	}
	return "none"
}

// credentialKey returns the masked key from whichever source has one, or "-".
func credentialKey(cfg models.ProviderConfig) string {
	if cfg.APIKey == "" {
		return "-"
	}
	return maskKey(cfg.APIKey)
}

// maskKey hides a credential's body. The field name is shaped to hit
// maskIfSecret's secret patterns (anything ending in _KEY), so the masking rule
// stays in that one function; a credential is always masked, whatever the field
// is called and whatever prefix it happens to use.
func maskKey(value string) string {
	return maskIfSecret("api_key", value)
}
