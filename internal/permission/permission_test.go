package permission

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"ultrac/internal/contract/tool"
	"ultrac/internal/shell"
)

func TestParseMode(t *testing.T) {
	for _, ok := range []string{"", "default", "acceptEdits", "plan", "bypassPermissions", "dontAsk"} {
		if _, err := ParseMode(ok); err != nil {
			t.Errorf("ParseMode(%q) = %v, want nil", ok, err)
		}
	}
	if _, err := ParseMode("yolo"); err == nil {
		t.Fatal("ParseMode(\"yolo\") accepted an unknown mode")
	}
}

func TestParseRule(t *testing.T) {
	cases := []struct {
		in      string
		tool    string
		content string
	}{
		{"Bash", "Bash", ""},
		{"Bash(git status)", "Bash", "git status"},
		{"Bash(git:*)", "Bash", "git:*"},
		{"Read(//tmp/**)", "Read", "//tmp/**"},
		{"Edit(/a/(b)/c)", "Edit", "/a/(b)/c"},
		{"Task", "Agent", ""}, // legacy rename
		{"Bash(unbalanced", "Bash(unbalanced", ""},
	}
	for _, c := range cases {
		got := ParseRule(c.in, BehaviorAllow, SourceUserSettings)
		if got.Tool != c.tool || got.Content != c.content {
			t.Errorf("ParseRule(%q) = {%q, %q}, want {%q, %q}",
				c.in, got.Tool, got.Content, c.tool, c.content)
		}
	}
}

func TestRuleRoundTrip(t *testing.T) {
	for _, s := range []string{"Bash", "Bash(git status)", "Bash(git:*)", "Edit(//tmp/**)"} {
		got := ParseRule(s, BehaviorAllow, SourceUserSettings).String()
		if got != s {
			t.Errorf("round trip of %q = %q", s, got)
		}
	}
}

func TestRuleMatching(t *testing.T) {
	cases := []struct {
		rule    string
		tool    string
		content string
		want    bool
	}{
		{"Bash", "Bash", "anything at all", true},
		{"Bash", "Read", "/tmp/x", false},
		{"Bash(git status)", "Bash", "git status", true},
		{"Bash(git status)", "Bash", "git status --short", false},
		{"Bash(git:*)", "Bash", "git status", true},
		{"Bash(git:*)", "Bash", "git", true},
		// A prefix rule must stop at a word boundary: `gitx` is a different
		// program, and a bare HasPrefix would approve it.
		{"Bash(git:*)", "Bash", "gitx", false},
		{"Edit(//tmp/**)", "Edit", "//tmp/a/b.txt", true},
		{"Edit(//tmp/**)", "Edit", "//var/a.txt", false},
		{"Edit(/src/*)", "Edit", "/src/a.go", true},
		{"Edit(/src/*)", "Edit", "/src/sub/a.go", false},
		{"Edit(/src/**)", "Edit", "/src/sub/a.go", true},
		{"Edit(**/secrets/**)", "Edit", "secrets/x", true},
		{"Edit(**/secrets/**)", "Edit", "a/b/secrets/x", true},
	}
	for _, c := range cases {
		r := ParseRule(c.rule, BehaviorAllow, SourceUserSettings)
		if got := r.Matches(c.tool, c.content); got != c.want {
			t.Errorf("%q matching %s(%s) = %v, want %v", c.rule, c.tool, c.content, got, c.want)
		}
	}
}

func TestModeBehaviour(t *testing.T) {
	work := t.TempDir()
	editReq := func() Request {
		return Request{
			Tool:    "Write",
			Kind:    KindEdit,
			Content: filepath.Join(work, "a.txt"),
			Paths:   []string{filepath.Join(work, "a.txt")},
		}
	}

	// default: an edit inside the working directory still asks.
	if res := NewEngine(ModeDefault, work).Check(context.Background(), editReq()); res.Decision != tool.DecisionAsk {
		t.Errorf("default mode edit = %s, want ask", res.Decision)
	}

	// acceptEdits: the same edit is approved.
	if res := NewEngine(ModeAcceptEdits, work).Check(context.Background(), editReq()); res.Decision != tool.DecisionAllow {
		t.Errorf("acceptEdits mode edit = %s, want allow", res.Decision)
	}

	// plan: refused outright.
	if res := NewEngine(ModePlan, work).Check(context.Background(), editReq()); res.Decision != tool.DecisionDeny {
		t.Errorf("plan mode edit = %s, want deny", res.Decision)
	}

	// bypassPermissions: approved.
	if res := NewEngine(ModeBypassPermissions, work).Check(context.Background(), editReq()); res.Decision != tool.DecisionAllow {
		t.Errorf("bypass mode edit = %s, want allow", res.Decision)
	}

	// dontAsk: an ask becomes a deny rather than hanging a headless run.
	if res := NewEngine(ModeDontAsk, work).Check(context.Background(), editReq()); res.Decision != tool.DecisionDeny {
		t.Errorf("dontAsk mode edit = %s, want deny", res.Decision)
	}
}

func TestAllowRuleDoesNotApproveCompoundBash(t *testing.T) {
	e := NewEngine(ModeDefault, t.TempDir())
	e.Rules.Add(ParseRule("Bash(git:*)", BehaviorAllow, SourceUserSettings))

	simple := "git status"
	if res := e.Check(context.Background(), Request{
		Tool: "Bash", Kind: KindBash, Content: simple, Bash: shell.Analyze(simple),
	}); res.Decision != tool.DecisionAllow {
		t.Errorf("allow rule on %q = %s, want allow", simple, res.Decision)
	}

	// The same rule must not approve a line that continues with something
	// else. This is the whole reason Compound exists.
	compound := "git status; rm -rf /tmp/x"
	res := e.Check(context.Background(), Request{
		Tool: "Bash", Kind: KindBash, Content: compound, Bash: shell.Analyze(compound),
	})
	if res.Decision == tool.DecisionAllow {
		t.Fatalf("allow rule approved compound command %q", compound)
	}
}

func TestDenyRuleBeatsBypass(t *testing.T) {
	e := NewEngine(ModeBypassPermissions, t.TempDir())
	e.Rules.Add(ParseRule("Bash(rm:*)", BehaviorDeny, SourceUserSettings))
	res := e.Check(context.Background(), Request{
		Tool:    "Bash",
		Kind:    KindBash,
		Content: "rm -rf /",
		Bash:    shell.Analyze("rm -rf /"),
	})
	if res.Decision != tool.DecisionDeny {
		t.Fatalf("deny rule under bypass mode = %s, want deny", res.Decision)
	}
}

func TestPlanModeRefusesWriteDespiteAllowRule(t *testing.T) {
	e := NewEngine(ModePlan, t.TempDir())
	e.Rules.Add(ParseRule("Bash", BehaviorAllow, SourceUserSettings))
	res := e.Check(context.Background(), Request{
		Tool:    "Bash",
		Kind:    KindBash,
		Content: "touch x",
		Bash:    shell.Analyze("touch x"),
	})
	if res.Decision != tool.DecisionDeny {
		t.Fatalf("plan mode with allow rule = %s, want deny", res.Decision)
	}
}

func TestPathValidation(t *testing.T) {
	work := t.TempDir()
	home := t.TempDir()
	cfg := t.TempDir()
	e := NewEngine(ModeAcceptEdits, work)
	e.ConfigDir = cfg

	// Inside the working directory: allowed.
	inDir := Request{
		Tool: "Write", Kind: KindEdit,
		Content: filepath.Join(work, "x.go"),
		Paths:   []string{filepath.Join(work, "x.go")},
	}
	if res := e.Check(context.Background(), inDir); res.Decision != tool.DecisionAllow {
		t.Errorf("write inside workdir = %s, want allow", res.Decision)
	}

	// A sibling directory whose name shares the prefix must not count as
	// inside: /work-other is not under /work.
	sibling := t.TempDir()
	if !within(work, filepath.Join(work, "a")) {
		t.Error("within() rejected a real child")
	}
	if within(work, sibling) {
		t.Error("within() accepted an unrelated directory")
	}

	// Outside the working directories: ask, not allow.
	outside := Request{
		Tool: "Write", Kind: KindEdit,
		Content: filepath.Join(home, "elsewhere.txt"),
		Paths:   []string{filepath.Join(home, "elsewhere.txt")},
	}
	if res := e.Check(context.Background(), outside); res.Decision != tool.DecisionAsk {
		t.Errorf("write outside workdir = %s, want ask", res.Decision)
	}

	// A shell-init file is asked about even inside the working directory,
	// because editing it runs code later.
	rc := Request{
		Tool: "Write", Kind: KindEdit,
		Content: filepath.Join(work, ".bashrc"),
		Paths:   []string{filepath.Join(work, ".bashrc")},
	}
	if res := e.Check(context.Background(), rc); res.Decision != tool.DecisionAsk {
		t.Errorf("write to .bashrc = %s, want ask", res.Decision)
	}
}

// TestRelativeRuleContentMatchesAbsolutePaths covers the shape rules are
// actually written in: settings.json says `Read(go.mod)`, the engine works in
// absolute paths. A rule that silently never matched would be worse than no
// rule at all, because the user believes they have restricted something.
func TestRelativeRuleContentMatchesAbsolutePaths(t *testing.T) {
	work := t.TempDir()
	abs := filepath.Join(work, "go.mod")

	e := NewEngine(ModeDefault, work)
	e.Rules.Add(ParseRule("Read(go.mod)", BehaviorDeny, SourceUserSettings))

	res := e.Check(context.Background(), Request{
		Tool:       "Read",
		Kind:       KindRead,
		Content:    abs,
		ContentAlt: "go.mod",
	})
	if res.Decision != tool.DecisionDeny {
		t.Fatalf("relative deny rule did not match an absolute path: %s", res.Decision)
	}

	// The same rule must not match a differently-named file.
	other := e.Check(context.Background(), Request{
		Tool:       "Read",
		Kind:       KindRead,
		Content:    filepath.Join(work, "other.mod"),
		ContentAlt: "other.mod",
	})
	if other.Decision != tool.DecisionAllow {
		t.Errorf("deny rule leaked onto another file: %s", other.Decision)
	}

	// And an absolute rule still matches the absolute form.
	e2 := NewEngine(ModeDefault, work)
	e2.Rules.Add(ParseRule("Read("+filepath.ToSlash(abs)+")", BehaviorDeny, SourceUserSettings))
	res2 := e2.Check(context.Background(), Request{
		Tool: "Read", Kind: KindRead, Content: abs, ContentAlt: "go.mod",
	})
	if res2.Decision != tool.DecisionDeny {
		t.Errorf("absolute rule did not match: %s", res2.Decision)
	}
}

func TestCanUseUnknownToolAsks(t *testing.T) {
	e := NewEngine(ModeBypassPermissions, t.TempDir())
	canUse := e.CanUse(func(string, json.RawMessage) (Request, bool) { return Request{}, false })
	res, err := canUse(context.Background(), "Mystery", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != tool.DecisionAsk {
		t.Fatalf("unknown tool = %s, want ask", res.Decision)
	}
}

// TestRuleSeparatorInsensitivity pins the cross-platform property: a settings
// file written on one platform has to keep working on the other.
func TestRuleSeparatorInsensitivity(t *testing.T) {
	cases := []struct {
		rule  string
		tool  string
		value string
		want  bool
	}{
		{`Read(src/a.go)`, "Read", `src/a.go`, true},
		{`Read(src/a.go)`, "Read", `src\a.go`, true},
		{`Read(src\a.go)`, "Read", `src/a.go`, true},
		{`Edit(src/**)`, "Edit", `src\a\b.go`, true},
		{`Read(C:/work/a.go)`, "Read", `C:/work/a.go`, true},
		// A rule about Edit must not apply to Read.
		{`Edit(src/a.go)`, "Read", `src/a.go`, false},
	}
	for _, c := range cases {
		r := ParseRule(c.rule, BehaviorDeny, SourceUserSettings)
		if got := r.Matches(c.tool, c.value); got != c.want {
			t.Errorf("%q matching %s(%q) = %v, want %v", c.rule, c.tool, c.value, got, c.want)
		}
	}
}
