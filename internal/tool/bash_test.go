package tool

// Tests for the bash tool. The suite runs against the real shell — Git Bash
// on this project's Windows hosts, /bin/bash elsewhere — because the two
// behaviours under test (temp-file output and process-tree kill) are
// precisely the ones a fake would paper over.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// haveShell reports whether a shell is available; the bash tests skip
// without one rather than fail on an environment issue.
func haveShell(t *testing.T) bool {
	t.Helper()
	_, _, err := resolveBashShell()
	if err != nil {
		t.Skipf("no shell available: %v", err)
	}
	return true
}

func TestBashHelloWorld(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	res, err := runTool(t, bashTool(dir), map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(res))
	}
	// The trailing newline survives: pi's accumulator does not trim it
	// (truncateTail returns the content unchanged when nothing is cut).
	if got := resultText(res); got != "hello\n" {
		t.Errorf("output = %q", got)
	}
}

func TestBashCapturesStderr(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	res, err := runTool(t, bashTool(dir), map[string]any{"command": "echo out; echo err 1>&2"})
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	got := resultText(res)
	if !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Errorf("stdout and stderr not both captured: %q", got)
	}
}

func TestBashNoOutput(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	res, err := runTool(t, bashTool(dir), map[string]any{"command": "true"})
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	if got := resultText(res); got != "(no output)" {
		t.Errorf("output = %q", got)
	}
}

func TestBashNonZeroExitIsErrorResult(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	res, err := runTool(t, bashTool(dir), map[string]any{"command": "exit 3"})
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	if !res.IsError {
		t.Fatal("non-zero exit must be an error result")
	}
	if !strings.Contains(resultText(res), "Command exited with code 3") {
		t.Errorf("output = %q", resultText(res))
	}
}

func TestBashRunsInGivenCwd(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	writeFile(t, dir, "marker.txt", "x")
	res, err := runTool(t, bashTool(dir), map[string]any{"command": "cat marker.txt"})
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	if resultText(res) != "x" {
		t.Errorf("cwd not honoured: %q", resultText(res))
	}
	_ = marker
}

func TestBashTruncationKeepsFullOutputFile(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	// 3000 lines: past the 2000-line tail limit.
	res, err := runTool(t, bashTool(dir), map[string]any{"command": "seq 1 3000"})
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	got := resultText(res)
	if !strings.Contains(got, "[Showing lines 1001-3000 of 3000. Full output: ") {
		t.Errorf("truncation notice missing:\n%s", got[len(got)-200:])
	}
	details, ok := res.Details.(BashDetails)
	if !ok || details.FullOutputPath == "" {
		t.Fatalf("details missing fullOutputPath: %+v", res.Details)
	}
	// The temp file must hold the FULL output — that is what the notice
	// promises the model.
	raw, readErr := os.ReadFile(details.FullOutputPath)
	if readErr != nil {
		t.Fatalf("full output file unreadable: %v", readErr)
	}
	if !strings.HasPrefix(string(raw), "1\n") || !strings.HasSuffix(strings.TrimSpace(string(raw)), "3000") {
		t.Errorf("full output file does not hold everything (%d bytes)", len(raw))
	}
	// structuredContent.output carries the complete (capped) text too.
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent type = %T", res.StructuredContent)
	}
	if out, _ := sc["output"].(string); !strings.Contains(out, "1\n") {
		t.Error("structuredContent.output missing the head")
	}
	if sc["exit_code"].(int) != 0 {
		t.Errorf("exit_code = %v", sc["exit_code"])
	}
	if _, ok := sc["wall_time_seconds"].(float64); !ok {
		t.Errorf("wall_time_seconds missing: %+v", sc)
	}
}

func TestBashNoTruncationCleansTempFile(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	tmp := t.TempDir()
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if runtime.GOOS != "windows" {
		t.Setenv("TMPDIR", tmp)
	}

	res, err := runTool(t, bashTool(dir), map[string]any{"command": "echo small"})
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	if resultText(res) != "small\n" {
		t.Fatalf("output = %q", resultText(res))
	}
	// Nothing truncated → no temp file left behind.
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		t.Errorf("temp file leaked: %s", e.Name())
	}
}

func TestBashTimeoutKillsProcessTree(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	// The shell spawns a grandchild and waits for it: killing only the
	// shell would leave the grandchild holding the run for 30 seconds.
	start := time.Now()
	res, err := runTool(t, bashTool(dir), map[string]any{
		"command": "sleep 30 & sleep 30",
		"timeout": 1,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("timeout must throw (the loop converts it to an isError result)")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("timeout took %v — the process tree was not killed", elapsed)
	}
	if !strings.Contains(err.Error(), "Command timed out after 1 seconds") {
		t.Errorf("error = %q", err.Error())
	}
	// The partial output (nothing here) plus the status is the message.
	if !strings.HasSuffix(err.Error(), "Command timed out after 1 seconds") {
		t.Errorf("error tail = %q", err.Error())
	}
	_ = res

	// The grandchild must be gone: a second sleep-based probe would still
	// be running otherwise. Give the OS a moment to reap.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, probeErr := runTool(t, bashTool(dir), map[string]any{"command": "sleep 0.1; echo done"})
		if probeErr == nil && !out.IsError && strings.TrimSpace(resultText(out)) == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("system still saturated after tree kill")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestBashAbortKillsTree(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	tool := bashTool(dir)
	prepared, err := tool.Prepare(map[string]any{"command": "sleep 30 & sleep 30"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	start := time.Now()
	_, runErr := tool.Run(ctx, "call-1", prepared, nil)
	elapsed := time.Since(start)
	if runErr == nil {
		t.Fatal("aborted run must throw")
	}
	if !errors.Is(runErr, errAbortedSentinel{}) && !strings.Contains(runErr.Error(), "aborted") {
		t.Errorf("error = %q", runErr.Error())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("abort took %v — tree not killed", elapsed)
	}
}

func TestBashInvalidTimeout(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		timeout float64
		want    string
	}{
		{0, "Invalid timeout: must be a finite number of seconds"},
		{-5, "Invalid timeout: must be a finite number of seconds"},
		{3_000_000, "Invalid timeout: maximum is 2147483.647 seconds"},
	}
	for _, c := range cases {
		_, err := runTool(t, bashTool(dir), map[string]any{"command": "echo x", "timeout": c.timeout})
		if err == nil || err.Error() != c.want {
			t.Errorf("timeout %v: err = %v, want %q", c.timeout, err, c.want)
		}
	}
}

func TestBashTimeoutUnitsAreSeconds(t *testing.T) {
	if !haveShell(t) {
		return
	}
	dir := t.TempDir()
	start := time.Now()
	_, err := runTool(t, bashTool(dir), map[string]any{"command": "sleep 5", "timeout": 1})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("timeout of 1 second took %v — the unit is not seconds", elapsed)
	}
}

func TestBashMissingCwd(t *testing.T) {
	dir := t.TempDir()
	_, err := runTool(t, bashTool(dir), map[string]any{"command": "echo x"})
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	// Point the tool at a cwd that does not exist: the error must name it
	// (pi: bash.ts:106-108).
	tool := bashTool(filepath.Join(dir, "gone"))
	prepared, perr := tool.Prepare(map[string]any{"command": "echo x"})
	if perr != nil {
		t.Fatalf("Prepare: %v", perr)
	}
	_, runErr := tool.Run(context.Background(), "call-1", prepared, nil)
	if runErr == nil || !strings.Contains(runErr.Error(), "Working directory does not exist") {
		t.Errorf("err = %v", runErr)
	}
}

func TestBashSchemaRejectsUnknownField(t *testing.T) {
	tool := bashTool(t.TempDir())
	if _, err := tool.Prepare(map[string]any{"command": "x", "run_in_background": true}); err == nil {
		t.Error("the Claude-Code-shaped background flag is not part of pi's schema")
	}
}

func TestReadFullOutputMiddleOmission(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	// 2MB: past the 1MB structuredContent cap, so the head and tail are
	// kept around an omission marker (output-accumulator.ts:159-175).
	data := strings.Repeat("A", 1024*1024) + strings.Repeat("B", 1024*1024)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	content, truncated := readFullOutput(path, structuredOutputMaxBytes)
	if !truncated {
		t.Fatal("want truncated")
	}
	if !strings.HasPrefix(content, strings.Repeat("A", 100)) {
		t.Error("head missing")
	}
	if !strings.HasSuffix(content, strings.Repeat("B", 100)) {
		t.Error("tail missing")
	}
	if !strings.Contains(content, "[... 1048576 bytes omitted ...]") {
		t.Errorf("omission marker wrong: %q", content[len(content)/2-40:len(content)/2+40])
	}
}

func TestReadFullOutputSmallFileNotTruncated(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "s.log", "tiny\n")
	content, truncated := readFullOutput(path, structuredOutputMaxBytes)
	if truncated || content != "tiny\n" {
		t.Errorf("content = %q, truncated = %v", content, truncated)
	}
}

func TestTruncationNoticeLastLinePartial(t *testing.T) {
	tr := TruncationResult{
		Truncated: true, TruncatedBy: "bytes", LastLinePartial: true,
		OutputLines: 1, OutputBytes: 50 * 1024,
	}
	got := truncationNotice(tr, 42, 700*1024, "/tmp/x.log")
	want := "\n\n[Showing last 50.0KB of line 42 (line is 700.0KB). Full output: /tmp/x.log]"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestTruncationNoticeByLines(t *testing.T) {
	tr := TruncationResult{Truncated: true, TruncatedBy: "lines", OutputLines: 2000}
	got := truncationNotice(tr, 3000, 0, "/tmp/x.log")
	want := "\n\n[Showing lines 1001-3000 of 3000. Full output: /tmp/x.log]"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestPowerShellWindowsOnly(t *testing.T) {
	tool := powerShellTool(t.TempDir())
	prepared, err := tool.Prepare(map[string]any{"command": "echo x"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	_, runErr := tool.Run(context.Background(), "call-1", prepared, nil)
	if runtime.GOOS == "windows" {
		// On Windows this either runs or reports a missing executable;
		// both are acceptable, a panic is not.
		return
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "only available on Windows") {
		t.Errorf("err = %v", runErr)
	}
}
