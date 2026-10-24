package main

// configfiles.go — the models.json / auth.json section of `ultrac config`
// (docs/开发计划/新功能计划/provider-config-plan.md P4, §4.7).
//
// # What it answers
//
// "Why did my key not work" has exactly three answers — the provider
// declares no credential, it declares one in a file the run did not read,
// or the file is broken — and all three are visible here or nowhere:
// both file paths per file, every provider the files declare with its
// base URL, model count and WHERE the credential comes from
// (models.json / auth.json / none), the key body masked, and a .gitignore
// reminder for the project-level files that carry secrets.
//
// The loaders are the same ones the run path uses (loadModelSnapshot /
// loadAuthStack), so what this prints is what the next run resolves —
// including the warnings a broken file produces.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"ultrac/internal/ai"
	"ultrac/internal/config/auth"
	"ultrac/internal/models"
)

// printProviderConfigSection renders the section. cwd is the working
// directory the project layers hang off; stderr receives the same
// file-level warnings the run path prints.
func printProviderConfigSection(stdout, stderr io.Writer, cwd string) {
	snap := loadModelSnapshot(cwd, stderr)
	for _, p := range snap.Problems() {
		fmt.Fprintf(stderr, "warning: %v\n", p)
	}
	stack := loadAuthStack(cwd, stderr)
	for _, p := range stack.Problems() {
		fmt.Fprintf(stderr, "warning: %v\n", p)
	}

	// -- models.json ------------------------------------------------------
	fmt.Fprintln(stdout, "models.json:")
	for _, p := range modelsFilePaths(cwd) {
		fmt.Fprintf(stdout, "  %-14s %s (%s)\n", layerLabel(p, cwd), p, existsLabel(p))
	}
	declared := declaredProviders(snap)
	if len(declared) == 0 {
		fmt.Fprintln(stdout, "  providers: (none declared)")
	} else {
		fmt.Fprintln(stdout, "  providers:")
		for _, id := range declared {
			cfg, _ := snap.ProviderConfig(ai.ProviderId(id))
			fmt.Fprintf(stdout, "    %s: baseUrl=%s, models=%d, credential=%s, key=%s\n",
				id, providerBaseURL(snap, ai.ProviderId(id), cfg), providerModelCount(snap, ai.ProviderId(id)),
				credentialSource(cfg, stack, id), credentialKey(cfg, stack, id))
		}
	}

	// -- auth.json --------------------------------------------------------
	fmt.Fprintln(stdout, "auth.json:")
	for _, p := range authFilePaths(cwd) {
		fmt.Fprintf(stdout, "  %-14s %s (%s)\n", layerLabel(p, cwd), p, existsLabel(p))
	}
	// Providers that carry an auth.json credential but declare no
	// models.json entry: the key resolves, but nothing selects the
	// provider by id — usually a typo, worth seeing.
	authOnly := authOnlyProviders(snap, stack)
	if len(authOnly) > 0 {
		fmt.Fprintln(stdout, "  providers with a credential but no models.json entry:")
		for _, id := range authOnly {
			fmt.Fprintf(stdout, "    %s: credential=auth.json, key=%s\n", id, maskKey(stack.APIKey(id)))
		}
	}

	// -- the .gitignore reminders ------------------------------------------
	//
	// Both files can hold a literal key; a project-level one is one `git
	// add .` away from a repository (plan §4.1's security boundary).
	projectAuth := filepath.Join(cwd, ".ultrac", "auth.json")
	if fileExists(projectAuth) {
		fmt.Fprintf(stdout, "  note: %s holds credentials — add \".ultrac/auth.json\" to .gitignore.\n", projectAuth)
	}
	projectModels := filepath.Join(cwd, ".ultrac", "models.json")
	if fileExists(projectModels) && modelsFileHasAPIKey(snap) {
		fmt.Fprintf(stdout, "  note: %s carries a literal apiKey — add \".ultrac/models.json\" to .gitignore.\n", projectModels)
	}
}

// layerLabel names a layer file by its position: "user" for the
// ConfigHome one, "project" for the cwd one. The paths are absolute, so
// the label is what tells the two apart at a glance.
func layerLabel(path, cwd string) string {
	if filepath.Dir(path) == filepath.Join(cwd, ".ultrac") {
		return "project layer:"
	}
	return "user layer:"
}

// existsLabel reports whether a layer file is there. ENOENT is the
// normal case and says so plainly.
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

// declaredProviders lists, in lexical order, the providers the
// models.json files declare (the merged winner per provider, D1).
// Built-in providers without a models.json entry are not listed: the
// catalog is not what this section is about.
func declaredProviders(snap *models.Snapshot) []string {
	var out []string
	for _, p := range snap.Providers() {
		if _, ok := snap.ProviderConfig(p); ok {
			out = append(out, string(p))
		}
	}
	sort.Strings(out)
	return out
}

// authOnlyProviders lists providers with an auth.json credential and no
// models.json entry, in lexical order.
func authOnlyProviders(snap *models.Snapshot, stack *auth.Stack) []string {
	var out []string
	for _, id := range stack.Providers() {
		if _, ok := snap.ProviderConfig(ai.ProviderId(id)); !ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// providerBaseURL is the URL a run would use for this provider: the
// provider entry's own baseUrl when it declares one, else the first
// (default) catalog entry's.
func providerBaseURL(snap *models.Snapshot, provider ai.ProviderId, cfg models.ProviderConfig) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	if m, ok := snap.DefaultModel(provider); ok && m.BaseURL != "" {
		return m.BaseURL
	}
	return "-"
}

// providerModelCount counts the merged catalog entries for a provider
// (built-in plus models.json upserts).
func providerModelCount(snap *models.Snapshot, provider ai.ProviderId) int {
	n := 0
	for _, m := range snap.Catalog() {
		if m.Provider == provider {
			n++
		}
	}
	return n
}

// credentialSource names where the provider's key comes from — the
// chain's order made visible (plan §4.7).
func credentialSource(cfg models.ProviderConfig, stack *auth.Stack, provider string) string {
	switch {
	case cfg.APIKey != "":
		return "models.json"
	case stack.APIKey(provider) != "":
		return "auth.json"
	default:
		return "none"
	}
}

// credentialKey returns the masked key from whichever source has one, or
// "-" when neither does.
func credentialKey(cfg models.ProviderConfig, stack *auth.Stack, provider string) string {
	if cfg.APIKey != "" {
		return maskKey(cfg.APIKey)
	}
	if key := stack.APIKey(provider); key != "" {
		return maskKey(key)
	}
	return "-"
}

// modelsFileHasAPIKey reports whether any declared provider carries a
// literal apiKey — the condition for the models.json .gitignore note.
func modelsFileHasAPIKey(snap *models.Snapshot) bool {
	for _, p := range snap.Providers() {
		if cfg, ok := snap.ProviderConfig(p); ok && cfg.APIKey != "" {
			return true
		}
	}
	return false
}

// maskKey hides a credential's body. The field name is shaped to hit
// maskIfSecret's secret patterns (anything ending in _KEY), so the
// masking rule stays in that one function; a credential is always
// masked, whatever the field is called.
func maskKey(value string) string {
	return maskIfSecret("api_key", value)
}
