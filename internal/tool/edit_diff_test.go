package tool

// Unit tests for the diff and fuzzy-match machinery.

import (
	"strings"
	"testing"
)

func TestNormalizeToLFAndRestore(t *testing.T) {
	if got := normalizeToLF("a\r\nb\rc\nd"); got != "a\nb\nc\nd" {
		t.Errorf("normalizeToLF = %q", got)
	}
	if got := restoreLineEndings("a\nb", "\r\n"); got != "a\r\nb" {
		t.Errorf("restoreLineEndings = %q", got)
	}
	if got := restoreLineEndings("a\nb", "\n"); got != "a\nb" {
		t.Errorf("restoreLineEndings passthrough = %q", got)
	}
}

func TestDetectLineEnding(t *testing.T) {
	cases := map[string]string{
		"a\r\nb":    "\r\n",
		"a\nb":      "\n",
		"a\r\nb\nc": "\r\n", // the first "\r\n" starts before the lone "\n"
		"abc":       "\n",
	}
	for in, want := range cases {
		if got := detectLineEnding(in); got != want {
			t.Errorf("detectLineEnding(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitBom(t *testing.T) {
	bom, text := splitBom("\uFEFFhello")
	if bom != "\uFEFF" || text != "hello" {
		t.Errorf("splitBom = %q/%q", bom, text)
	}
	bom, text = splitBom("hello")
	if bom != "" || text != "hello" {
		t.Errorf("splitBom passthrough = %q/%q", bom, text)
	}
}

func TestNormalizeForFuzzyMatch(t *testing.T) {
	got := normalizeForFuzzyMatch("a  \n\u2018q\u2019 \u2013 \u00A0x")
	// Trailing whitespace stripped per line; smart quotes, dash and NBSP
	// folded to ASCII (edit-diff.ts:34-55). The NBSP becomes a space under
	// NFKC, so the space before it survives as a double space — that is
	// pi's behavior, not a bug.
	want := "a\n'q' -  x"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeForFuzzyMatchNFKC(t *testing.T) {
	// Full-width forms fold to ASCII under NFKC.
	if got := normalizeForFuzzyMatch("（ＡＢＣ）"); got != "(ABC)" {
		t.Errorf("NFKC not applied: %q", got)
	}
}

func TestFuzzyFindTextExactFirst(t *testing.T) {
	m := fuzzyFindText("hello world", "world")
	if !m.Found || m.UsedFuzzyMatch || m.Index != 6 || m.MatchLength != 5 {
		t.Fatalf("exact match broken: %+v", m)
	}
}

func TestFuzzyFindTextFuzzyFallback(t *testing.T) {
	m := fuzzyFindText("say \u201chi\u201d", `say "hi"`)
	if !m.Found || !m.UsedFuzzyMatch {
		t.Fatalf("fuzzy fallback broken: %+v", m)
	}
	if m.ContentForReplacement != `say "hi"` {
		t.Errorf("replacement content = %q", m.ContentForReplacement)
	}
}

func TestFuzzyFindTextNotFound(t *testing.T) {
	m := fuzzyFindText("abc", "xyz")
	if m.Found {
		t.Fatal("should not be found")
	}
}

func TestCountOccurrences(t *testing.T) {
	if n := countOccurrences("a\nb\na\nb\na", "a"); n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

func TestApplyEditsReverseOrderStability(t *testing.T) {
	// Two matches whose offsets would shift if applied forwards.
	res, err := applyEditsToNormalizedContent("aa bb cc", []Edit{
		{OldText: "aa", NewText: "AAAA"},
		{OldText: "cc", NewText: "CC"},
	}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if res.NewContent != "AAAA bb CC" {
		t.Errorf("newContent = %q", res.NewContent)
	}
}

func TestApplyEditsFuzzyOverlayPreservesUnchangedLines(t *testing.T) {
	// The file's indentation on untouched lines must survive a fuzzy match
	// on a different line.
	original := "func f() {\n\treturn 1   \n}\n"
	res, err := applyEditsToNormalizedContent(original, []Edit{
		{OldText: "return 1", NewText: "return 2"},
	}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if res.NewContent != "func f() {\n\treturn 2   \n}\n" {
		t.Errorf("unchanged line lost its bytes: %q", res.NewContent)
	}
}

func TestApplyReplacementsPreservingUnchangedLinesLineCountMismatch(t *testing.T) {
	_, err := applyReplacementsPreservingUnchangedLines("a\nb", "a\nb\nc", nil)
	if err == nil {
		t.Fatal("expected the line-count guard to fire")
	}
}

func TestSplitLinesWithEndings(t *testing.T) {
	got := splitLinesWithEndings("a\nbb\nc")
	if strings.Join(got, "|") != "a\n|bb\n|c" {
		t.Errorf("got %q", got)
	}
	if splitLinesWithEndings("") != nil {
		t.Error("empty content should split to nil")
	}
}

func TestGenerateUnifiedPatchHeader(t *testing.T) {
	patch := generateUnifiedPatch("f.go", "a\nb\nc\n", "a\nB\nc\n", 4)
	if !strings.HasPrefix(patch, "Index: f.go\n") {
		t.Errorf("missing jsdiff-style header:\n%s", patch)
	}
	if !strings.Contains(patch, "--- f.go\n+++ f.go\n") {
		t.Errorf("missing file headers:\n%s", patch)
	}
	if !strings.Contains(patch, "-b\n+B") {
		t.Errorf("hunk body wrong:\n%s", patch)
	}
}

func TestGenerateUnifiedPatchEmptyWhenIdentical(t *testing.T) {
	if p := generateUnifiedPatch("f", "x\n", "x\n", 4); p != "" {
		t.Errorf("patch = %q", p)
	}
}

func TestGenerateDiffStringFirstChangedLine(t *testing.T) {
	diff, first := generateDiffString("1\n2\n3\n4\n5\n", "1\n2\nX\n4\n5\n", 4)
	if first != 3 {
		t.Errorf("firstChangedLine = %d, want 3", first)
	}
	if !strings.Contains(diff, "+3 X") || !strings.Contains(diff, "-3 3") {
		t.Errorf("diff body:\n%s", diff)
	}
}

func TestGenerateDiffStringContextElision(t *testing.T) {
	// A change in the middle of a long unchanged run elides the middle.
	var oldB, newB strings.Builder
	for i := 1; i <= 20; i++ {
		oldB.WriteString("line\n")
		newB.WriteString("line\n")
		if i == 10 {
			newB.WriteString("CHANGED\n")
		}
	}
	diff, first := generateDiffString(oldB.String(), newB.String(), 2)
	if first != 11 {
		t.Errorf("firstChangedLine = %d, want 11", first)
	}
	if !strings.Contains(diff, " ...") {
		t.Errorf("elision marker missing:\n%s", diff)
	}
	if !strings.Contains(diff, "+11 CHANGED") {
		t.Errorf("change missing:\n%s", diff)
	}
}

func TestLcsOpsTrimsCommonAffixes(t *testing.T) {
	// Without prefix/suffix trimming this pair would build a huge table;
	// with it, the middle is tiny.
	prefix := strings.Repeat("p\n", 5000)
	suffix := strings.Repeat("s\n", 5000)
	a := prefix + "MID\n" + suffix
	b := prefix + "OTHER\n" + suffix
	ops, ok := lcsOps(splitLinesKeep(a), splitLinesKeep(b))
	if !ok {
		t.Fatal("ops should be computable")
	}
	changed := 0
	for _, op := range ops {
		if op.kind != ' ' {
			changed++
		}
	}
	if changed != 2 {
		t.Errorf("changed ops = %d, want 2", changed)
	}
}

func TestLcsOpsTooLargeFallsBack(t *testing.T) {
	// Two large files with no common affixes exceed the cell cap.
	a := strings.Repeat("a\n", 3000)
	b := strings.Repeat("b\n", 3000)
	if _, ok := lcsOps(splitLinesKeep(a), splitLinesKeep(b)); ok {
		t.Error("expected the cell cap to force the summary fallback")
	}
}

func TestBigramDice(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"gammq", "gamma", 0.75},
		{"same", "same", 1},
		{"a", "b", 0},
		{"abc", "xyz", 0},
	}
	for _, c := range cases {
		if got := bigramDice(c.a, c.b); got != c.want {
			t.Errorf("bigramDice(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
