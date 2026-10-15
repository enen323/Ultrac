package tool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesFileAndParents(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "c.txt")
	res := mustRun(t, writeTool(dir), map[string]any{"path": target, "content": "hello"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	// pi reports the path exactly as the model sent it.
	if got := resultText(res); got != "Successfully wrote to "+target {
		t.Errorf("result = %q", got)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "hello" {
		t.Fatalf("file content = %q, err = %v", raw, err)
	}
}

func TestWriteOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "over.txt", "old")
	res := mustRun(t, writeTool(dir), map[string]any{"path": path, "content": "new"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "new" {
		t.Errorf("content = %q", raw)
	}
}

func TestWriteRelativePath(t *testing.T) {
	dir := t.TempDir()
	res := mustRun(t, writeTool(dir), map[string]any{"path": "sub/rel.txt", "content": "x"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "rel.txt")); err != nil {
		t.Fatalf("file not created relative to cwd: %v", err)
	}
}

func TestWriteMissingPathRejected(t *testing.T) {
	// path is required by the schema, so the rejection happens at Prepare —
	// the runtime's preflight, before Execute would ever run (the loop
	// turns it into an isError tool result, as pi's schema validation
	// does).
	tool := writeTool(t.TempDir())
	if _, err := tool.Prepare(map[string]any{"content": "x"}); err == nil {
		t.Error("path is required")
	}
}

func TestWriteSchemaRejectsUnknownField(t *testing.T) {
	tool := writeTool(t.TempDir())
	if _, err := tool.Prepare(map[string]any{"path": "x", "content": "y", "extra": true}); err == nil {
		t.Error("additionalProperties:false should reject unknown fields")
	}
}
