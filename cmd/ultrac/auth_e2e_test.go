package main

// The P3 end-to-end acceptance (docs/开发计划/新功能计划/provider-config-plan.md
// §5): the same fake-endpoint shape as the P1 test (models_e2e_test.go),
// but the credential lives in auth.json instead of the settings env
// block — and the settings block carries NO token at all, so the only
// way the request can authenticate is the per-provider chain. What it
// proves is the P3 promise: a key in <ConfigHome>/auth.json reaches the
// wire as the provider's Bearer token, with no code change and no env
// fallback to hide behind.
//
// Nothing is faked but the endpoint: the real openai adapter, the real
// registry, the real harness, the real auth.json loader.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPrintWithAuthJSONCredentialEndToEnd(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	// No settings.json at all — and in particular no ANTHROPIC_AUTH_TOKEN.
	// If the run authenticates, it is the chain that did it.

	// The fake endpoint speaks the Chat Completions dialect and records
	// the Authorization header the chain resolved.
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var wire struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &wire)
		gotModel = wire.Model
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fakeOpenAITextStream("AUTH-OK"))
	}))
	defer srv.Close()

	// The user-layer models.json: one provider, one model, NO apiKey —
	// the credential comes from auth.json.
	modelsJSON := `{
      "providers": {
        "my-proxy": {
          "name": "My Proxy",
          "baseUrl": "` + srv.URL + `",
          "api": "openai-completions",
          "models": [
            { "id": "xxx", "name": "Proxy Model",
              "cost": { "input": 1.0, "output": 2.0, "cacheRead": 0.1, "cacheWrite": 1.0 },
              "contextWindow": 128000, "maxTokens": 8192 }
          ]
        }
      }
    }`
	if err := os.WriteFile(filepath.Join(home, "models.json"), []byte(modelsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// The user-layer auth.json: the literal key (D5/D6 — read as-is,
	// sent as-is).
	if err := os.WriteFile(filepath.Join(home, "auth.json"),
		[]byte(`{"my-proxy": {"type": "api_key", "key": "sk-auth-literal-key"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	flags := &agentFlags{Mode: "text", Model: "my-proxy/xxx"}
	flags.setPrompt("say AUTH-OK")
	ps := &printSession{
		flags:  flags,
		stdout: &stdout,
		stderr: &stderr,
		cwd:    cwd,
		// streamFn nil on purpose: the real registry and the real openai
		// adapter. The only fake in this test is the endpoint.
	}
	if code := ps.run(); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if stdout.String() != "AUTH-OK\n" {
		t.Errorf("stdout = %q, want the fake endpoint's answer", stdout.String())
	}
	// The auth.json literal reached the wire as the provider's Bearer
	// token — resolved by provider id through the chain, not through the
	// (absent) settings env token.
	if gotAuth != "Bearer sk-auth-literal-key" {
		t.Errorf("authorization = %q, want the auth.json key", gotAuth)
	}
	if gotModel != "xxx" {
		t.Errorf("request model = %q, want xxx (the models.json id)", gotModel)
	}
}
