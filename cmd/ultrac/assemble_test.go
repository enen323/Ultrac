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
	resolved, err := resolveModel("", "", h.effective)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if !containsModel(models, resolved) {
		t.Errorf("settings model %s/%s missing from %d entries", resolved.Provider, resolved.ID, len(models))
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
