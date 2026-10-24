package main

// Tests for the models.json / auth.json section of `ultrac config`
// (provider-config-plan.md P4, §4.7): both paths per file, the
// per-provider summary with its credential source, the masked key, and
// the .gitignore reminders for project-level files that carry secrets.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintProviderConfigSection(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	// User layer: one provider with its own apiKey.
	writeFile(t, filepath.Join(home, "models.json"), `{
      "providers": {
        "from-models": {
          "baseUrl": "https://a.example.com/v1",
          "api": "openai-completions",
          "apiKey": "sk-super-secret-models-key",
          "models": [
            { "id": "m1", "cost": { "input": 1, "output": 2, "cacheRead": 0.1, "cacheWrite": 1 } },
            { "id": "m2", "cost": { "input": 1, "output": 2, "cacheRead": 0.1, "cacheWrite": 1 } }
          ]
        }
      }
    }`)
	// User layer auth.json: a key for a built-in provider (deepseek),
	// which declares no models.json entry.
	writeFile(t, filepath.Join(home, "auth.json"),
		`{"deepseek": {"type": "api_key", "key": "sk-deepseek-auth-key"}}`)

	// Project layer: a provider with no credential anywhere, and a
	// project auth.json carrying one for it.
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "models.json"), `{
      "providers": {
        "no-key": {
          "baseUrl": "https://b.example.com/v1",
          "api": "openai-completions",
          "models": [
            { "id": "n1", "cost": { "input": 1, "output": 2, "cacheRead": 0.1, "cacheWrite": 1 } }
          ]
        }
      }
    }`)
	writeFile(t, filepath.Join(cwd, ".ultrac", "auth.json"),
		`{"no-key": {"type": "api_key", "key": "sk-project-no-key-key"}}`)

	var stdout, stderr strings.Builder
	printProviderConfigSection(&stdout, &stderr, cwd)
	out := stdout.String()

	// Both paths per file, labelled.
	for _, want := range []string{
		"models.json:",
		"user layer:    " + filepath.Join(home, "models.json") + " (found)",
		"project layer: " + filepath.Join(cwd, ".ultrac", "models.json") + " (found)",
		"auth.json:",
		"user layer:    " + filepath.Join(home, "auth.json") + " (found)",
		"project layer: " + filepath.Join(cwd, ".ultrac", "auth.json") + " (found)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// The per-provider summary: baseUrl, model count, credential source.
	if !strings.Contains(out, "from-models: baseUrl=https://a.example.com/v1, models=2, credential=models.json,") {
		t.Errorf("from-models summary wrong:\n%s", out)
	}
	if !strings.Contains(out, "no-key: baseUrl=https://b.example.com/v1, models=1, credential=auth.json,") {
		t.Errorf("no-key summary wrong:\n%s", out)
	}
	// The masked key: enough to recognise, not enough to use.
	if !strings.Contains(out, "key=sk-sup***") {
		t.Errorf("the models.json key must be masked, not dropped:\n%s", out)
	}
	// The key bodies must not reach the terminal.
	for _, secret := range []string{
		"sk-super-secret-models-key",
		"sk-deepseek-auth-key",
		"sk-project-no-key-key",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks the credential %q:\n%s", secret, out)
		}
	}
	// The .gitignore reminders: the project auth.json always, the
	// project models.json because a provider carries an apiKey.
	if !strings.Contains(out, `add ".ultrac/auth.json" to .gitignore`) {
		t.Errorf("the project auth.json must prompt .gitignore:\n%s", out)
	}
	if !strings.Contains(out, `add ".ultrac/models.json" to .gitignore`) {
		t.Errorf("a project models.json with an apiKey must prompt .gitignore:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want no warnings for healthy files", stderr.String())
	}
}

func TestPrintProviderConfigSectionNoFiles(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	var stdout, stderr strings.Builder
	printProviderConfigSection(&stdout, &stderr, cwd)
	out := stdout.String()

	// ENOENT is the normal case: the paths are still shown (that is the
	// point of the section — "where would I put the file"), marked not
	// found, with no providers and no .gitignore noise.
	for _, want := range []string{
		filepath.Join(home, "models.json") + " (not found)",
		filepath.Join(cwd, ".ultrac", "models.json") + " (not found)",
		filepath.Join(home, "auth.json") + " (not found)",
		"providers: (none declared)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".gitignore") {
		t.Errorf("no project-level secret files exist; .gitignore note is noise:\n%s", out)
	}
}

// TestPrintProviderConfigSectionMasksOnlySecrets — a project models.json
// with NO apiKey gets no .gitignore note (it is a config file like any
// other), while the auth.json beside it still does.
func TestPrintProviderConfigSectionMasksOnlySecrets(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "models.json"), `{
      "providers": {
        "plain": {
          "baseUrl": "https://b.example.com/v1",
          "api": "openai-completions",
          "models": [
            { "id": "n1", "cost": { "input": 1, "output": 2, "cacheRead": 0.1, "cacheWrite": 1 } }
          ]
        }
      }
    }`)
	writeFile(t, filepath.Join(cwd, ".ultrac", "auth.json"),
		`{"plain": {"type": "api_key", "key": "sk-project-plain-key"}}`)

	var stdout, stderr strings.Builder
	printProviderConfigSection(&stdout, &stderr, cwd)
	out := stdout.String()

	if strings.Contains(out, `add ".ultrac/models.json" to .gitignore`) {
		t.Errorf("a models.json without apiKey is not a secret file:\n%s", out)
	}
	if !strings.Contains(out, `add ".ultrac/auth.json" to .gitignore`) {
		t.Errorf("the project auth.json must prompt .gitignore:\n%s", out)
	}
	if !strings.Contains(out, "plain: baseUrl=https://b.example.com/v1, models=1, credential=auth.json, key=sk-pro***") {
		t.Errorf("plain summary wrong:\n%s", out)
	}
}
