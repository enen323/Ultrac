package models

// models_test.go — the load layer (P0).
//
// What is under test: pi's load semantics ported verbatim — ENOENT is the
// normal case (empty config, no error), and every other failure (read,
// parse, schema) comes back as an error that names the file so the caller
// can downgrade it to a warning. The schema's job beyond that is the
// dotted-path error format: an unknown field or a wrong type must say
// WHICH field, not "invalid document".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write is the sandbox helper: one models.json under a temp dir, its path
// handed to Load.
func write(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidDocument(t *testing.T) {
	path := write(t, `{
  "$schema": "https://ultrac.dev/schemas/models.json",
  "providers": {
    "my-proxy": {
      "name": "My Proxy",
      "baseUrl": "https://proxy.example.com/v1",
      "api": "openai-completions",
      "apiKey": "sk-literal-key",
      "oauth": { "some": "pi leftover" },
      "headers": { "X-Custom": "value" },
      "models": [
        {
          "id": "my-model",
          "name": "My Model",
          "reasoning": true,
          "thinkingLevelMap": { "high": "high", "xhigh": null },
          "input": ["text", "image"],
          "inputLimits": {
            "maxRequestBytes": 1000000,
            "images": {
              "resize": { "maxWidth": 1024, "maxHeight": 1024, "maxBytes": 5000000, "jpegQuality": 85 },
              "maxPerMessage": 5,
              "maxPerRequest": 10
            }
          },
          "cost": {
            "input": 1.0, "output": 2.0, "cacheRead": 0.1, "cacheWrite": 0.3,
            "tiers": [
              { "inputTokensAbove": 100000, "input": 0.5, "output": 1.0, "cacheRead": 0.05, "cacheWrite": 0.15 }
            ]
          },
          "promptCache": { "short": 300 },
          "contextWindow": 128000,
          "maxTokens": 8192,
          "samplingParams": { "temperature": 0.5 },
          "samplingParamsByThinkingLevel": { "high": { "temperature": 0.5 } },
          "headers": { "X-Model": "yes" }
        }
      ],
      "modelOverrides": {
        "my-model": { "maxTokens": 16384, "cost": { "input": 0.5 } }
      }
    }
  }
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := cfg.Providers["my-proxy"]
	if !ok {
		t.Fatal("my-proxy missing from the decoded config")
	}
	if p.BaseURL != "https://proxy.example.com/v1" || p.API != "openai-completions" {
		t.Errorf("provider baseUrl/api = %q/%q", p.BaseURL, p.API)
	}
	if p.APIKey != "sk-literal-key" {
		t.Errorf("apiKey = %q, want the literal value (D5/D6)", p.APIKey)
	}
	if len(p.OAuth) == 0 {
		t.Error("oauth should decode (D4: accepted; the merge layer ignores it)")
	}
	if len(p.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(p.Models))
	}
	m := p.Models[0]
	if m.ID != "my-model" || m.Name != "My Model" {
		t.Errorf("model id/name = %q/%q", m.ID, m.Name)
	}
	if m.Reasoning == nil || !*m.Reasoning {
		t.Error("reasoning should decode as true")
	}
	if m.ContextWindow == nil || *m.ContextWindow != 128000 {
		t.Errorf("contextWindow = %v", m.ContextWindow)
	}
	if m.MaxTokens == nil || *m.MaxTokens != 8192 {
		t.Errorf("maxTokens = %v", m.MaxTokens)
	}
	if m.Cost == nil || m.Cost.Input != 1.0 || len(m.Cost.Tiers) != 1 {
		t.Errorf("cost = %+v", m.Cost)
	}
	if len(m.Input) != 2 || m.Input[0] != "text" {
		t.Errorf("input = %v", m.Input)
	}
	if _, ok := m.ThinkingLevelMap["xhigh"]; !ok || m.ThinkingLevelMap["xhigh"] != nil {
		t.Errorf("thinkingLevelMap[xhigh] = %v, want present-and-null (unsupported)", m.ThinkingLevelMap["xhigh"])
	}
	ov, ok := p.ModelOverrides["my-model"]
	if !ok {
		t.Fatal("modelOverrides[my-model] missing")
	}
	if ov.MaxTokens == nil || *ov.MaxTokens != 16384 {
		t.Errorf("override maxTokens = %v", ov.MaxTokens)
	}
	if ov.Cost == nil || ov.Cost.Input == nil || *ov.Cost.Input != 0.5 {
		t.Errorf("override cost = %+v (partial cost must decode)", ov.Cost)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	// ENOENT is the normal case: no models.json, no user providers.
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("ENOENT should not be an error: %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("providers = %v, want empty", cfg.Providers)
	}
}

func TestLoadEmptyPathIsNotAnError(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("empty path should not be an error: %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("providers = %v, want empty", cfg.Providers)
	}
}

func TestLoadParseFailureNamesTheFile(t *testing.T) {
	path := write(t, `{"providers": {`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("a truncated document must fail")
	}
	if !strings.Contains(err.Error(), "failed to parse models.json") {
		t.Errorf("error = %v, want a parse failure", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want the file path %s", err, path)
	}
}

func TestLoadUnknownFieldRoot(t *testing.T) {
	path := write(t, `{"providers": {}, "bogus": 1}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("an unknown root field must fail the schema")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("error = %v, want a path-bearing unknown-field error", err)
	}
}

func TestLoadUnknownFieldProviderAndModel(t *testing.T) {
	path := write(t, `{
  "providers": {
    "my-proxy": {
      "baseUrl": "https://proxy.example.com/v1",
      "bogus": 1,
      "models": [{ "id": "m", "nope": true }]
    }
  }
}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("unknown fields must fail the schema")
	}
	for _, want := range []string{"providers.my-proxy.bogus", "providers.my-proxy.models.0.nope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %s", err, want)
		}
	}
}

func TestLoadTypeError(t *testing.T) {
	path := write(t, `{"providers": {"p": {"baseUrl": 42}}}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("a wrong type must fail the schema")
	}
	if !strings.Contains(err.Error(), "providers.p.baseUrl") {
		t.Errorf("error = %v, want the field path", err)
	}
	if !strings.Contains(err.Error(), "string") {
		t.Errorf("error = %v, want the expected type", err)
	}
}

func TestLoadProvidersRequired(t *testing.T) {
	path := write(t, `{"$schema": "https://ultrac.dev/schemas/models.json"}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("a document without providers must fail (pi: Type.Object required)")
	}
	if !strings.Contains(err.Error(), "providers") {
		t.Errorf("error = %v, want the missing property named", err)
	}
}

func TestLoadRootMustBeAnObject(t *testing.T) {
	path := write(t, `["not", "an", "object"]`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("an array root must fail")
	}
	if !strings.Contains(err.Error(), "invalid models.json schema") {
		t.Errorf("error = %v, want a schema failure", err)
	}
}

func TestLoadStripsBOM(t *testing.T) {
	// Windows editors write one; encoding/json would reject it with
	// "invalid character 'ï'".
	path := write(t, "\xef\xbb\xbf"+`{"providers": {}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a BOM-prefixed document should load: %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("providers = %v, want empty", cfg.Providers)
	}
}

func TestLoadEmptyProviderEntryPassesSchema(t *testing.T) {
	// An all-fields-absent entry is schema-valid (every field optional);
	// the empty-entry rule is the merge layer's (pi: applyModelsJson), and
	// merge_test.go covers the rejection.
	path := write(t, `{"providers": {"p": {}}}`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}
