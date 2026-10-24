package main

// Tests for the two auth.json layers (provider-config-plan.md P3, D1):
// the user layer at <ConfigHome>/auth.json, the project layer at
// <cwd>/.ultrac/auth.json, project winning per provider id — and the
// degradation rules: a broken file is a warning, never a stop, and a
// per-provider problem (an oauth block) costs that provider only.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAuthStackTwoLayers(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	// The user layer carries two providers.
	writeFile(t, filepath.Join(home, "auth.json"), `{
      "user-a": { "type": "api_key", "key": "sk-user-a" },
      "user-b": { "type": "api_key", "key": "sk-user-b" }
    }`)
	// The project layer replaces user-a wholesale (D1) and leaves user-b
	// alone.
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "auth.json"), `{
      "user-a": { "type": "api_key", "key": "sk-project-a" }
    }`)

	stack := loadAuthStack(cwd, io.Discard)

	if got := stack.APIKey("user-a"); got != "sk-project-a" {
		t.Errorf("user-a key = %q, want the project layer's", got)
	}
	if got := stack.APIKey("user-b"); got != "sk-user-b" {
		t.Errorf("user-b key = %q, want the user layer's (an unrelated provider must survive)", got)
	}
	if probs := stack.Problems(); len(probs) != 0 {
		t.Errorf("problems = %v, want none", probs)
	}
}

func TestLoadAuthStackProjectLayerOnly(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "auth.json"),
		`{"solo": {"type": "api_key", "key": "sk-project-only"}}`)

	stack := loadAuthStack(cwd, io.Discard)
	if got := stack.APIKey("solo"); got != "sk-project-only" {
		t.Fatalf("solo key = %q, want the project layer's", got)
	}
}

func TestLoadAuthStackNoFilesIsEmpty(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	stack := loadAuthStack(cwd, io.Discard)
	if len(stack.Providers()) != 0 {
		t.Errorf("providers = %v, want none (no auth.json is the normal case)", stack.Providers())
	}
	if probs := stack.Problems(); len(probs) != 0 {
		t.Errorf("problems = %v, want none", probs)
	}
}

func TestLoadAuthStackBrokenFileWarnsAndDegrades(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// A broken user layer (an editor's half-save) and a good project
	// layer: the user layer is a warning, the project layer still
	// loads, and no provider loses its credential over it.
	writeFile(t, filepath.Join(home, "auth.json"), `{"user-a": {"type"`)
	if err := os.MkdirAll(filepath.Join(cwd, ".ultrac"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".ultrac", "auth.json"),
		`{"user-a": {"type": "api_key", "key": "sk-project-a"}}`)

	var stderr strings.Builder
	stack := loadAuthStack(cwd, &stderr)
	if !strings.Contains(stderr.String(), "auth.json") {
		t.Errorf("stderr = %q, want a warning naming the broken file", stderr.String())
	}
	if got := stack.APIKey("user-a"); got != "sk-project-a" {
		t.Errorf("user-a key = %q, want the project layer's", got)
	}
}

func TestLoadAuthStackReportsProviderProblems(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// An oauth block (D4: the schema accepts it, the credential chain
	// refuses it) beside a good entry: the oauth provider loses its
	// credential with a named problem, the good one is untouched.
	writeFile(t, filepath.Join(home, "auth.json"), `{
      "wants-oauth": { "type": "oauth", "access": "tok" },
      "fine": { "type": "api_key", "key": "sk-fine" }
    }`)

	var stderr strings.Builder
	stack := loadAuthStack(cwd, &stderr)
	// File-level failures warn through the callback; per-provider
	// problems ride on the stack for the caller to print (assemble.go),
	// so the stream stays quiet here.
	_ = stderr

	if got := stack.APIKey("wants-oauth"); got != "" {
		t.Errorf("wants-oauth key = %q, want empty (an oauth credential is refused)", got)
	}
	if _, ok := stack.Entry("wants-oauth"); ok {
		t.Error("an oauth provider must not carry a usable entry")
	}
	probs := stack.Problems()
	if len(probs) != 1 {
		t.Fatalf("problems = %v, want exactly one", probs)
	}
	if !strings.Contains(probs[0].Error(), "wants-oauth") || !strings.Contains(probs[0].Error(), "oauth") {
		t.Errorf("problem = %v, want it to name the provider and the oauth refusal", probs[0])
	}
	// The good entry survives.
	if got := stack.APIKey("fine"); got != "sk-fine" {
		t.Errorf("fine key = %q, want sk-fine", got)
	}
}
