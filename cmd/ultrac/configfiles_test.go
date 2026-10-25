package main

// Tests for the provider section of `ultrac config`
// (settings-unify-plan.md, superseding provider-config-plan.md P4).
//
// Three answers matter and each has a test: what the user layer declares (with
// the key masked), which non-user layers were ignored and why, and the empty
// case. The masked-key assertion is a security test, not a formatting one: an
// unmasked key in a terminal or a pasted log is a leak.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ultrac/internal/config"
)

// settingsLayers loads the real layer stack for cwd, the way `ultrac config`
// does. Using the resolver rather than hand-built Layers keeps the test honest
// about which files back which source.
func settingsLayers(t *testing.T, cwd string) []config.Layer {
	t.Helper()
	r := config.NewFileResolver()
	layers, _ := r.Layers(context.Background(), cwd)
	return layers
}

func TestProviderConfigSectionShowsDeclaredProviders(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	writeFile(t, filepath.Join(home, "settings.json"), `{
      "env": {"ANTHROPIC_MODEL": "deepseek-flash"},
      "hooks": {"Stop": [{"command": "echo done"}]},
      "providers": {
        "api-stepfun-com": {
          "baseUrl": "https://api.stepfun.com/step_plan/v1",
          "api": "openai-completions",
          "apiKey": "6CRS-super-secret-stepfun-key",
          "models": [
            { "id": "step-5-preview", "contextWindow": 128000, "maxTokens": 8192 }
          ]
        }
      }
    }`)

	var stdout, stderr strings.Builder
	printProviderConfigSection(&stdout, &stderr, cwd, settingsLayers(t, cwd))
	out := stdout.String()

	for _, want := range []string{
		"providers (settings.json):",
		"user layer:    " + filepath.Join(home, "settings.json") + " (found)",
		"api-stepfun-com:",
		"baseUrl=https://api.stepfun.com/step_plan/v1",
		"api=openai-completions",
		"models=1",
		"credential=settings",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// The key is masked, and its body never reaches the terminal. maskKey
	// keeps six characters; this key is not "sk-"-prefixed, which is exactly
	// the case a prefix-based mask would have missed.
	if strings.Contains(out, "6CRS-super-secret-stepfun-key") {
		t.Errorf("the full apiKey was printed:\n%s", out)
	}
	if !strings.Contains(out, "6CRS-s") || !strings.Contains(out, "***") {
		t.Errorf("apiKey not masked as expected:\n%s", out)
	}
}

func TestProviderConfigSectionReportsNonUserLayers(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	// A project settings file declaring providers: ignored, and the reason
	// must be printed. Silence here would look like "my provider vanished".
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "settings.json"), `{
      "providers": {
        "leaky": { "baseUrl": "https://leaky.example.com/v1", "apiKey": "sk-project-secret" }
      }
    }`)

	var stdout, stderr strings.Builder
	printProviderConfigSection(&stdout, &stderr, cwd, settingsLayers(t, cwd))
	out := stdout.String()

	if !strings.Contains(out, "ignored (providers must live in the user layer):") {
		t.Errorf("output does not report the ignored layer:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(cwd, ".ultrac", "settings.json")) {
		t.Errorf("output does not name the offending file:\n%s", out)
	}
	if !strings.Contains(out, "leaky") {
		t.Errorf("output does not name the ignored provider:\n%s", out)
	}
	// The project key is a secret and must not be printed, even while being
	// reported as ignored.
	if strings.Contains(out, "sk-project-secret") {
		t.Errorf("the project layer's apiKey leaked into the report:\n%s", out)
	}
}

func TestProviderConfigSectionEmpty(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	var stdout, stderr strings.Builder
	printProviderConfigSection(&stdout, &stderr, cwd, settingsLayers(t, cwd))
	out := stdout.String()

	// ENOENT is the normal case: the path is still shown (that is the point —
	// "where would I put it"), marked not found, with no provider list and no
	// legacy section.
	for _, want := range []string{
		"providers (settings.json):",
		"settings.json (not found)",
		"providers: (none declared)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "legacy provider files") {
		t.Errorf("legacy section printed with no legacy files:\n%s", out)
	}
}

func TestProviderConfigSectionReportsIgnoredEnvKeys(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	// ANTHROPIC_BASE_URL still exists but is a fallback now (D9). Saying so
	// is what stops the next "why is my custom model hitting DeepSeek"
	// debugging session.
	writeFile(t, filepath.Join(home, "settings.json"), `{
      "env": {
        "ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
        "ANTHROPIC_MODEL": "deepseek-flash",
        "ANTHROPIC_AUTH_TOKEN": "sk-env-token"
      },
      "providers": {
        "custom": { "baseUrl": "https://custom.example.com/v1", "apiKey": "k-custom-secret" }
      }
    }`)

	var stdout, stderr strings.Builder
	printProviderConfigSection(&stdout, &stderr, cwd, settingsLayers(t, cwd))
	out := stdout.String()

	if !strings.Contains(out, "ignored env keys") {
		t.Errorf("the ignored env block is not reported:\n%s", out)
	}
	for _, want := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not list %s as ignored:\n%s", want, out)
		}
	}
	// The custom provider is still declared and selectable — the env block
	// being dead does not make the provider dead.
	if !strings.Contains(out, "custom:") {
		t.Errorf("the declared provider is missing:\n%s", out)
	}
}
