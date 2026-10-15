package tool

// Tests for the edit tool: the exact/fuzzy matching contract, the error
// strings, and the batch semantics.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// editArgs builds raw model arguments the way JSON decoding would deliver
// them: []any of map[string]any, not the typed Go slice — the shim's type
// switch has to see what the wire actually carries.
func editArgs(path string, edits []EditPair) map[string]any {
	raw := make([]any, len(edits))
	for i, e := range edits {
		raw[i] = map[string]any{"oldText": e.OldText, "newText": e.NewText}
	}
	return map[string]any{"path": path, "edits": raw}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestEditExactSingle(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "one\ntwo\nthree")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "two", NewText: "TWO"}}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got := resultText(res); got != "Successfully replaced 1 block(s) in "+path+"." {
		t.Errorf("result = %q", got)
	}
	if got := readFileString(t, path); got != "one\nTWO\nthree" {
		t.Errorf("file = %q", got)
	}
}

func TestEditMultipleDisjoint(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "aaa\nbbb\nccc\nddd")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{
		{OldText: "aaa", NewText: "AAA"},
		{OldText: "ddd", NewText: "DDD"},
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if !strings.HasPrefix(resultText(res), "Successfully replaced 2 block(s)") {
		t.Errorf("result = %q", resultText(res))
	}
	if got := readFileString(t, path); got != "AAA\nbbb\nccc\nDDD" {
		t.Errorf("file = %q", got)
	}
}

func TestEditMatchedAgainstOriginalNotIncrementally(t *testing.T) {
	// Two edits whose oldTexts only both exist in the ORIGINAL: applying
	// the first would destroy the second's match if edits were chained.
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "x = 1\ny = 2\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{
		{OldText: "x = 1", NewText: "x = 10"},
		{OldText: "y = 2", NewText: "y = 20"},
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got := readFileString(t, path); got != "x = 10\ny = 20\n" {
		t.Errorf("file = %q", got)
	}
}

func TestEditMultipleMatchesRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "dup\ndup\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "dup", NewText: "x"}}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	want := "Found 2 occurrences of the text in " + path + ". The text must be unique. Please provide more context to make it unique."
	if got := resultText(res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestEditNotFoundSingle(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "alpha\nbeta\ngamma\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "delta", NewText: "x"}}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	want := "Could not find the exact text in " + path + ". The old text must match exactly including all whitespace and newlines.\nNo line of the old text appears anywhere in the file."
	if got := resultText(res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestEditNotFoundWithClosestFragment(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "alpha\nbeta\ngamma\n")
	// "gammq" is a one-character typo of "gamma": the hint must name line 3
	// and report the similarity.
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "gammq", NewText: "x"}}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	got := resultText(res)
	if !strings.HasPrefix(got, "Could not find the exact text in "+path+".") {
		t.Errorf("missing base message: %q", got)
	}
	if !strings.Contains(got, "Closest match at line 3 (75% similar to the old text):") {
		t.Errorf("closest-fragment hint wrong:\n%s", got)
	}
	// The window around the closest line is shown for comparison.
	if !strings.Contains(got, "  3  gamma") {
		t.Errorf("window not rendered:\n%s", got)
	}
}

func TestEditNotFoundPartialLineMatch(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "alpha\nbeta\ngamma\n")
	// One exact line plus one mistyped: tier 1 anchors on the exact line.
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "beta\ngammq", NewText: "x"}}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	got := resultText(res)
	if !strings.Contains(got, "Closest match at line 1: 1 of 2 lines from the old text appear here.") {
		t.Errorf("tier-1 hint wrong:\n%s", got)
	}
	if !strings.Contains(got, "  2  beta") {
		t.Errorf("window not rendered:\n%s", got)
	}
}

func TestEditNotFoundMultiEditNamesIndex(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "keep\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{
		{OldText: "keep", NewText: "kept"},
		{OldText: "missing", NewText: "x"},
	}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	if !strings.HasPrefix(resultText(res), "Could not find edits[1] in "+path+".") {
		t.Errorf("error = %q", resultText(res))
	}
	// The file must be untouched when any edit fails.
	if got := readFileString(t, path); got != "keep\n" {
		t.Errorf("file was modified despite the failed batch: %q", got)
	}
}

func TestEditFuzzyTrailingWhitespace(t *testing.T) {
	dir := t.TempDir()
	// The file has trailing spaces the model's oldText omits, on both the
	// edited block's first line and on lines the edit must not touch.
	path := writeFile(t, dir, "a.txt", "header   \nline one   \nline two\nfooter\t\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "line one\nline two", NewText: "LINE ONE\nLINE TWO"}}))
	if res.IsError {
		t.Fatalf("fuzzy match failed: %s", resultText(res))
	}
	// The touched lines are rewritten from the normalized view; every
	// untouched line keeps its original bytes — that is what
	// applyReplacementsPreservingUnchangedLines buys.
	if got := readFileString(t, path); got != "header   \nLINE ONE\nLINE TWO\nfooter\t\n" {
		t.Errorf("file = %q", got)
	}
}

func TestEditFuzzySmartQuotes(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "say \u201chello\u201d now\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: `say "hello" now`, NewText: `say "bye" now`}}))
	if res.IsError {
		t.Fatalf("fuzzy quote match failed: %s", resultText(res))
	}
	if got := readFileString(t, path); got != `say "bye" now`+"\n" {
		t.Errorf("file = %q", got)
	}
}

func TestEditFuzzyFullWidthNFKC(t *testing.T) {
	dir := t.TempDir()
	// Full-width parentheses in the file; the model sends ASCII.
	path := writeFile(t, dir, "a.txt", "call（foo）\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "call(foo)", NewText: "call(bar)"}}))
	if res.IsError {
		t.Fatalf("NFKC fuzzy match failed: %s", resultText(res))
	}
	if got := readFileString(t, path); got != "call(bar)\n" {
		t.Errorf("file = %q", got)
	}
}

func TestEditEmptyOldText(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "x")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "", NewText: "y"}}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	if want := "oldText must not be empty in " + path + "."; resultText(res) != want {
		t.Errorf("got %q, want %q", resultText(res), want)
	}
}

func TestEditNoChange(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "same")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "same", NewText: "same"}}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	want := "No changes made to " + path + ". The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected."
	if got := resultText(res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestEditOverlapRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "0123456789")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{
		{OldText: "234", NewText: "x"},
		{OldText: "456", NewText: "y"},
	}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	want := "edits[0] and edits[1] overlap in " + path + ". Merge them into one edit or target disjoint regions."
	if got := resultText(res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestEditPreservesBOM(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "\uFEFFhello\nworld\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "hello", NewText: "goodbye"}}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got := readFileString(t, path); got != "\uFEFFgoodbye\nworld\n" {
		t.Errorf("BOM not preserved: %q", got)
	}
}

func TestEditPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "one\r\ntwo\r\nthree\r\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "two", NewText: "TWO"}}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got := readFileString(t, path); got != "one\r\nTWO\r\nthree\r\n" {
		t.Errorf("CRLF not preserved: %q", got)
	}
}

func TestEditMissingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.txt")
	res := mustRun(t, editTool(dir), editArgs(missing, []EditPair{{OldText: "a", NewText: "b"}}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	if !strings.HasPrefix(resultText(res), "Could not edit file: "+missing+".") {
		t.Errorf("error = %q", resultText(res))
	}
}

func TestEditDetailsCarryDiffAndPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "one\ntwo\nthree\n")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{{OldText: "two", NewText: "TWO"}}))
	details, ok := res.Details.(EditDetails)
	if !ok {
		t.Fatalf("details type = %T", res.Details)
	}
	if details.FirstChangedLine == nil || *details.FirstChangedLine != 2 {
		t.Errorf("firstChangedLine = %v", details.FirstChangedLine)
	}
	if !strings.Contains(details.Diff, "-2 two") || !strings.Contains(details.Diff, "+2 TWO") {
		t.Errorf("display diff missing the change:\n%s", details.Diff)
	}
	if !strings.Contains(details.Patch, "--- "+path) || !strings.Contains(details.Patch, "@@ -1,4 +1,4 @@") {
		t.Errorf("patch malformed:\n%s", details.Patch)
	}
}

func TestEditShimSingleEditObject(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "x")
	// edits as a bare object instead of a one-element array.
	res := mustRun(t, editTool(dir), map[string]any{
		"path": path,
		"edits": map[string]any{
			"oldText": "x", "newText": "y",
		},
	})
	if res.IsError {
		t.Fatalf("single-object shim failed: %s", resultText(res))
	}
	if got := readFileString(t, path); got != "y" {
		t.Errorf("file = %q", got)
	}
}

func TestEditShimJSONStringEdits(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "x")
	res := mustRun(t, editTool(dir), map[string]any{
		"path":  path,
		"edits": `[{"oldText":"x","newText":"y"}]`,
	})
	if res.IsError {
		t.Fatalf("JSON-string shim failed: %s", resultText(res))
	}
	if got := readFileString(t, path); got != "y" {
		t.Errorf("file = %q", got)
	}
}

func TestEditShimLegacyFlatPair(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "x")
	res := mustRun(t, editTool(dir), map[string]any{
		"path": path, "oldText": "x", "newText": "y",
	})
	if res.IsError {
		t.Fatalf("legacy flat shim failed: %s", resultText(res))
	}
	if got := readFileString(t, path); got != "y" {
		t.Errorf("file = %q", got)
	}
}

func TestEditShimRejectsMalformedElement(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "x")
	// The shim owns the final shape (P0 decision), so a malformed element
	// is rejected at Prepare — the runtime's preflight.
	tool := editTool(dir)
	_, err := tool.Prepare(map[string]any{
		"path":  path,
		"edits": []any{map[string]any{"oldText": "x"}}, // newText missing
	})
	if err == nil || !strings.Contains(err.Error(), "oldText and newText strings") {
		t.Errorf("err = %v", err)
	}
}

func TestEditEmptyEditsRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "x")
	res := mustRun(t, editTool(dir), editArgs(path, []EditPair{}))
	if !res.IsError {
		t.Fatal("expected rejection")
	}
	if want := "Edit tool input is invalid. edits must contain at least one replacement."; resultText(res) != want {
		t.Errorf("got %q, want %q", resultText(res), want)
	}
}

func TestEditMutationQueueSerializesSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Two concurrent appends through the queue: without serialization one
	// read-modify-write would clobber the other.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := withFileMutationQueue(path, func() error {
				cur := readFileStringMust(t, path)
				return os.WriteFile(path, []byte(cur+string(rune('a'+i))+"\n"), 0o644)
			})
			if err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got := readFileStringMust(t, path)
	if len(got) != 16 { // 8 lines of 2 bytes
		t.Errorf("lost updates: %q", got)
	}
}

func readFileStringMust(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
