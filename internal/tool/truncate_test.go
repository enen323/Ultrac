package tool

// Tests for the truncation primitives. The notices these produce are
// quoted verbatim in tool outputs the model reads, so the expected strings
// here are the contract.

import (
	"strings"
	"testing"
)

func TestFormatSize(t *testing.T) {
	cases := []struct {
		bytes int
		want  string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		// The tie case: 1280/1024 = 1.25 exactly. JS toFixed(1) picks the
		// larger n ("1.3"); Go's default round-half-to-even would say
		// "1.2". formatSize must round like JavaScript.
		{1280, "1.3KB"},
		{50 * 1024, "50.0KB"},
		{1024 * 1024, "1.0MB"},
		{1536 * 1024, "1.5MB"},
	}
	for _, c := range cases {
		if got := formatSize(c.bytes); got != c.want {
			t.Errorf("formatSize(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

func TestTruncateHeadNoTruncation(t *testing.T) {
	tr := truncateHead("a\nb\nc", TruncationOptions{})
	if tr.Truncated || tr.Content != "a\nb\nc" {
		t.Fatalf("unexpected result: %+v", tr)
	}
	if tr.TotalLines != 3 || tr.OutputLines != 3 || tr.TotalBytes != 5 {
		t.Fatalf("counters wrong: %+v", tr)
	}
}

func TestTruncateHeadByLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2500; i++ {
		b.WriteString("line\n")
	}
	tr := truncateHead(b.String(), TruncationOptions{})
	if !tr.Truncated || tr.TruncatedBy != "lines" {
		t.Fatalf("want lines truncation, got %+v", tr)
	}
	if tr.OutputLines != defaultMaxLines {
		t.Errorf("output lines = %d, want %d", tr.OutputLines, defaultMaxLines)
	}
	if tr.TotalLines != 2500 {
		t.Errorf("total lines = %d, want 2500", tr.TotalLines)
	}
	if strings.Count(tr.Content, "\n") != defaultMaxLines-1 {
		t.Errorf("content has %d newlines, want %d", strings.Count(tr.Content, "\n"), defaultMaxLines-1)
	}
}

func TestTruncateHeadByBytes(t *testing.T) {
	// 2000 short lines fit the line limit but blow the byte limit.
	line := strings.Repeat("x", 100)
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString(line + "\n")
	}
	tr := truncateHead(b.String(), TruncationOptions{})
	if !tr.Truncated || tr.TruncatedBy != "bytes" {
		t.Fatalf("want bytes truncation, got %+v", tr)
	}
	if tr.OutputBytes > defaultMaxBytes {
		t.Errorf("output bytes %d exceeds limit %d", tr.OutputBytes, defaultMaxBytes)
	}
}

func TestTruncateHeadFirstLineExceeds(t *testing.T) {
	huge := strings.Repeat("y", defaultMaxBytes+10)
	tr := truncateHead(huge+"\nnext", TruncationOptions{})
	if !tr.Truncated || !tr.FirstLineExceeds {
		t.Fatalf("want firstLineExceedsLimit, got %+v", tr)
	}
	if tr.Content != "" {
		t.Errorf("content = %q, want empty", tr.Content)
	}
}

func TestTruncateTailKeepsEnd(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		b.WriteString("line\n")
	}
	tr := truncateTail(b.String(), TruncationOptions{})
	if !tr.Truncated || tr.TruncatedBy != "lines" {
		t.Fatalf("want lines truncation, got %+v", tr)
	}
	if tr.OutputLines != defaultMaxLines {
		t.Errorf("output lines = %d, want %d", tr.OutputLines, defaultMaxLines)
	}
	// The kept content must be the LAST lines.
	if !strings.HasSuffix(tr.Content, "line") {
		t.Errorf("tail does not end with the last line: %q", tr.Content[len(tr.Content)-20:])
	}
	lines := strings.Split(tr.Content, "\n")
	if lines[len(lines)-1] != "line" || lines[0] != "line" {
		t.Errorf("unexpected tail content")
	}
}

func TestTruncateTailLastLinePartial(t *testing.T) {
	// A single line larger than the whole byte limit.
	huge := strings.Repeat("z", defaultMaxBytes+100)
	tr := truncateTail(huge, TruncationOptions{})
	if !tr.Truncated || !tr.LastLinePartial {
		t.Fatalf("want lastLinePartial, got %+v", tr)
	}
	if len(tr.Content) > defaultMaxBytes {
		t.Errorf("partial line is %d bytes, over the %d limit", len(tr.Content), defaultMaxBytes)
	}
	if !strings.HasSuffix(tr.Content, strings.Repeat("z", 10)) {
		t.Error("partial line is not the END of the original line")
	}
}

func TestTruncateTailNoTruncation(t *testing.T) {
	tr := truncateTail("only\nlines", TruncationOptions{})
	if tr.Truncated || tr.Content != "only\nlines" {
		t.Fatalf("unexpected result: %+v", tr)
	}
}

func TestTruncateTailUTF8Boundary(t *testing.T) {
	// A multi-byte rune straddling the cut must not be split.
	huge := strings.Repeat("界", defaultMaxBytes/3+50)
	tr := truncateTail(huge, TruncationOptions{})
	// The kept content must START on a character boundary: a leading
	// continuation byte would mean the cut landed mid-rune.
	if len(tr.Content) > 0 && tr.Content[0]&0xC0 == 0x80 {
		t.Fatal("content starts mid-rune")
	}
	// And it must be the END of the original line.
	if !strings.HasSuffix(tr.Content, strings.Repeat("界", 10)) {
		t.Error("partial line is not the END of the original line")
	}
}

func TestTruncateLine(t *testing.T) {
	text, cut := truncateLine("short", 500)
	if cut || text != "short" {
		t.Fatalf("short line changed: %q %v", text, cut)
	}
	long := strings.Repeat("a", 600)
	text, cut = truncateLine(long, 500)
	if !cut {
		t.Fatal("long line not truncated")
	}
	if text != strings.Repeat("a", 500)+"... [truncated]" {
		t.Errorf("unexpected truncated form: %q", text[:20])
	}
}

func TestTruncateLineCountsRunes(t *testing.T) {
	// 600 two-byte runes: 1200 bytes but 600 characters — the character
	// budget is what applies.
	line := strings.Repeat("界", 600)
	text, cut := truncateLine(line, 500)
	if !cut {
		t.Fatal("want truncation")
	}
	if n := len([]rune(strings.TrimSuffix(text, "... [truncated]"))); n != 500 {
		t.Errorf("kept %d runes, want 500", n)
	}
}
