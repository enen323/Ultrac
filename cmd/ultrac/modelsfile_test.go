package main

// Tests for the two models.json layers (provider-config-plan.md P1, D1):
// the user layer at <ConfigHome>/models.json, the project layer at
// <cwd>/.ultrac/models.json, project winning per provider id — and the
// degradation rules: a broken file is a warning, never a stop.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/models"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func providerJSON(baseURL string) string {
	return `{
      "providers": {
        "my-proxy": {
          "baseUrl": "` + baseURL + `",
          "api": "openai-completions",
          "models": [
            { "id": "xxx", "name": "Proxy Model",
              "cost": { "input": 1.0, "output": 2.0, "cacheRead": 0.1, "cacheWrite": 1.0 } }
          ]
        }
      }
    }`
}

func TestLoadModelSnapshotTwoLayers(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	// The user layer carries two providers.
	writeFile(t, filepath.Join(home, "models.json"), `{
      "providers": {
        "my-proxy": {
          "baseUrl": "https://user.example.com/v1",
          "api": "openai-completions",
          "models": [
            { "id": "xxx", "cost": { "input": 1.0, "output": 2.0, "cacheRead": 0.1, "cacheWrite": 1.0 } }
          ]
        },
        "other-proxy": {
          "baseUrl": "https://other.example.com/v1",
          "api": "openai-completions",
          "models": [
            { "id": "yyy", "cost": { "input": 1.0, "output": 2.0, "cacheRead": 0.1, "cacheWrite": 1.0 } }
          ]
        }
      }
    }`)
	// The project layer replaces my-proxy wholesale (D1) — different URL,
	// different model id — and leaves other-proxy alone.
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "models.json"), providerJSON("https://project.example.com/v1"))

	snap := loadModelSnapshot(cwd, io.Discard)

	// The project layer won for my-proxy: its URL and its model.
	m, ok := snap.Lookup("my-proxy", "xxx")
	if !ok {
		t.Fatal("my-proxy/xxx missing from the merged snapshot")
	}
	if m.BaseURL != "https://project.example.com/v1" {
		t.Errorf("my-proxy baseUrl = %q, want the project layer's", m.BaseURL)
	}
	if _, ok := snap.Lookup("my-proxy", "user-only"); ok {
		t.Error("a project entry must replace the user entry wholesale, not merge with it")
	}
	// The user layer's other provider survived untouched.
	other, ok := snap.Lookup("other-proxy", "yyy")
	if !ok {
		t.Fatal("other-proxy/yyy missing: an unrelated user provider must survive")
	}
	if other.BaseURL != "https://other.example.com/v1" {
		t.Errorf("other-proxy baseUrl = %q, want the user layer's", other.BaseURL)
	}
	// And the built-in catalog is still in play.
	if _, ok := snap.Lookup(ai.ProviderDeepseek, "deepseek-chat"); !ok {
		t.Error("the built-in catalog must survive the merge")
	}
}

func TestLoadModelSnapshotProjectLayerOnly(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "models.json"), providerJSON("https://project.example.com/v1"))

	snap := loadModelSnapshot(cwd, io.Discard)
	m, ok := snap.Lookup("my-proxy", "xxx")
	if !ok || m.BaseURL != "https://project.example.com/v1" {
		t.Fatalf("project-only layer did not apply: %+v ok=%v", m, ok)
	}
}

func TestLoadModelSnapshotNoFilesIsBuiltin(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	snap := loadModelSnapshot(cwd, io.Discard)
	if len(snap.Catalog()) != len(ai.Catalog()) {
		t.Errorf("catalog = %d entries, want the built-in %d", len(snap.Catalog()), len(ai.Catalog()))
	}
	if probs := snap.Problems(); len(probs) != 0 {
		t.Errorf("problems = %v, want none", probs)
	}
}

func TestLoadModelSnapshotBrokenFileWarnsAndDegrades(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// A broken user layer and a good project layer: the user layer is a
	// warning, the project layer still applies, and the run would have
	// the built-in catalog either way (pi: load failures never stop
	// startup).
	writeFile(t, filepath.Join(home, "models.json"), `{"providers": {`)
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "models.json"), providerJSON("https://project.example.com/v1"))

	var stderr strings.Builder
	snap := loadModelSnapshot(cwd, &stderr)
	if !strings.Contains(stderr.String(), "models.json") {
		t.Errorf("stderr = %q, want a warning naming the broken file", stderr.String())
	}
	m, ok := snap.Lookup("my-proxy", "xxx")
	if !ok || m.BaseURL != "https://project.example.com/v1" {
		t.Fatalf("a broken user layer must not block the project layer: %+v ok=%v", m, ok)
	}
	if _, ok := snap.Lookup(ai.ProviderDeepseek, "deepseek-chat"); !ok {
		t.Error("the built-in catalog must survive a broken layer")
	}
}

func TestLoadModelSnapshotReportsProviderProblems(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// An empty provider entry passes the schema (P0) and fails at merge
	// time (pi's empty-entry rule): it must surface as a snapshot problem,
	// not as a panic and not as silence.
	writeFile(t, filepath.Join(home, "models.json"), `{
      "providers": {
        "empty-one": {}
      }
    }`)

	var stderr strings.Builder
	snap := loadModelSnapshot(cwd, &stderr)
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
	// models.Build with an empty config is the same thing, proving the
	// problem lives in the merge and not in the load.
	if len(models.Build().Problems()) != 0 {
		t.Error("an empty build must carry no problems")
	}
}
