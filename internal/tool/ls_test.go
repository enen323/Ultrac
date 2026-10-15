package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLsListsSortedWithDirSuffix(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "banana.txt", "")
	writeFile(t, dir, "Apple.txt", "")
	writeFile(t, dir, "cherry.txt", "")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, ".hidden", "")

	res := mustRun(t, lsTool(dir), map[string]any{})
	got := resultText(res)
	// Case-insensitive alphabetical order, dotfiles included, directories
	// carry a trailing "/" (pi ls.ts:108-130).
	want := ".hidden\nApple.txt\nbanana.txt\ncherry.txt\nsub/"
	if got != want {
		t.Errorf("listing =\n%q\nwant\n%q", got, want)
	}
}

func TestLsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	res := mustRun(t, lsTool(dir), map[string]any{})
	if got := resultText(res); got != "(empty directory)" {
		t.Errorf("got %q", got)
	}
}

func TestLsExplicitPathAndDefault(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "inner.txt", "")

	// Explicit path.
	res := mustRun(t, lsTool(dir), map[string]any{"path": sub})
	if got := resultText(res); got != "inner.txt" {
		t.Errorf("explicit path listing = %q", got)
	}
	// Default path is the tool's cwd.
	res = mustRun(t, lsTool(sub), map[string]any{})
	if got := resultText(res); got != "inner.txt" {
		t.Errorf("default listing = %q", got)
	}
}

func TestLsEntryLimitNotice(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, dir, name, "")
	}
	res := mustRun(t, lsTool(dir), map[string]any{"limit": 2})
	got := resultText(res)
	if !strings.Contains(got, "a\nb") {
		t.Errorf("entries = %q", got)
	}
	want := "\n\n[2 entries limit reached. Use limit=4 for more]"
	if !strings.HasSuffix(got, want) {
		t.Errorf("notice missing: %q", got)
	}
	details, ok := res.Details.(LsDetails)
	if !ok || details.EntryLimitReached != 2 {
		t.Errorf("details = %+v", res.Details)
	}
}

func TestLsPathNotFound(t *testing.T) {
	dir := t.TempDir()
	res := mustRun(t, lsTool(dir), map[string]any{"path": filepath.Join(dir, "missing")})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if want := "Path not found: " + filepath.Join(dir, "missing"); resultText(res) != want {
		t.Errorf("error = %q, want %q", resultText(res), want)
	}
}

func TestLsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "file.txt", "x")
	res := mustRun(t, lsTool(dir), map[string]any{"path": path})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if want := "Not a directory: " + path; resultText(res) != want {
		t.Errorf("error = %q, want %q", resultText(res), want)
	}
}
