// assemble_test.go — the shared harness assembly (P6, plan §3.7).
//
// What is under test: the per-prompt sections patch lifecycle (the diff pi
// recomputes per prompt), the user message with images, the model list
// set_model draws from, and the --cwd resolution both modes share. The
// stream is faked the same way print_test.go fakes it, so nothing here
// reaches the network.

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ultrac/internal/ai"
	"ultrac/internal/prompt"
	"ultrac/internal/rpc"
	"ultrac/internal/session"
)

// testHarness assembles a harness over the print sandbox (settings home +
// working directory) with the fake provider wired in.
func testHarness(t *testing.T) *harness {
	t.Helper()
	env := newPrintEnv(t)
	h, code := buildHarness(env.flags, env.cwd, io.Discard, env.provider.streamFn)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	return h
}

func TestBuildPromptPatchLifecycle(t *testing.T) {
	h := testHarness(t)
	mgr := session.InMemorySession(h.cwd)
	a := h.newAgent(mgr, session.SessionContext{}, false)

	// First prompt: the transcript declares no sections yet, so the patch
	// carries the whole sectioned prompt.
	in1, err := h.buildPrompt(a, "first", nil)
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if in1.Patch == nil {
		t.Fatal("the first prompt must carry a sections patch")
	}
	if in1.Patch.Sections[prompt.SectionPreamble] == nil {
		t.Errorf("patch sections = %v, want the preamble declared", in1.Patch.Sections)
	}
	if ai.MessageText(in1.User) != "first" {
		t.Errorf("user message = %q", ai.MessageText(in1.User))
	}

	// Replay the first run into a resumed agent: patch, user, assistant.
	assistant := ai.Message{
		Role:        ai.RoleAssistant,
		Content:     []ai.ContentBlock{ai.TextBlock("answer")},
		TimestampMS: time.Now().UnixMilli(),
	}
	h.seed = session.SessionContext{Messages: []ai.Message{*in1.Patch, in1.User, assistant}}
	a2 := h.newAgent(mgr, h.seed, true)

	// Second prompt, nothing changed: the diff is empty and no system
	// message is appended (pi: system-prompt.ts:217-229).
	in2, err := h.buildPrompt(a2, "second", nil)
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if in2.Patch != nil {
		t.Errorf("second patch = %v, want nil (sections unchanged)", in2.Patch.Sections)
	}

	// A context file edited mid-session changes the sections, so the next
	// prompt patches again — that is the point of recomputing per prompt.
	if err := os.WriteFile(filepath.Join(h.cwd, "AGENTS.md"), []byte("# project rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in3, err := h.buildPrompt(a2, "third", nil)
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if in3.Patch == nil {
		t.Error("a changed context file must produce a patch")
	}
}

func TestUserMessageWithImages(t *testing.T) {
	got := userMessageWith("what is this", []rpc.ImageContent{
		{Type: "image", Data: "aGVsbG8=", MimeType: "image/png"},
	})
	if got.Role != ai.RoleUser {
		t.Errorf("role = %q, want user", got.Role)
	}
	if got.TimestampMS == 0 {
		t.Error("the timestamp pi records must be set (agent-session.ts:1912-1916)")
	}
	if len(got.Content) != 2 {
		t.Fatalf("blocks = %d, want 2 (text + image)", len(got.Content))
	}
	if got.Content[0].Type != ai.BlockText || got.Content[0].Text != "what is this" {
		t.Errorf("text block = %+v", got.Content[0])
	}
	if got.Content[1].Type != ai.BlockImage || got.Content[1].Data != "aGVsbG8=" || got.Content[1].MimeType != "image/png" {
		t.Errorf("image block = %+v", got.Content[1])
	}
}

func TestAvailableModelsCarriesDeclaredProviders(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	writeFile(t, filepath.Join(home, "settings.json"), `{
      "providers": {
        "my-proxy": {
          "baseUrl": "https://proxy.example.com/v1",
          "api": "openai-completions",
          "apiKey": "sk-x",
          "models": [{"id": "my-model"}]
        }
      }
    }`)

	h, code := buildHarness(&agentFlags{Mode: "text", Model: "my-proxy/my-model"}, cwd, io.Discard, nil)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	// The picker's list is the merged catalog: a declared provider's model is
	// in it, and the built-ins are too.
	found := false
	for _, m := range h.availableModels() {
		if m.Provider == ai.ProviderId("my-proxy") && m.ID == "my-model" {
			found = true
		}
	}
	if !found {
		t.Errorf("declared model missing from %d available entries", len(h.availableModels()))
	}
	if !containsModel(h.availableModels(), ai.WithDefaults(ai.Model{
		Provider: ai.ProviderDeepseek, ID: "deepseek-chat",
	})) {
		t.Error("the built-in catalog must survive")
	}
}

// TestHarnessCredentialComesOnlyFromTheProviderEntry — D11 at the harness
// level: the env block still carrying ANTHROPIC_AUTH_TOKEN changes nothing. A
// provider without its own key resolves to "", which is what the startup
// fail-fast and the per-request GetAPIKey both see.
func TestHarnessCredentialComesOnlyFromTheProviderEntry(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	writeFile(t, filepath.Join(home, "settings.json"), `{
      "env": {
        "ANTHROPIC_AUTH_TOKEN": "sk-env-token",
        "ANTHROPIC_MODEL": "deepseek-chat",
        "ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic"
      },
      "providers": {
        "with-key": {
          "baseUrl": "https://a.example.com/v1",
          "api": "openai-completions",
          "apiKey": "sk-with-key",
          "models": [{"id": "m1"}]
        },
        "no-key": {
          "baseUrl": "https://b.example.com/v1",
          "api": "openai-completions",
          "models": [{"id": "m2"}]
        }
      }
    }`)

	h, code := buildHarness(&agentFlags{Mode: "text", Model: "with-key/m1"}, cwd, io.Discard, nil)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	if got := h.credentialFor("with-key"); got != "sk-with-key" {
		t.Errorf("credentialFor(with-key) = %q, want the declared key", got)
	}
	// No fallback of any kind: not the env token, not another provider's key.
	if got := h.credentialFor("no-key"); got != "" {
		t.Errorf("credentialFor(no-key) = %q, want empty (no env fallback)", got)
	}
	if got := h.credentialFor("deepseek"); got != "" {
		t.Errorf("credentialFor(deepseek) = %q, want empty even though env has a token", got)
	}
	if got := h.credentialFor(string(h.model.Provider)); got != "sk-with-key" {
		t.Errorf("the startup model's provider = %q, want the declared key", got)
	}
}

// TestBuildHarnessNoModelIsCliError — with no --model and no session model,
// a CLI run exits 2 with the guidance that names the only source.
func TestBuildHarnessNoModelIsCliError(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// An env block naming a model must not rescue this: it is not a source
	// any more (D11).
	writeFile(t, filepath.Join(home, "settings.json"), `{
      "env": {"ANTHROPIC_MODEL": "deepseek-chat", "ANTHROPIC_AUTH_TOKEN": "sk-env"}
    }`)

	var stderr strings.Builder
	h, code := buildHarness(&agentFlags{Mode: "text"}, cwd, &stderr, nil)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (no model named)", code)
	}
	if h != nil {
		t.Error("no harness should be returned without a model")
	}
	msg := stderr.String()
	if !strings.Contains(msg, "no model configured") {
		t.Errorf("stderr = %q, want the no-model wording", msg)
	}
	if !strings.Contains(msg, "--model") || !strings.Contains(msg, `"providers"`) {
		t.Errorf("stderr = %q, want it to name both sources", msg)
	}
}

// TestBuildHarnessNoModelKeepsRPCAlive — the same situation in RPC mode is a
// state, not a failure: the desktop shell needs a live kernel to show a model
// picker, so the harness comes up with an empty model and cmdPrompt answers
// "no model configured" until set_model arrives.
func TestBuildHarnessNoModelKeepsRPCAlive(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	writeFile(t, filepath.Join(home, "settings.json"), `{}`)

	var stderr strings.Builder
	h, code := buildHarness(&agentFlags{Mode: "rpc"}, cwd, &stderr, nil)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (RPC mode tolerates no model)", code)
	}
	if h == nil {
		t.Fatal("a harness must come up so a client can still select a model")
	}
	if h.model.ID != "" {
		t.Errorf("model = %+v, want the empty model", h.model)
	}
	if len(h.availableModels()) == 0 {
		t.Error("the built-in catalog must still be offered by the picker")
	}
}

// TestBuildHarnessFailsFastPerProvider — a model whose provider declares no
// credential exits 1 with guidance that names the provider and the one place a
// key can live.
func TestBuildHarnessFailsFastPerProvider(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// No providers at all, but the model is named on the command line: it
	// resolves from the built-in catalog and has no key.
	writeFile(t, filepath.Join(home, "settings.json"), `{"env": {}}`)

	var stderr strings.Builder
	h, code := buildHarness(&agentFlags{Mode: "text", Model: "deepseek/deepseek-chat"}, cwd, &stderr, nil)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (no credential anywhere)", code)
	}
	if h != nil {
		t.Error("no harness should be returned on the credential fail-fast")
	}
	msg := stderr.String()
	if !strings.Contains(msg, `no credential is configured for provider "deepseek"`) {
		t.Errorf("stderr = %q, want the per-provider wording", msg)
	}
	// The guidance names the one place a key can now live, and says the env
	// fallback is gone (settings-unify-plan.md D4/D11).
	if !strings.Contains(msg, `"providers"`) || !strings.Contains(msg, "settings.json") {
		t.Errorf("stderr = %q, want the providers-section guidance", msg)
	}
	if strings.Contains(msg, "auth.json") {
		t.Errorf("stderr = %q, still points at the deleted auth.json path", msg)
	}
}

func TestPersistableRoles(t *testing.T) {
	// pi's set (agent-session.ts:1158-1163): system, user, assistant,
	// toolResult.
	for _, r := range []ai.Role{ai.RoleSystem, ai.RoleUser, ai.RoleAssistant, ai.RoleToolResult} {
		if !persistable(r) {
			t.Errorf("persistable(%q) = false, want true", r)
		}
	}
	// Every other role — including the two the wire has no constant for —
	// stays out of the file.
	for _, r := range []ai.Role{"summary", ""} {
		if persistable(r) {
			t.Errorf("persistable(%q) = true, want false", r)
		}
	}
}

func TestResolveCWD(t *testing.T) {
	f := &agentFlags{}
	got, code := resolveCWD(f, "/tmp", io.Discard)
	if code != 0 || got != "/tmp" {
		t.Errorf("no --cwd: got (%q, %d), want (/tmp, 0)", got, code)
	}

	f.CWD = filepath.Join(t.TempDir(), "missing")
	if _, code := resolveCWD(f, "/tmp", io.Discard); code != 2 {
		t.Errorf("a missing --cwd dir: code = %d, want 2", code)
	}

	dir := t.TempDir()
	got, code = resolveCWD(&agentFlags{CWD: dir}, "/tmp", io.Discard)
	if code != 0 || got != dir {
		t.Errorf("a real --cwd dir: got (%q, %d), want (%q, 0)", got, code, dir)
	}
}
