package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func base(dir string) Options {
	return Options{
		WorkDir:          dir,
		Model:            "test-model",
		Version:          "1.2.3",
		Tools:            []string{"Read", "Glob", "Grep", "Bash", "Write", "Edit", "TodoWrite", "Agent"},
		IsNonInteractive: true,
		Now:              fixedNow,
	}
}

func TestBuildIncludesTheEssentials(t *testing.T) {
	dir := t.TempDir()
	got := Build(base(dir))

	for _, want := range []string{
		"software engineering tasks", // identity
		"# System",                   // system section
		"# Doing tasks",              // behaviour
		"# Executing actions with care",
		"# Using your tools",
		"# Tone and style",
		"# Output efficiency",
		"# Environment",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	// The environment section has to be accurate, because the model uses it
	// instead of spending a tool call to find out where it is.
	if !strings.Contains(got, dir) {
		t.Errorf("working directory missing from the prompt")
	}
	if !strings.Contains(got, "2026-10-08") {
		t.Errorf("date missing from the prompt")
	}
	if !strings.Contains(got, "test-model") {
		t.Errorf("model missing from the prompt")
	}
	if !strings.Contains(got, "1.2.3") {
		t.Errorf("version missing from the prompt")
	}
}

// TestMentionsOnlyAvailableTools is the property that keeps the prompt
// honest: naming a tool the engine does not have teaches the model to call it
// and fail.
func TestMentionsOnlyAvailableTools(t *testing.T) {
	dir := t.TempDir()
	withBash := Build(base(dir))
	if !strings.Contains(withBash, "TodoWrite") {
		t.Error("TodoWrite guidance missing although the tool is present")
	}
	if !strings.Contains(withBash, "Agent") {
		t.Error("Agent guidance missing although the tool is present")
	}

	noTools := base(dir)
	noTools.Tools = []string{"Read"}
	got := Build(noTools)
	if strings.Contains(got, "TodoWrite") {
		t.Error("the prompt names TodoWrite although the tool is absent")
	}
	if strings.Contains(got, "the Bash tool") {
		t.Error("the prompt names Bash although the tool is absent")
	}
}

func TestEnvironmentOmitsUnknownValues(t *testing.T) {
	dir := t.TempDir()
	opts := base(dir)
	opts.Model = ""
	opts.Version = ""
	got := Build(opts)
	if strings.Contains(got, "Model:") {
		t.Error("an empty model was still reported")
	}
	if strings.Contains(got, "Engine version:") {
		t.Error("an empty version was still reported")
	}
}

func TestProjectContextIsIncluded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# House rules\nAlways write tests.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(dir, ".ultrac", "rules")
	if err := os.MkdirAll(rules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rules, "go.md"), []byte("Use gofmt."), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Build(base(dir))
	if !strings.Contains(got, "Always write tests.") {
		t.Error("CLAUDE.md was not included")
	}
	if !strings.Contains(got, "Use gofmt.") {
		t.Error("a project rule was not included")
	}
	// The path is part of the message: the model needs to know where the
	// instruction came from when the user asks why it did something.
	if !strings.Contains(got, "CLAUDE.md") {
		t.Error("the instruction file is not identified")
	}
}

func TestProjectContextAbsentWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	got := Build(base(dir))
	if strings.Contains(got, "project instructions") {
		t.Error("an empty directory produced project context")
	}
	// An empty file must not produce a section either.
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(Build(base(dir)), "project instructions") {
		t.Error("a whitespace-only CLAUDE.md produced project context")
	}
}

func TestOverrideReplacesEverything(t *testing.T) {
	dir := t.TempDir()
	opts := base(dir)
	opts.OverrideSystemPrompt = "just this"
	opts.AppendSystemPrompt = "and not this"
	got := Build(opts)
	if got != "just this" {
		t.Fatalf("override did not replace the prompt: %q", got)
	}
}

func TestCustomPromptKeepsAppend(t *testing.T) {
	dir := t.TempDir()
	opts := base(dir)
	opts.CustomSystemPrompt = "custom base"
	opts.AppendSystemPrompt = "appended tail"
	got := Build(opts)
	if !strings.Contains(got, "custom base") {
		t.Error("custom prompt missing")
	}
	if !strings.Contains(got, "appended tail") {
		t.Error("append was lost when a custom prompt was set")
	}
	if strings.Contains(got, "# Doing tasks") {
		t.Error("the built-in sections survived a custom prompt")
	}
	// The environment is not part of the "built-in sections" and is still
	// needed — a custom prompt is a replacement for the guidance, not for the
	// facts about where the session is running.
	if !strings.Contains(got, "# Environment") {
		t.Error("environment section lost with a custom prompt")
	}
}

func TestDefaultAppendApplies(t *testing.T) {
	dir := t.TempDir()
	opts := base(dir)
	opts.AppendSystemPrompt = "extra instructions"
	got := Build(opts)
	if !strings.Contains(got, "# Doing tasks") {
		t.Error("the default sections were dropped")
	}
	if !strings.Contains(got, "extra instructions") {
		t.Error("append missing")
	}
}

func TestExtraSectionsAppear(t *testing.T) {
	dir := t.TempDir()
	opts := base(dir)
	opts.ExtraSections = []string{"", "Todo list:\n[ ] Do a thing\n"}
	got := Build(opts)
	if !strings.Contains(got, "Do a thing") {
		t.Error("an extra section was dropped")
	}
}

func TestGitRepoIsDetected(t *testing.T) {
	dir := t.TempDir()
	if strings.Contains(Build(base(dir)), "Is a git repository") {
		t.Error("a plain directory was reported as a git repository")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(Build(base(dir)), "Is a git repository: yes") {
		t.Error(".git was not detected")
	}
}

func TestInteractiveGuidanceIsSuppressed(t *testing.T) {
	dir := t.TempDir()
	nonInteractive := base(dir)
	nonInteractive.IsNonInteractive = true
	if strings.Contains(Build(nonInteractive), "The user may interrupt you") {
		t.Error("non-interactive prompt mentions interrupting")
	}

	interactive := base(dir)
	interactive.IsNonInteractive = false
	if !strings.Contains(Build(interactive), "The user may interrupt you") {
		t.Error("interactive prompt omits interrupt guidance")
	}
}

func TestBuildIsStable(t *testing.T) {
	dir := t.TempDir()
	first := Build(base(dir))
	second := Build(base(dir))
	if first != second {
		t.Error("the prompt is not deterministic for the same inputs")
	}
}

func TestDescribe(t *testing.T) {
	got := Describe(base(t.TempDir()))
	if !strings.Contains(got, "8 tool(s)") {
		t.Errorf("describe = %q", got)
	}
}
