package tool

// Tests for the grep tool.
//
// ripgrep is an external dependency and is not installed in every
// environment (including the one this was developed on), so the suite has
// three layers:
//
//  1. The not-installed path, tested deterministically by stubbing
//     lookPath.
//  2. The full parse/format/limit pipeline, tested against a fake rg: the
//     test binary re-executes itself as a child that speaks rg's --json
//     protocol. No real binary needed, cross-platform.
//  3. Integration tests against a real rg, skipped when absent.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain dispatches the fake-rg / fake-fd child processes. The child
// detects its role from the ULTRAC_TOOL_FAKE_* environment variables and
// never reaches the test runner.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("ULTRAC_TOOL_FAKE_RG") != "":
		fakeRgMain()
		return
	case os.Getenv("ULTRAC_TOOL_FAKE_FD") != "":
		fakeFdMain()
		return
	}
	os.Exit(m.Run())
}

// fakeRgMain speaks a slice of ripgrep's --json event protocol, driven by
// the pattern argument so tests can select scenarios.
//
// Events are marshaled through encoding/json, never string-formatted:
// a Windows search path contains backslashes, and raw %s interpolation
// would emit invalid JSON that the tool would (correctly) refuse to parse.
func fakeRgMain() {
	args := os.Args[1:]
	pattern, searchPath := "", "."
	for i, a := range args {
		if a == "--" {
			if i+1 < len(args) {
				pattern = args[i+1]
			}
			if i+2 < len(args) {
				searchPath = args[i+2]
			}
		}
	}
	switch pattern {
	case "NOMATCH":
		os.Exit(1) // rg: no matches
	case "BOOM":
		fmt.Fprintln(os.Stderr, "rg exploded: regex parse error")
		os.Exit(2)
	}

	type pathText struct {
		Text string `json:"text"`
	}
	type linesText struct {
		Text string `json:"text"`
	}
	type matchData struct {
		Path       pathText  `json:"path"`
		Lines      linesText `json:"lines"`
		LineNumber int       `json:"line_number"`
	}
	type event struct {
		Type string     `json:"type"`
		Data *matchData `json:"data,omitempty"`
	}
	emit := func(ev event) {
		raw, err := json.Marshal(ev)
		if err != nil {
			return
		}
		fmt.Fprintf(os.Stdout, "%s\n", raw)
	}

	alpha := searchPath + string(os.PathSeparator) + "alpha.txt"
	beta := searchPath + string(os.PathSeparator) + "beta.txt"
	emit(event{Type: "begin", Data: &matchData{Path: pathText{alpha}}})
	emit(event{Type: "match", Data: &matchData{Path: pathText{alpha}, Lines: linesText{"first needle\n"}, LineNumber: 1}})
	emit(event{Type: "match", Data: &matchData{Path: pathText{beta}, Lines: linesText{"needle two\n"}, LineNumber: 7}})
	emit(event{Type: "match", Data: &matchData{Path: pathText{beta}, Lines: linesText{strings.Repeat("L", 600) + "\n"}, LineNumber: 9}})
	emit(event{Type: "end", Data: &matchData{Path: pathText{beta}}})
	emit(event{Type: "summary"})
	os.Exit(0)
}

// stubLookPath points the tools at the re-executed test binary.
func stubLookPath(t *testing.T, role string) {
	t.Helper()
	old := lookPath
	lookPath = func(name string) (string, error) {
		if name == "rg" || name == "fd" {
			return os.Args[0], nil
		}
		return old(name)
	}
	t.Cleanup(func() { lookPath = old })
	t.Setenv("ULTRAC_TOOL_FAKE_"+role, "1")
}

func TestGrepMissingRipgrep(t *testing.T) {
	old := lookPath
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { lookPath = old })

	res := mustRun(t, grepTool(t.TempDir()), map[string]any{"pattern": "x"})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(resultText(res), "ripgrep (rg) is not installed") {
		t.Errorf("error = %q", resultText(res))
	}
}

func TestGrepFakeRgMatchFormat(t *testing.T) {
	stubLookPath(t, "RG")
	dir := t.TempDir()
	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	got := resultText(res)
	// Paths are relativized against the search root with forward slashes;
	// the 600-char line is truncated to 500 with the marker; the notices
	// report the truncation (grep.ts:273-303).
	wantLines := []string{
		"alpha.txt:1: first needle",
		"beta.txt:7: needle two",
		"beta.txt:9: " + strings.Repeat("L", 500) + "... [truncated]",
	}
	for _, want := range wantLines {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Some lines truncated to 500 chars. Use read tool to see full lines") {
		t.Errorf("lines-truncated notice missing:\n%s", got)
	}
}

func TestGrepFakeRgLimitNotice(t *testing.T) {
	stubLookPath(t, "RG")
	dir := t.TempDir()
	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "needle", "limit": 2})
	got := resultText(res)
	if !strings.Contains(got, "2 matches limit reached. Use limit=4 for more, or refine pattern") {
		t.Errorf("limit notice missing:\n%s", got)
	}
	// Only two matches are shown.
	if strings.Count(got, "beta.txt:")+strings.Count(got, "alpha.txt:") != 2 {
		t.Errorf("too many matches shown:\n%s", got)
	}
}

func TestGrepFakeRgNoMatches(t *testing.T) {
	stubLookPath(t, "RG")
	dir := t.TempDir()
	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "NOMATCH"})
	if res.IsError {
		t.Fatalf("no matches is not an error: %s", resultText(res))
	}
	if got := resultText(res); got != "No matches found" {
		t.Errorf("output = %q", got)
	}
}

func TestGrepFakeRgErrorExit(t *testing.T) {
	stubLookPath(t, "RG")
	dir := t.TempDir()
	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "BOOM"})
	if !res.IsError {
		t.Fatal("rg's stderr must surface as an error result")
	}
	if !strings.Contains(resultText(res), "rg exploded") {
		t.Errorf("error = %q", resultText(res))
	}
}

func TestGrepFakeRgContextLines(t *testing.T) {
	stubLookPath(t, "RG")
	dir := t.TempDir()
	// With context > 0 the tool reads the matched files itself (pi's
	// formatBlock), so the files must exist for the context lines to
	// render.
	writeFile(t, dir, "alpha.txt", "first needle\nsecond\nthird\n")
	writeFile(t, dir, "beta.txt", "l1\nl2\nl3\nl4\nl5\nl6\nneedle two\nl8\nlong line\n")
	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "needle", "context": 2})
	got := resultText(res)
	if !strings.Contains(got, "alpha.txt:1: first needle") {
		t.Errorf("match line missing:\n%s", got)
	}
	if !strings.Contains(got, "alpha.txt-2- second") || !strings.Contains(got, "alpha.txt-3- third") {
		t.Errorf("context lines missing:\n%s", got)
	}
	if !strings.Contains(got, "beta.txt:7: needle two") {
		t.Errorf("second file's match missing:\n%s", got)
	}
	if !strings.Contains(got, "beta.txt-5- l5") || !strings.Contains(got, "beta.txt-9- long line") {
		t.Errorf("second file's context missing:\n%s", got)
	}
}

func TestGrepPatternRequired(t *testing.T) {
	// pattern is required by the schema: preflight rejects it before
	// Execute runs.
	tool := grepTool(t.TempDir())
	if _, err := tool.Prepare(map[string]any{}); err == nil {
		t.Error("pattern is required")
	}
}

func TestGrepPathNotFound(t *testing.T) {
	stubLookPath(t, "RG")
	dir := t.TempDir()
	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "x", "path": filepath.Join(dir, "missing")})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if want := "Path not found: " + filepath.Join(dir, "missing"); resultText(res) != want {
		t.Errorf("error = %q, want %q", resultText(res), want)
	}
}

// --- integration against a real rg (skipped when absent) ----------------

func TestGrepFormatPathRelativeAndEscape(t *testing.T) {
	root := `/repo`
	if got := grepFormatPath(`/repo/sub/f.go`, true, root); got != "sub/f.go" {
		t.Errorf("inside root: %q", got)
	}
	// A path outside the search root falls back to the basename — a "../"
	// prefix would leak structure the model did not ask about.
	if got := grepFormatPath(`/other/f.go`, true, root); got != "f.go" {
		t.Errorf("outside root: %q", got)
	}
	// A single-file search reports basenames.
	if got := grepFormatPath(`/other/f.go`, false, `/other/f.go`); got != "f.go" {
		t.Errorf("single file: %q", got)
	}
}

func TestGrepRealRipgrep(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	writeFile(t, dir, "hit.txt", "alpha\nneedle here\nomega\n")
	writeFile(t, dir, "miss.txt", "nothing to see\n")

	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	got := resultText(res)
	if !strings.Contains(got, "hit.txt:2: needle here") {
		t.Errorf("match line wrong:\n%s", got)
	}
	if strings.Contains(got, "miss.txt") {
		t.Errorf("non-matching file reported:\n%s", got)
	}
}

func TestGrepRealRipgrepIgnoreCase(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	writeFile(t, dir, "c.txt", "MixedCase\n")
	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "mixedcase", "ignoreCase": true})
	if res.IsError || !strings.Contains(resultText(res), "MixedCase") {
		t.Errorf("ignoreCase broken: %+v", res)
	}
}

func TestGrepRealRipgrepGlobFilter(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "target\n")
	writeFile(t, dir, "b.js", "target\n")
	res := mustRun(t, grepTool(dir), map[string]any{"pattern": "target", "glob": "*.ts"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got := resultText(res); !strings.Contains(got, "a.ts") || strings.Contains(got, "b.js") {
		t.Errorf("glob filter broken:\n%s", got)
	}
}
