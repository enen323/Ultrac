package main

// The P1 end-to-end acceptance (docs/开发计划/新功能计划/provider-config-plan.md
// §5): a models.json provider wired through the REAL registry
// (streamFn = nil) to a fake OpenAI-compatible endpoint, selected with
// --model my-proxy/xxx and no --api flag — the entry's declared dialect
// is what resolves (§4.3 rule 6).
//
// Nothing is faked but the endpoint: the real openai adapter builds the
// request, the real registry dispatches on the dialect, the real harness
// assembles the stack, the real snapshot merges models.json over the
// built-in catalog. What it proves is the chain the plan promises — a
// user-edited models.json makes an arbitrary OpenAI-compatible endpoint
// usable without recompiling — including the two P1 wirings the unit
// tests cover in isolation: the provider header on the model entry
// reaching the wire, and the settings-env credential reaching it as the
// provider's Bearer token.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintWithDeclaredProviderEndToEnd(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	// The fake endpoint speaks the Chat Completions dialect and records
	// what arrived: the credential, the provider header from models.json,
	// and the model id the merged catalog resolved.
	var gotAuth, gotProxy, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotProxy = r.Header.Get("X-Proxy")
		body, _ := io.ReadAll(r.Body)
		var wire struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &wire)
		gotModel = wire.Model
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fakeOpenAITextStream("PROXY-OK"))
	}))
	defer srv.Close()

	// The user-layer settings: one provider, one model, pointing at the fake
	// endpoint, declaring the openai dialect, its own key and a custom header.
	settingsJSON := `{
      "providers": {
        "my-proxy": {
          "name": "My Proxy",
          "baseUrl": "` + srv.URL + `",
          "api": "openai-completions",
          "apiKey": "sk-e2e-proxy",
          "headers": { "X-Proxy": "ultrac-e2e" },
          "models": [
            { "id": "xxx", "name": "Proxy Model",
              "cost": { "input": 1.0, "output": 2.0, "cacheRead": 0.1, "cacheWrite": 1.0 },
              "contextWindow": 128000, "maxTokens": 8192 }
          ]
        }
      }
    }`
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	flags := &agentFlags{Mode: "text", Model: "my-proxy/xxx"}
	flags.setPrompt("say PROXY-OK")
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
	if stdout.String() != "PROXY-OK\n" {
		t.Errorf("stdout = %q, want the fake endpoint's answer", stdout.String())
	}
	// The provider entry's own apiKey reached the wire as the Bearer token.
	// It has to: there is no env fallback any more
	// (settings-unify-plan.md D11), so a provider that declares no key simply
	// cannot authenticate.
	if gotAuth != "Bearer sk-e2e-proxy" {
		t.Errorf("authorization = %q, want the provider's own apiKey", gotAuth)
	}
	// The provider's custom header rode on the model entry and reached the
	// wire — the adapter wiring P1 verified first.
	if gotProxy != "ultrac-e2e" {
		t.Errorf("X-Proxy = %q, want the provider's header", gotProxy)
	}
	if gotModel != "xxx" {
		t.Errorf("request model = %q, want xxx (the declared id)", gotModel)
	}
}

// fakeOpenAITextStream is a complete, valid single-text-block response in
// the Chat Completions dialect: a role chunk, the content chunks, a finish
// chunk, the usage-only chunk and the sentinel — the shape
// internal/ai/openai's own tests use (openai_test.go's textStream).
func fakeOpenAITextStream(text string) string {
	var b strings.Builder
	frame := func(data string) { b.WriteString("data: " + data + "\n\n") }
	frame(`{"id":"chatcmpl-e2e","model":"proxy-model","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)
	for _, piece := range []string{text[:len(text)/2], text[len(text)/2:]} {
		enc, _ := json.Marshal(piece)
		frame(`{"id":"chatcmpl-e2e","choices":[{"index":0,"delta":{"content":` + string(enc) + `}}]}`)
	}
	frame(`{"id":"chatcmpl-e2e","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	frame(`{"id":"chatcmpl-e2e","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	frame("[DONE]")
	return b.String()
}
