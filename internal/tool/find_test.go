package tool

// Tests for the find tool, structured like grep_test.go: deterministic
// coverage through a fake fd (the test binary re-executing itself), plus
// integration tests against a real fd when it is installed.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeFdMain prints paths one per line like fd does. The number of results
// follows --max-results so tests can exercise the limit logic, and the
// pattern selects the empty and error scenarios.
func fakeFdMain() {
	args := os.Args[1:]
	pattern, searchPath := "", "."
	maxResults := 5
	for i, a := range args {
		switch a {
		case "--":
			if i+1 < len(args) {
				pattern = args[i+1]
			}
			if i+2 < len(args) {
				searchPath = args[i+2]
			}
		case "--max-results":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					maxResults = n
				}
			}
		}
	}
	if pattern == "BOOM" {
		fmt.Fprintln(os.Stderr, "fd: error")
		os.Exit(2)
	}
	if pattern == "NONE" {
		maxResults = 0
	}
	for i := 1; i <= maxResults; i++ {
		fmt.Fprintf(os.Stdout, "%s/file%02d.txt\n", searchPath, i)
	}
	os.Exit(0)
}

func TestFindMissingFd(t *testing.T) {
	old := lookPath
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { lookPath = old })

	res := mustRun(t, findTool(t.TempDir()), map[string]any{"pattern": "*.txt"})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(resultText(res), "fd is not installed") {
		t.Errorf("error = %q", resultText(res))
	}
}

func TestFindFakeFdRelativizesResults(t *testing.T) {
	stubLookPath(t, "FD")
	dir := t.TempDir()
	res := mustRun(t, findTool(dir), map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	// fd emits absolute paths; the tool relativizes them against the
	// search root (find.ts:14-24, 268-273). The default limit is 1000, so
	// the fake (which honours --max-results) fills it and the notice
	// fires.
	got := resultText(res)
	lines := strings.Split(got, "\n")
	if lines[0] != "file01.txt" || lines[4] != "file05.txt" {
		t.Errorf("first lines = %q", lines[:5])
	}
	if !strings.Contains(got, "[1000 results limit reached. Use limit=2000 for more, or refine pattern]") {
		t.Errorf("limit notice missing")
	}
}

func TestFindFakeFdLimitNotice(t *testing.T) {
	stubLookPath(t, "FD")
	dir := t.TempDir()
	res := mustRun(t, findTool(dir), map[string]any{"pattern": "*.txt", "limit": 3})
	got := resultText(res)
	if !strings.Contains(got, "3 results limit reached. Use limit=6 for more, or refine pattern") {
		t.Errorf("limit notice missing:\n%s", got)
	}
	if strings.Count(got, "file") != 3 {
		t.Errorf("more than the limit shown:\n%s", got)
	}
	details, ok := res.Details.(FindDetails)
	if !ok || details.ResultLimitReached != 3 {
		t.Errorf("details = %+v", res.Details)
	}
}

func TestFindFakeFdNoResults(t *testing.T) {
	stubLookPath(t, "FD")
	dir := t.TempDir()
	res := mustRun(t, findTool(dir), map[string]any{"pattern": "NONE"})
	if res.IsError {
		t.Fatalf("no results is not an error: %s", resultText(res))
	}
	if got := resultText(res); got != "No files found matching pattern" {
		t.Errorf("output = %q", got)
	}
}

func TestFindFakeFdErrorExit(t *testing.T) {
	stubLookPath(t, "FD")
	dir := t.TempDir()
	res := mustRun(t, findTool(dir), map[string]any{"pattern": "BOOM"})
	if !res.IsError {
		t.Fatal("fd's stderr must surface as an error result")
	}
	if !strings.Contains(resultText(res), "fd: error") {
		t.Errorf("error = %q", resultText(res))
	}
}

func TestFindPathNotFound(t *testing.T) {
	stubLookPath(t, "FD")
	dir := t.TempDir()
	res := mustRun(t, findTool(dir), map[string]any{"pattern": "*", "path": filepath.Join(dir, "missing")})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if want := "Path not found: " + filepath.Join(dir, "missing"); resultText(res) != want {
		t.Errorf("error = %q, want %q", resultText(res), want)
	}
}

func TestFindPatternRequired(t *testing.T) {
	// pattern is required by the schema: preflight rejects it before
	// Execute runs.
	tool := findTool(t.TempDir())
	if _, err := tool.Prepare(map[string]any{}); err == nil {
		t.Error("pattern is required")
	}
}

func TestRelativizeFindResultPath(t *testing.T) {
	// OS-absolute roots: filepath.Rel requires volume-qualified paths on
	// Windows, so Unix-style "/a/b" literals would not exercise it there.
	root := t.TempDir()
	cases := []struct {
		result, want string
	}{
		{filepath.Join(root, "c.txt"), "c.txt"},
		{filepath.Join(root, "sub", "c.txt"), "sub/c.txt"},
		{"c.txt", "c.txt"},
		{filepath.Join(root, "dir") + string(filepath.Separator), "dir/"},
	}
	for _, c := range cases {
		if got := relativizeFindResultPath(c.result, root); got != c.want {
			t.Errorf("relativize(%q, %q) = %q, want %q", c.result, root, got, c.want)
		}
	}
}

func TestInsideGitRepo(t *testing.T) {
	dir := t.TempDir()
	if insideGitRepo(dir) {
		t.Error("plain temp dir is not a git repo")
	}
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !insideGitRepo(nested) {
		t.Error("nested dir inside a repo should be detected")
	}
}

// --- integration against a real fd (skipped when absent) -----------------

func TestFindRealFd(t *testing.T) {
	if _, err := exec.LookPath("fd"); err != nil {
		t.Skip("fd not installed")
	}
	dir := t.TempDir()
	writeFile(t, dir, "one.txt", "")
	writeFile(t, dir, "two.txt", "")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "sub"), "three.txt", "")

	res := mustRun(t, findTool(dir), map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	got := resultText(res)
	for _, want := range []string{"one.txt", "two.txt", "sub/three.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFindRealFdPathPattern(t *testing.T) {
	if _, err := exec.LookPath("fd"); err != nil {
		t.Skip("fd not installed")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "src"), "a.spec.ts", "")
	writeFile(t, filepath.Join(dir, "src"), "b.ts", "")
	res := mustRun(t, findTool(dir), map[string]any{"pattern": "src/**/*.spec.ts"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got := resultText(res); !strings.Contains(got, "a.spec.ts") || strings.Contains(got, "b.ts") {
		t.Errorf("path pattern broken:\n%s", got)
	}
}
