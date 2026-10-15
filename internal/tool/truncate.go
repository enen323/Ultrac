package tool

// Truncation primitives, ported from pi's
// packages/coding-agent/src/core/tools/truncate.ts (315 lines).
//
// Two independent limits — whichever is hit first wins:
//   - lines (default 2000)
//   - bytes  (default 50KB)
//
// Head truncation keeps the beginning (file reads); tail truncation keeps
// the end (command output, where the error usually is). Neither returns a
// partial line, except tail truncation's documented edge case where the
// single last line alone exceeds the byte limit.

import (
	"math"
	"strconv"
	"strings"
)

// Default limits. DEFAULT_MAX_BYTES / 1024 = 50 appears verbatim in several
// tool descriptions, so it is computed, not repeated.
const (
	defaultMaxLines = 2000
	defaultMaxBytes = 50 * 1024

	// grepMaxLineLength caps one rendered match line (truncate.ts:13).
	grepMaxLineLength = 500
)

// TruncationResult is the full accounting of a truncate* call.
//
// pi: truncate.ts:15-38.
type TruncationResult struct {
	Content          string
	Truncated        bool
	TruncatedBy      string // "lines" | "bytes" | "" (empty = not truncated)
	TotalLines       int
	TotalBytes       int
	OutputLines      int
	OutputBytes      int
	LastLinePartial  bool
	FirstLineExceeds bool
	MaxLines         int
	MaxBytes         int
}

// TruncationOptions overrides the defaults.
//
// pi: truncate.ts:40-45.
type TruncationOptions struct {
	MaxLines int
	MaxBytes int
}

func (o TruncationOptions) maxLines() int {
	if o.MaxLines > 0 {
		return o.MaxLines
	}
	return defaultMaxLines
}

func (o TruncationOptions) maxBytes() int {
	if o.MaxBytes > 0 {
		return o.MaxBytes
	}
	return defaultMaxBytes
}

// splitLinesForCounting splits on "\n" and drops the trailing empty element
// a final newline produces, so "a\n" is one line, not two.
//
// pi: truncate.ts:47-56.
func splitLinesForCounting(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// formatSize renders bytes the way pi's formatSize does (truncate.ts:61-69):
// "512B", "1.5KB", "2.3MB".
//
// The rounding is deliberately not strconv's default: JavaScript's
// toFixed(1) resolves ties by picking the larger n (ECMA-262
// Number.prototype.toFixed step 5), so 256 bytes formats as "0.3KB", while
// Go's round-half-to-even would say "0.2KB". math.Round rounds half away
// from zero, which matches. The multiplication is exact for our inputs
// (bytes/1024 and bytes/1048576 both scale by a power of two), so no
// floating-point drift can flip a tie.
func formatSize(bytes int) string {
	switch {
	case bytes < 1024:
		return strconv.Itoa(bytes) + "B"
	case bytes < 1024*1024:
		return strconv.FormatFloat(round1(float64(bytes)/1024), 'f', 1, 64) + "KB"
	default:
		return strconv.FormatFloat(round1(float64(bytes)/(1024*1024)), 'f', 1, 64) + "MB"
	}
}

// round1 rounds to one decimal, ties away from zero (JS toFixed semantics).
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// truncateHead keeps the first maxLines lines / maxBytes bytes.
//
// Never returns partial lines. If the first line alone exceeds the byte
// limit, the content is empty and FirstLineExceeds is set — the caller
// points the model at a bash fallback instead of showing nothing.
//
// pi: truncate.ts:78-160.
func truncateHead(content string, opts TruncationOptions) TruncationResult {
	maxLines, maxBytes := opts.maxLines(), opts.maxBytes()
	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return TruncationResult{
			Content: content, TruncatedBy: "",
			TotalLines: totalLines, TotalBytes: totalBytes,
			OutputLines: totalLines, OutputBytes: totalBytes,
			MaxLines: maxLines, MaxBytes: maxBytes,
		}
	}

	if len(lines[0]) > maxBytes {
		return TruncationResult{
			Content: "", Truncated: true, TruncatedBy: "bytes",
			TotalLines: totalLines, TotalBytes: totalBytes,
			FirstLineExceeds: true,
			MaxLines:         maxLines, MaxBytes: maxBytes,
		}
	}

	out := make([]string, 0, maxLines)
	outBytes := 0
	truncatedBy := "lines"
	for i := 0; i < len(lines) && i < maxLines; i++ {
		lineBytes := len(lines[i])
		if i > 0 {
			lineBytes++ // the newline that joins it
		}
		if outBytes+lineBytes > maxBytes {
			truncatedBy = "bytes"
			break
		}
		out = append(out, lines[i])
		outBytes += lineBytes
	}
	if len(out) >= maxLines && outBytes <= maxBytes {
		truncatedBy = "lines"
	}

	output := strings.Join(out, "\n")
	return TruncationResult{
		Content: output, Truncated: true, TruncatedBy: truncatedBy,
		TotalLines: totalLines, TotalBytes: totalBytes,
		OutputLines: len(out), OutputBytes: len(output),
		MaxLines: maxLines, MaxBytes: maxBytes,
	}
}

// truncateTail keeps the last maxLines lines / maxBytes bytes.
//
// May return a partial first line in one edge case: when the final line of
// the original content alone exceeds maxBytes, its end is kept and
// LastLinePartial is set.
//
// pi: truncate.ts:168-241.
func truncateTail(content string, opts TruncationOptions) TruncationResult {
	maxLines, maxBytes := opts.maxLines(), opts.maxBytes()
	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return TruncationResult{
			Content: content, TruncatedBy: "",
			TotalLines: totalLines, TotalBytes: totalBytes,
			OutputLines: totalLines, OutputBytes: totalBytes,
			MaxLines: maxLines, MaxBytes: maxBytes,
		}
	}

	// Walk backwards collecting whole lines.
	collected := make([]string, 0, maxLines)
	collectedBytes := 0
	truncatedBy := "lines"
	lastLinePartial := false
	for i := len(lines) - 1; i >= 0 && len(collected) < maxLines; i-- {
		lineBytes := len(lines[i])
		if len(collected) > 0 {
			lineBytes++ // the newline that joins it
		}
		if collectedBytes+lineBytes > maxBytes {
			truncatedBy = "bytes"
			if len(collected) == 0 {
				// Nothing fits: keep the end of this one line, cut at a
				// UTF-8 character boundary.
				kept := truncateStringToBytesFromEnd(lines[i], maxBytes)
				collected = append(collected, kept)
				collectedBytes = len(kept)
				lastLinePartial = true
			}
			break
		}
		collected = append(collected, lines[i])
		collectedBytes += lineBytes
	}
	if len(collected) >= maxLines && collectedBytes <= maxBytes {
		truncatedBy = "lines"
	}

	// collected is in reverse order; restore it.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	output := strings.Join(collected, "\n")
	return TruncationResult{
		Content: output, Truncated: true, TruncatedBy: truncatedBy,
		TotalLines: totalLines, TotalBytes: totalBytes,
		OutputLines: len(collected), OutputBytes: len(output),
		LastLinePartial: lastLinePartial,
		MaxLines:        maxLines, MaxBytes: maxBytes,
	}
}

// truncateStringToBytesFromEnd keeps the last maxBytes bytes of s, cut at a
// UTF-8 character boundary (never mid-rune).
//
// pi: truncate.ts:247-262.
func truncateStringToBytesFromEnd(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	// Continuation bytes (10xxxxxx) are not character starts; step forward
	// until one is.
	for start < len(s) && s[start]&0xC0 == 0x80 {
		start++
	}
	return s[start:]
}

// truncateLine caps one grep match line, marking the cut.
//
// Counted in runes, not UTF-16 code units: pi's line.slice counts code
// units, but a Go port that split a surrogate pair would produce invalid
// text, and the 500-char budget is a display budget either way.
//
// pi: truncate.ts:268-276.
func truncateLine(line string, maxChars int) (text string, wasTruncated bool) {
	if maxChars <= 0 {
		maxChars = grepMaxLineLength
	}
	runes := []rune(line)
	if len(runes) <= maxChars {
		return line, false
	}
	return string(runes[:maxChars]) + "... [truncated]", true
}

// noLineLimit is the stand-in for pi's Number.MAX_SAFE_INTEGER: ls, grep
// and find cap rows by their own counters and only need the byte limit
// from truncateHead.
var noLineLimit = math.MaxInt
