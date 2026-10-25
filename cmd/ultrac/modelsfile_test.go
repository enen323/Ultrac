package main

// Tests for the settings → catalog-snapshot step.
//
// The snapshot is built from `settings.providers` and nothing else: the
// models.json / auth.json readers (and the migration command that replaced
// them) are gone, so there is no second source to test for precedence. What is
// worth pinning down is the degradation rule — a provider whose entry does not
// compose must surface as a snapshot problem, not as a panic or as silence —
// and that an empty section still yields the built-in catalog.

import (
	"os"
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/config"
	"ultrac/internal/models"
)

// writeFile is shared by the package's tests.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotFromSettingsEmptyIsBuiltin(t *testing.T) {
	snap := snapshotFromSettings(config.Settings{})
	if len(snap.Catalog()) != len(ai.Catalog()) {
		t.Errorf("catalog = %d entries, want the built-in %d", len(snap.Catalog()), len(ai.Catalog()))
	}
	if probs := snap.Problems(); len(probs) != 0 {
		t.Errorf("problems = %v, want none", probs)
	}
}

func TestSnapshotFromSettingsCarriesDeclaredProviders(t *testing.T) {
	snap := snapshotFromSettings(config.Settings{Providers: map[string]models.ProviderConfig{
		"my-proxy": {
			BaseURL: "https://proxy.example.com/v1",
			API:     "openai-completions",
			APIKey:  "sk-x",
			Models:  []models.ModelDefinition{{ID: "my-model"}},
		},
	}})

	m, ok := snap.Lookup("my-proxy", "my-model")
	if !ok {
		t.Fatal("my-proxy/my-model missing from the snapshot")
	}
	if m.BaseURL != "https://proxy.example.com/v1" || m.API != ai.APIOpenAICompletions {
		t.Errorf("entry = %+v, want the declared endpoint and dialect", m)
	}
	// The built-in catalog survives next to it.
	if _, ok := snap.Lookup(ai.ProviderDeepseek, "deepseek-chat"); !ok {
		t.Error("the built-in catalog must survive alongside a declaration")
	}
}

func TestSnapshotFromSettingsReportsProviderProblems(t *testing.T) {
	// An empty provider entry passes the schema and fails at merge time (pi's
	// empty-entry rule): it must surface as a snapshot problem, not as a panic
	// and not as silence.
	snap := snapshotFromSettings(config.Settings{Providers: map[string]models.ProviderConfig{
		"empty-one": {},
	}})

	probs := snap.Problems()
	if len(probs) != 1 {
		t.Fatalf("problems = %v, want exactly one", probs)
	}
	if !strings.Contains(probs[0].Error(), "empty-one") {
		t.Errorf("problem = %v, want it to name the provider", probs[0])
	}
	// The snapshot is still usable for everything else.
	if _, ok := snap.Lookup(ai.ProviderDeepseek, "deepseek-chat"); !ok {
		t.Error("the built-in catalog must survive a provider problem")
	}
	// models.Build with an empty config carries no problems, proving the
	// problem lives in the merge and not in the load.
	if len(models.Build().Problems()) != 0 {
		t.Error("an empty build must carry no problems")
	}
}
