package tool

// Tests for the read tool. The continuation notices are the contract with
// the model, so they are asserted character for character against the
// TypeScript source strings.

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// runTool executes one tool call the way the agent runtime does: Prepare
// (shim + schema validation + typed decode) then Run.
func runTool(t *testing.T, tool agent.Tool, args map[string]any) (agent.Result, error) {
	t.Helper()
	prepared, err := tool.Prepare(args)
	if err != nil {
		t.Fatalf("Prepare(%v) failed: %v", args, err)
	}
	return tool.Run(context.Background(), "call-1", prepared, nil)
}

// Erased-tool constructors: the tests drive the runtime view of each tool,
// exactly as the agent loop does.
func readTool(cwd string) agent.Tool       { return agent.Erase(NewReadTool(cwd)) }
func writeTool(cwd string) agent.Tool      { return agent.Erase(NewWriteTool(cwd)) }
func lsTool(cwd string) agent.Tool         { return agent.Erase(NewLsTool(cwd)) }
func editTool(cwd string) agent.Tool       { return agent.Erase(NewEditTool(cwd)) }
func bashTool(cwd string) agent.Tool       { return agent.Erase(NewBashTool(cwd)) }
func grepTool(cwd string) agent.Tool       { return agent.Erase(NewGrepTool(cwd)) }
func findTool(cwd string) agent.Tool       { return agent.Erase(NewFindTool(cwd)) }
func powerShellTool(cwd string) agent.Tool { return agent.Erase(NewPowerShellTool(cwd)) }

// mustRun executes one call and fails the test on a thrown error.
func mustRun(t *testing.T, tool agent.Tool, args map[string]any) agent.Result {
	t.Helper()
	res, err := runTool(t, tool, args)
	if err != nil {
		t.Fatalf("Run(%v) threw: %v", args, err)
	}
	return res
}

// resultText joins the text blocks of a result.
func resultText(r agent.Result) string {
	var parts []string
	for _, b := range r.Content {
		if b.Type == ai.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestReadSmallFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "hello.txt", "one\ntwo\nthree")
	res := mustRun(t, readTool(dir), map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(res))
	}
	if got := resultText(res); got != "one\ntwo\nthree" {
		t.Errorf("content = %q", got)
	}
}

func TestReadRelativePathResolvesAgainstCwd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "rel.txt", "relative")
	res := mustRun(t, readTool(dir), map[string]any{"path": "rel.txt"})
	if res.IsError || resultText(res) != "relative" {
		t.Fatalf("relative path not resolved: %+v", res)
	}
}

func TestReadTruncationByLinesNotice(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		b.WriteString("line\n")
	}
	path := writeFile(t, dir, "big.txt", b.String())
	res := mustRun(t, readTool(dir), map[string]any{"path": path})
	// 2501, not 2500: pi splits on "\n" and keeps the trailing empty
	// element (read.ts:157), so the file's line count includes it.
	body := strings.TrimSuffix(strings.Repeat("line\n", defaultMaxLines), "\n")
	want := body + "\n\n[Showing lines 1-2000 of 2501. Use offset=2001 to continue.]"
	if got := resultText(res); got != want {
		t.Errorf("content mismatch:\n got %q\nwant %q", got, want)
	}
	details, ok := res.Details.(ReadDetails)
	if !ok || details.Truncation == nil || !details.Truncation.Truncated {
		t.Errorf("details missing truncation: %+v", res.Details)
	}
}

func TestReadTruncationByBytesNotice(t *testing.T) {
	dir := t.TempDir()
	// 100 lines of exactly 1000 chars: the 50KB byte limit hits at line 51
	// (1000 + 50*1001 = 51050 bytes fit; the 52nd line would exceed).
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(strings.Repeat("x", 1000) + "\n")
	}
	path := writeFile(t, dir, "wide.txt", b.String())
	res := mustRun(t, readTool(dir), map[string]any{"path": path})
	// 101 = 100 lines plus the trailing empty element of split("\n").
	body := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", 1000)+"\n", 51), "\n")
	want := body + "\n\n[Showing lines 1-51 of 101 (50.0KB limit). Use offset=52 to continue.]"
	got := resultText(res)
	if got != want {
		t.Errorf("content mismatch:\n got %q...\nwant %q...", got[:40], want[:40])
	}
	if !strings.Contains(got, "(50.0KB limit)") {
		t.Error("byte-limit notice missing the formatted size")
	}
}

func TestReadFirstLineExceedsLimit(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "huge.txt", strings.Repeat("q", 60*1024)+"\nsecond")
	res := mustRun(t, readTool(dir), map[string]any{"path": path})
	want := "[Line 1 is 60.0KB, exceeds 50.0KB limit. Use bash: sed -n '1p' " + path + " | head -c 51200]"
	if got := resultText(res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestReadOffsetBeyondEnd(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "ten.txt", strings.Repeat("l\n", 10))
	res := mustRun(t, readTool(dir), map[string]any{"path": path, "offset": 3000})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	// 11 = 10 lines plus the trailing empty element of split("\n").
	want := "Offset 3000 is beyond end of file (11 lines total)"
	if got := resultText(res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestReadUserLimitWithRemaining(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "six.txt", "a\nb\nc\nd\ne\nf")
	res := mustRun(t, readTool(dir), map[string]any{"path": path, "offset": 2, "limit": 3})
	// nextOffset = startLine + shown + 1 = 1 + 3 + 1 = 5 (read.ts:198-199).
	want := "b\nc\nd\n\n[2 more lines in file. Use offset=5 to continue.]"
	if got := resultText(res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestReadOffsetAndLimitWindow(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "win.txt", "a\nb\nc\nd\ne\nf")
	res := mustRun(t, readTool(dir), map[string]any{"path": path, "offset": 2, "limit": 2})
	// The window is exact, and the remaining-lines notice names the next
	// offset (read.ts:195-199).
	want := "b\nc\n\n[3 more lines in file. Use offset=4 to continue.]"
	if got := resultText(res); got != want {
		t.Errorf("window = %q", got)
	}
}

func TestReadEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "empty.txt", "")
	res := mustRun(t, readTool(dir), map[string]any{"path": path})
	// Documented deviation from pi (which returns ""): an empty text block
	// risks an API rejection, so a note goes back instead.
	if got := resultText(res); got != "File exists but is empty (0 bytes)." {
		t.Errorf("empty file result = %q", got)
	}
}

func TestReadMissingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.txt")
	res := mustRun(t, readTool(dir), map[string]any{"path": missing})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	// The OS-native not-found message (Windows: "The system cannot find
	// the file specified."; Unix: "no such file or directory") — pi
	// surfaces the raw fs error too.
	got := resultText(res)
	if !strings.Contains(got, missing) {
		t.Errorf("error does not name the file: %q", got)
	}
}

func TestReadDirectoryFails(t *testing.T) {
	dir := t.TempDir()
	res := mustRun(t, readTool(dir), map[string]any{"path": dir})
	if !res.IsError {
		t.Fatal("reading a directory should be an error (pi behavior; ls is the tool for that)")
	}
}

func TestReadImageReturnsImageBlock(t *testing.T) {
	dir := t.TempDir()
	// Minimal PNG magic plus payload.
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("payload")...)
	path := writeFile(t, dir, "dot.png", string(png))
	res := mustRun(t, readTool(dir), map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if len(res.Content) != 2 {
		t.Fatalf("want [note, image], got %d blocks", len(res.Content))
	}
	if res.Content[0].Text != "Read image file [image/png]" {
		t.Errorf("note = %q", res.Content[0].Text)
	}
	img := res.Content[1]
	if img.Type != ai.BlockImage || img.MimeType != "image/png" {
		t.Fatalf("image block = %+v", img)
	}
	want := base64.StdEncoding.EncodeToString(png)
	if img.Data != want {
		t.Error("image data is not the base64 of the file")
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok || sc["mimeType"] != "image/png" {
		t.Errorf("structuredContent = %+v", res.StructuredContent)
	}
}

func TestReadBinaryGuard(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "blob.bin", "\x00\x01\x02binary\x00stuff")
	res := mustRun(t, readTool(dir), map[string]any{"path": path})
	if !res.IsError {
		t.Fatal("binary file should be refused, not pushed into the transcript as text")
	}
	if !strings.Contains(resultText(res), "binary data") {
		t.Errorf("error = %q", resultText(res))
	}
}

func TestReadAborted(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := readTool(dir)
	prepared, err := tool.Prepare(map[string]any{"path": "x"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := tool.Run(ctx, "call-1", prepared, nil)
	if err != nil {
		t.Fatalf("Run threw: %v", err)
	}
	if !res.IsError || resultText(res) != "Operation aborted" {
		t.Errorf("abort result = %+v", res)
	}
}

func TestReadSchemaRejectsUnknownField(t *testing.T) {
	tool := readTool(t.TempDir())
	if _, err := tool.Prepare(map[string]any{"path": "x", "bogus": 1}); err == nil {
		t.Error("additionalProperties:false should reject unknown fields")
	}
}

func TestReadSchemaRejectsMissingPath(t *testing.T) {
	tool := readTool(t.TempDir())
	if _, err := tool.Prepare(map[string]any{"offset": 1}); err == nil {
		t.Error("required path should be enforced")
	}
}
