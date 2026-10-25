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

func TestAvailableModelsIncludeSettingsModel(t *testing.T) {
	h := testHarness(t)
	models := h.availableModels()
	if len(models) == 0 {
		t.Fatal("availableModels is empty")
	}
	// The settings-resolved model must be selectable even when the
	// handwritten catalog has not caught up (pi's model runtime snapshot
	// works the same way, rpc-mode.ts:471).
	cfg := h.cfg.Load()
	resolved, err := resolveModel(cfg.snapshot, "", "", cfg.effective)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if !containsModel(models, resolved) {
		t.Errorf("settings model %s/%s missing from %d entries", resolved.Provider, resolved.ID, len(models))
	}
}

// TestHarnessCredentialResolvesPerProvider — P3's wiring at the harness
// level: the credential resolver the agent loop calls per request uses
// the provider parameter. models.json's apiKey is step one, auth.json is
// step two, and a provider with neither still gets the settings env
// token — the pre-P3 path, preserved as the last-resort fallback (D6).
// The chain always beats the env token: from-models resolves to its own
// key even though the settings block carries one.
func TestHarnessCredentialResolvesPerProvider(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	writeFile(t, filepath.Join(home, "settings.json"), `{"env": {
		"ANTHROPIC_AUTH_TOKEN": "sk-env-token",
		"ANTHROPIC_MODEL": "deepseek-chat"
	}}`)
	writeFile(t, filepath.Join(home, "models.json"), `{
		"providers": {
			"from-models": {
				"baseUrl": "https://a.example.com/v1",
				"api": "openai-completions",
				"apiKey": "sk-models",
				"models": [
					{"id": "m1", "cost": {"input": 1, "output": 2, "cacheRead": 0.1, "cacheWrite": 1}}
				]
			}
		}
	}`)
	writeFile(t, filepath.Join(home, "auth.json"), `{
		"from-auth": {"type": "api_key", "key": "sk-auth"}
	}`)

	h, code := buildHarness(&agentFlags{Mode: "text"}, cwd, io.Discard, nil)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	if h.cfg.Load() == nil {
		t.Fatal("the harness must carry a resolved config (P3/D4)")
	}
	if got := h.credentialFor("from-models"); got != "sk-models" {
		t.Errorf("credentialFor(from-models) = %q, want the models.json key", got)
	}
	if got := h.credentialFor("from-auth"); got != "sk-auth" {
		t.Errorf("credentialFor(from-auth) = %q, want the auth.json key", got)
	}
	if got := h.credentialFor("deepseek"); got != "sk-env-token" {
		t.Errorf("credentialFor(deepseek) = %q, want the settings env token (the unaffected path)", got)
	}
	if got := h.credentialFor("nobody"); got != "sk-env-token" {
		t.Errorf("credentialFor(nobody) = %q, want the settings env token", got)
	}
	// The startup model resolved through the same resolver, so the
	// fail-fast below could not have fired with these files present.
	if got := h.credentialFor(string(h.model.Provider)); got == "" {
		t.Error("the startup model's provider must resolve to a credential")
	}
}

// TestBuildHarnessFailsFastPerProvider — the P3 startup check: a model
// whose provider has no credential anywhere exits 1 with guidance that
// names the provider and points at the files the chain reads. The old
// "no credential is configured" wording stays as the lead (print_test's
// TestPrintNoCredential asserts on it); the body is the per-provider
// upgrade the plan asks for.
func TestBuildHarnessFailsFastPerProvider(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// No env token, no models.json, no auth.json: the settings block
	// names the model and nothing else.
	writeFile(t, filepath.Join(home, "settings.json"), `{"env": {"ANTHROPIC_MODEL": "deepseek-chat"}}`)

	var stderr strings.Builder
	h, code := buildHarness(&agentFlags{Mode: "text"}, cwd, &stderr, nil)
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
	if !strings.Contains(msg, "auth.json") {
		t.Errorf("stderr = %q, want the auth.json guidance", msg)
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
