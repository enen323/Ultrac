package tool

// Diff and fuzzy-match machinery for the edit tool, ported from pi's
// packages/coding-agent/src/core/tools/edit-diff.ts (557 lines).
//
// The port keeps pi's structure and its error strings verbatim: the error
// text is part of the tool's contract with the model, and the multi-edit
// variants name the failing index precisely ("Could not find edits[2] in
// …") so a batch failure is actionable.
//
// # Two deviations from the TypeScript
//
//  1. NFKC normalization uses golang.org/x/text/unicode/norm — Go's
//     standard library has no Unicode normalization. x/text was already in
//     the module graph (indirect, via jsonschema), so this promotes it to a
//     direct dependency rather than adding a download.
//  2. The unified patch and display diff run on a hand-rolled LCS engine
//     (ported from the pre-port builtin diff.go, with common
//     prefix/suffix trimming added) instead of the jsdiff package — plan
//     §5.2: "若已够用则不引". details.patch is log/UI data, never
//     model-facing content, so byte-exact jsdiff hunk formatting is not a
//     parity target.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ---------------------------------------------------------------------------
// Line endings and BOM
// ---------------------------------------------------------------------------

// detectLineEnding reports the dominant line ending by first occurrence
// (edit-diff.ts:11-17).
func detectLineEnding(content string) string {
	crlf := strings.Index(content, "\r\n")
	lf := strings.Index(content, "\n")
	if lf == -1 || crlf == -1 {
		return "\n"
	}
	if crlf < lf {
		return "\r\n"
	}
	return "\n"
}

// normalizeToLF folds CRLF and lone CR to LF (edit-diff.ts:19-21).
func normalizeToLF(text string) string {
	if !strings.ContainsAny(text, "\r") {
		return text
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// restoreLineEndings re-expands LF to the original ending (edit-diff.ts:23-25).
func restoreLineEndings(text, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// splitBom separates a leading UTF-8 BOM (utils/text.ts:2-4). The model
// never includes an invisible BOM in oldText, so matching happens on the
// text and the BOM is re-attached on write.
//
// The slice is by byte length, not by 1: U+FEFF is three UTF-8 bytes, and
// JavaScript's slice(1) counts UTF-16 code units where U+FEFF is one.
func splitBom(content string) (bom, text string) {
	const bomBytes = "\uFEFF"
	if strings.HasPrefix(content, bomBytes) {
		return bomBytes, content[len(bomBytes):]
	}
	return "", content
}

// ---------------------------------------------------------------------------
// Fuzzy normalization
// ---------------------------------------------------------------------------

// fuzzyNormalizer applies every character-level normalization in one pass.
//
// pi: edit-diff.ts:34-55 — NFKC, per-line trailing-whitespace strip, smart
// quotes to ASCII, Unicode dashes to "-", special spaces to " ".
var fuzzyNormalizer = strings.NewReplacer(
	"\u2018", "'", "\u2019", "'", "\u201A", "'", "\u201B", "'",
	"\u201C", `"`, "\u201D", `"`, "\u201E", `"`, "\u201F", `"`,
	"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-",
	"\u2014", "-", "\u2015", "-", "\u2212", "-",
	"\u00A0", " ", "\u2002", " ", "\u2003", " ", "\u2004", " ",
	"\u2005", " ", "\u2006", " ", "\u2007", " ", "\u2008", " ",
	"\u2009", " ", "\u200A", " ", "\u202F", " ", "\u205F", " ", "\u3000", " ",
)

// normalizeForFuzzyMatch normalizes text for fuzzy matching.
func normalizeForFuzzyMatch(text string) string {
	text = norm.NFKC.String(text)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		// JS trimEnd(): all Unicode whitespace. NFKC has already folded
		// the exotic spaces to plain ones, so this is mostly tabs/spaces.
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	return fuzzyNormalizer.Replace(strings.Join(lines, "\n"))
}

// ---------------------------------------------------------------------------
// Line spans and replacement application
// ---------------------------------------------------------------------------

// lineSpan is the byte range of one line, its terminator included.
type lineSpan struct {
	start, end int
}

// splitLinesWithEndings splits content into lines that keep their trailing
// newline (edit-diff.ts:57-59: content.match(/[^\n]*\n|[^\n]+/g) ?? []).
func splitLinesWithEndings(content string) []string {
	if content == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lines = append(lines, content[start:i+1])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

// getLineSpans computes each line's byte range (edit-diff.ts:75-82).
func getLineSpans(content string) []lineSpan {
	offset := 0
	lines := splitLinesWithEndings(content)
	spans := make([]lineSpan, 0, len(lines))
	for _, line := range lines {
		spans = append(spans, lineSpan{start: offset, end: offset + len(line)})
		offset = spans[len(spans)-1].end
	}
	return spans
}

// getReplacementLineRange widens a byte-range replacement to the lines it
// touches (edit-diff.ts:84-109). endLine is exclusive.
func getReplacementLineRange(lines []lineSpan, matchIndex, matchLength int) (startLine, endLine int, err error) {
	replacementEnd := matchIndex + matchLength
	startLine = -1
	for i, line := range lines {
		if matchIndex >= line.start && matchIndex < line.end {
			startLine = i
			break
		}
	}
	if startLine == -1 {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return 0, 0, fmt.Errorf("Replacement range is outside the base content.")
	}
	endLine = startLine
	for endLine < len(lines) && lines[endLine].end < replacementEnd {
		endLine++
	}
	if endLine >= len(lines) {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return 0, 0, fmt.Errorf("Replacement range is outside the base content.")
	}
	return startLine, endLine + 1, nil
}

// textReplacement is one match to apply.
type textReplacement struct {
	matchIndex  int
	matchLength int
	newText     string
}

// applyReplacements applies replacements to content from last to first, so
// earlier offsets stay valid (edit-diff.ts:111-120).
func applyReplacements(content string, replacements []textReplacement, offset int) string {
	result := content
	for i := len(replacements) - 1; i >= 0; i-- {
		r := replacements[i]
		idx := r.matchIndex - offset
		result = result[:idx] + r.newText + result[idx+r.matchLength:]
	}
	return result
}

// applyReplacementsPreservingUnchangedLines overlays normalized-space
// replacements onto the original content, so untouched lines keep their
// original bytes (edit-diff.ts:132-173).
//
// This is what makes a fuzzy match safe: the model's oldText matched a
// whitespace-normalized view, but the file's actual indentation must
// survive everywhere the edit did not touch.
func applyReplacementsPreservingUnchangedLines(originalContent, baseContent string, replacements []textReplacement) (string, error) {
	originalLines := splitLinesWithEndings(originalContent)
	baseLines := getLineSpans(baseContent)
	if len(originalLines) != len(baseLines) {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return "", fmt.Errorf("Cannot preserve unchanged lines because the base content has a different line count.")
	}

	type group struct {
		startLine, endLine int
		replacements       []textReplacement
	}
	var groups []group
	sorted := make([]textReplacement, len(replacements))
	copy(sorted, replacements)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].matchIndex < sorted[j].matchIndex })
	for _, r := range sorted {
		startLine, endLine, err := getReplacementLineRange(baseLines, r.matchIndex, r.matchLength)
		if err != nil {
			return "", err
		}
		if n := len(groups); n > 0 && startLine < groups[n-1].endLine {
			if endLine > groups[n-1].endLine {
				groups[n-1].endLine = endLine
			}
			groups[n-1].replacements = append(groups[n-1].replacements, r)
			continue
		}
		groups = append(groups, group{startLine: startLine, endLine: endLine, replacements: []textReplacement{r}})
	}

	var b strings.Builder
	originalLineIndex := 0
	for _, g := range groups {
		for _, line := range originalLines[originalLineIndex:g.startLine] {
			b.WriteString(line)
		}
		groupStart := baseLines[g.startLine].start
		groupEnd := baseLines[g.endLine-1].end
		b.WriteString(applyReplacements(baseContent[groupStart:groupEnd], g.replacements, groupStart))
		originalLineIndex = g.endLine
	}
	for _, line := range originalLines[originalLineIndex:] {
		b.WriteString(line)
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// Fuzzy finding
// ---------------------------------------------------------------------------

// FuzzyMatchResult reports where oldText was found and in which space
// (edit-diff.ts:175-189).
type FuzzyMatchResult struct {
	Found                 bool
	Index                 int
	MatchLength           int
	UsedFuzzyMatch        bool
	ContentForReplacement string
}

// fuzzyFindText tries an exact match first, then a normalized one
// (edit-diff.ts:207-245).
func fuzzyFindText(content, oldText string) FuzzyMatchResult {
	if idx := strings.Index(content, oldText); idx != -1 {
		return FuzzyMatchResult{
			Found: true, Index: idx, MatchLength: len(oldText),
			ContentForReplacement: content,
		}
	}
	fuzzyContent := normalizeForFuzzyMatch(content)
	fuzzyOld := normalizeForFuzzyMatch(oldText)
	if idx := strings.Index(fuzzyContent, fuzzyOld); idx != -1 {
		return FuzzyMatchResult{
			Found: true, Index: idx, MatchLength: len(fuzzyOld),
			UsedFuzzyMatch: true, ContentForReplacement: fuzzyContent,
		}
	}
	return FuzzyMatchResult{Found: false, Index: -1, ContentForReplacement: content}
}

// countOccurrences counts matches in normalized space (edit-diff.ts:247-251).
func countOccurrences(content, oldText string) int {
	return strings.Count(normalizeForFuzzyMatch(content), normalizeForFuzzyMatch(oldText))
}

// ---------------------------------------------------------------------------
// Error messages (verbatim from edit-diff.ts:253-289, plus the closest-
// fragment suffix the plan's P3 DoD requires)
// ---------------------------------------------------------------------------

func notFoundError(path string, editIndex, totalEdits int, content, oldText string) error {
	var msg string
	if totalEdits == 1 {
		msg = fmt.Sprintf("Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)
	} else {
		msg = fmt.Sprintf("Could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines.", editIndex, path)
	}
	// DoD addition (plan §3.4): "未找到时给出最接近片段". pi stops at the
	// generic message; a model that mis-transcribed one character of a
	// 40-line block gets no signal about where it went wrong. The closest
	// fragment names the best-matching window and how many of the old
	// text's lines appear there.
	if hint := closestFragmentHint(content, oldText); hint != "" {
		msg += "\n" + hint
	}
	return fmt.Errorf("%s", msg)
}

func duplicateError(path string, editIndex, totalEdits, occurrences int) error {
	if totalEdits == 1 {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return fmt.Errorf("Found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.", occurrences, path)
	}
	//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
	return fmt.Errorf("Found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique.", occurrences, editIndex, path)
}

func emptyOldTextError(path string, editIndex, totalEdits int) error {
	if totalEdits == 1 {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return fmt.Errorf("oldText must not be empty in %s.", path)
	}
	//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
	return fmt.Errorf("edits[%d].oldText must not be empty in %s.", editIndex, path)
}

func noChangeError(path string, totalEdits int) error {
	if totalEdits == 1 {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return fmt.Errorf("No changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.", path)
	}
	//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
	return fmt.Errorf("No changes made to %s. The replacements produced identical content.", path)
}

// ---------------------------------------------------------------------------
// The closest fragment (plan §3.4 DoD addition, not in pi)
// ---------------------------------------------------------------------------

// closestFragmentWindows caps the scan so a pathological file cannot turn
// an error message into a hang.
const closestFragmentWindows = 200_000

// closestFragmentHint renders the best-matching window of content for an
// oldText that was not found.
//
// Two tiers, because there are two failure shapes:
//
//   - Tier 1 (exact lines): a window's score is the number of DISTINCT
//     oldText lines that appear in it. Computed exactly in linear time:
//     each occurrence p of an oldText line covers the windows [p-w+1, p],
//     so a difference array over windows turns the whole scoring into one
//     pass over the file's lines. This is the high-confidence regime — a
//     model that mis-transcribed one line of a 40-line block still anchors
//     on the other 39.
//   - Tier 2 (fuzzy line): when NO line matches exactly — a single-line
//     oldText with a typo, say — the first oldText line is scored against
//     every file line with a character bigram Dice coefficient, and the
//     best line above the threshold anchors the window.
//
// Both tiers degrade to "no line appears anywhere" rather than showing an
// arbitrary window.
func closestFragmentHint(content, oldText string) string {
	oldLines := splitLinesForCounting(normalizeToLF(oldText))
	fileLines := splitLinesForCounting(content)
	if len(oldLines) == 0 || len(fileLines) == 0 {
		return ""
	}

	w := len(oldLines)
	if w > len(fileLines) {
		w = len(fileLines)
	}
	windows := len(fileLines) - w + 1
	if windows > closestFragmentWindows {
		windows = closestFragmentWindows
	}

	// --- Tier 1: exact line voting ---------------------------------------
	want := make(map[string]bool, len(oldLines))
	for _, line := range oldLines {
		want[line] = true
	}
	diff := make([]int, windows+1)
	seen := make(map[string]bool, len(want))
	for i := 0; i < len(fileLines); i++ {
		if !want[fileLines[i]] {
			continue
		}
		seen[fileLines[i]] = true
		lo := i - w + 1
		if lo < 0 {
			lo = 0
		}
		hi := i
		if hi >= windows {
			hi = windows - 1
		}
		diff[lo]++
		diff[hi+1]--
	}
	if len(seen) > 0 {
		best, bestScore, running := 0, -1, 0
		for i := 0; i < windows; i++ {
			running += diff[i]
			if running > bestScore {
				best, bestScore = i, running
			}
		}
		return renderClosestWindow(fileLines, best, w,
			fmt.Sprintf("Closest match at line %d: %d of %d lines from the old text appear here.", best+1, bestScore, len(oldLines)))
	}

	// --- Tier 2: fuzzy single-line anchor --------------------------------
	anchor := oldLines[0]
	for _, line := range oldLines {
		if len(line) > len(anchor) {
			anchor = line
		}
	}
	bestIdx, bestScore := -1, 0.0
	limit := len(fileLines)
	if limit > closestFragmentLines {
		limit = closestFragmentLines
	}
	for i := 0; i < limit; i++ {
		if score := bigramDice(anchor, fileLines[i]); score > bestScore {
			bestIdx, bestScore = i, score
		}
	}
	if bestIdx < 0 || bestScore < fuzzyAnchorThreshold {
		return "No line of the old text appears anywhere in the file."
	}
	pct := int(math.Round(bestScore * 100))
	return renderClosestWindow(fileLines, bestIdx, 1,
		fmt.Sprintf("Closest match at line %d (%d%% similar to the old text):", bestIdx+1, pct))
}

// closestFragmentLines caps the tier-2 scan.
const closestFragmentLines = 50_000

// fuzzyAnchorThreshold is the Dice score below which a "closest" line is
// noise rather than a hint.
const fuzzyAnchorThreshold = 0.5

// renderClosestWindow renders the message header plus the window of lines
// starting at from, capped so the message stays readable.
func renderClosestWindow(fileLines []string, from, w int, header string) string {
	const maxShown = 20
	to := from + w
	if to-from > maxShown {
		to = from + maxShown
	}
	if to > len(fileLines) {
		to = len(fileLines)
	}
	width := len(fmt.Sprintf("%d", to))
	var b strings.Builder
	b.WriteString(header)
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, "\n  %*d  %s", width, i+1, fileLines[i])
	}
	return b.String()
}

// bigramDice is the Sørensen–Dice coefficient over character bigrams:
// 2|A∩B| / (|A|+|B|). Cheap, order-sensitive enough to tell "gammq" from
// "gamma" (0.75) while scoring unrelated lines near zero.
func bigramDice(a, b string) float64 {
	if a == b {
		return 1
	}
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	counts := make(map[string]int, len(a))
	for i := 0; i+1 < len(a); i++ {
		counts[a[i:i+2]]++
	}
	shared := 0
	for i := 0; i+1 < len(b); i++ {
		if n := counts[b[i:i+2]]; n > 0 {
			counts[b[i:i+2]] = n - 1
			shared++
		}
	}
	total := (len(a) - 1) + (len(b) - 1)
	if total == 0 {
		return 0
	}
	return 2 * float64(shared) / float64(total)
}

// ---------------------------------------------------------------------------
// Applying a batch of edits
// ---------------------------------------------------------------------------

// Edit is one targeted replacement (edit-diff.ts:191-194).
type Edit struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// AppliedEditsResult is the base content and the edited result
// (edit-diff.ts:196-199).
type AppliedEditsResult struct {
	BaseContent string
	NewContent  string
}

// matchedEdit records where one edit matched (edit-diff.ts:65-73).
type matchedEdit struct {
	editIndex   int
	matchIndex  int
	matchLength int
	newText     string
}

// applyEditsToNormalizedContent applies every edit against the same
// original content (edit-diff.ts:300-362).
//
// All edits are matched against the original, then applied in reverse
// order so offsets stay stable. When any edit needed fuzzy matching, the
// batch runs in normalized space and the result is overlaid back onto the
// original so unchanged lines keep their bytes.
func applyEditsToNormalizedContent(normalizedContent string, edits []Edit, path string) (AppliedEditsResult, error) {
	normalized := make([]Edit, len(edits))
	for i, e := range edits {
		normalized[i] = Edit{OldText: normalizeToLF(e.OldText), NewText: normalizeToLF(e.NewText)}
	}
	for i := range normalized {
		if normalized[i].OldText == "" {
			return AppliedEditsResult{}, emptyOldTextError(path, i, len(normalized))
		}
	}

	initial := make([]FuzzyMatchResult, len(normalized))
	usedFuzzy := false
	for i := range normalized {
		initial[i] = fuzzyFindText(normalizedContent, normalized[i].OldText)
		if initial[i].UsedFuzzyMatch {
			usedFuzzy = true
		}
	}
	base := normalizedContent
	if usedFuzzy {
		base = normalizeForFuzzyMatch(normalizedContent)
	}

	var matched []matchedEdit
	for i := range normalized {
		m := fuzzyFindText(base, normalized[i].OldText)
		if !m.Found {
			return AppliedEditsResult{}, notFoundError(path, i, len(normalized), normalizedContent, normalized[i].OldText)
		}
		if n := countOccurrences(base, normalized[i].OldText); n > 1 {
			return AppliedEditsResult{}, duplicateError(path, i, len(normalized), n)
		}
		matched = append(matched, matchedEdit{
			editIndex: i, matchIndex: m.Index, matchLength: m.MatchLength,
			newText: normalized[i].NewText,
		})
	}

	sort.SliceStable(matched, func(i, j int) bool { return matched[i].matchIndex < matched[j].matchIndex })
	for i := 1; i < len(matched); i++ {
		prev, cur := matched[i-1], matched[i]
		if prev.matchIndex+prev.matchLength > cur.matchIndex {
			//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
			return AppliedEditsResult{}, fmt.Errorf(
				"edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions.",
				prev.editIndex, cur.editIndex, path)
		}
	}

	baseContent := normalizedContent
	var newContent string
	if usedFuzzy {
		overlaid, err := applyReplacementsPreservingUnchangedLines(normalizedContent, base, toTextReplacements(matched))
		if err != nil {
			return AppliedEditsResult{}, err
		}
		newContent = overlaid
	} else {
		newContent = applyReplacements(base, toTextReplacements(matched), 0)
	}
	if baseContent == newContent {
		return AppliedEditsResult{}, noChangeError(path, len(normalized))
	}
	return AppliedEditsResult{BaseContent: baseContent, NewContent: newContent}, nil
}

func toTextReplacements(matched []matchedEdit) []textReplacement {
	out := make([]textReplacement, len(matched))
	for i, m := range matched {
		out[i] = textReplacement{matchIndex: m.matchIndex, matchLength: m.matchLength, newText: m.newText}
	}
	return out
}

// ---------------------------------------------------------------------------
// Diff rendering
// ---------------------------------------------------------------------------

// diffMaxCells bounds the LCS table (kept from the pre-port builtin
// diff.go): two 2000-line files is 4M cells and fine; two 20000-line files
// is 400M and not.
const diffMaxCells = 4_000_000

// generateUnifiedPatch renders a standard unified patch with a jsdiff-style
// header (edit-diff.ts:365-370).
func generateUnifiedPatch(path, oldContent, newContent string, contextLines int) string {
	if oldContent == newContent {
		return ""
	}
	ops, ok := lcsOps(splitLinesKeep(oldContent), splitLinesKeep(newContent))
	if !ok {
		return summaryPatch(splitLinesKeep(oldContent), splitLinesKeep(newContent))
	}
	var b strings.Builder
	b.WriteString("Index: " + path + "\n")
	b.WriteString(strings.Repeat("=", 67) + "\n")
	b.WriteString("--- " + path + "\n")
	b.WriteString("+++ " + path + "\n")
	b.WriteString(renderHunks(ops, contextLines))
	return b.String()
}

// generateDiffString renders the display diff with line numbers and
// context elision (edit-diff.ts:376-499).
func generateDiffString(oldContent, newContent string, contextLines int) (diff string, firstChangedLine int) {
	parts := diffParts(splitLinesKeep(oldContent), splitLinesKeep(newContent))
	if len(parts) == 0 {
		return "", 0
	}

	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	maxLineNum := len(oldLines)
	if len(newLines) > maxLineNum {
		maxLineNum = len(newLines)
	}
	width := len(fmt.Sprintf("%d", maxLineNum))

	oldNum, newNum := 1, 1
	lastWasChange := false
	firstChanged := 0
	var out []string

	pad := func(n int) string { return fmt.Sprintf("%*d", width, n) }
	for i, part := range parts {
		raw := part.lines
		if len(raw) > 0 && raw[len(raw)-1] == "" {
			raw = raw[:len(raw)-1]
		}
		if part.added || part.removed {
			if firstChanged == 0 {
				firstChanged = newNum
			}
			for _, line := range raw {
				if part.added {
					out = append(out, "+"+pad(newNum)+" "+line)
					newNum++
				} else {
					out = append(out, "-"+pad(oldNum)+" "+line)
					oldNum++
				}
			}
			lastWasChange = true
			continue
		}
		nextIsChange := i < len(parts)-1 && (parts[i+1].added || parts[i+1].removed)
		switch {
		case lastWasChange && nextIsChange:
			if len(raw) <= contextLines*2 {
				for _, line := range raw {
					out = append(out, " "+pad(oldNum)+" "+line)
					oldNum++
					newNum++
				}
			} else {
				for _, line := range raw[:contextLines] {
					out = append(out, " "+pad(oldNum)+" "+line)
					oldNum++
					newNum++
				}
				out = append(out, " "+strings.Repeat(" ", width)+" ...")
				oldNum += len(raw) - 2*contextLines
				newNum += len(raw) - 2*contextLines
				for _, line := range raw[len(raw)-contextLines:] {
					out = append(out, " "+pad(oldNum)+" "+line)
					oldNum++
					newNum++
				}
			}
		case lastWasChange:
			for _, line := range raw[:min(contextLines, len(raw))] {
				out = append(out, " "+pad(oldNum)+" "+line)
				oldNum++
				newNum++
			}
			if skipped := len(raw) - contextLines; skipped > 0 {
				out = append(out, " "+strings.Repeat(" ", width)+" ...")
				oldNum += skipped
				newNum += skipped
			}
		case nextIsChange:
			skipped := len(raw) - contextLines
			if skipped < 0 {
				skipped = 0
			}
			if skipped > 0 {
				out = append(out, " "+strings.Repeat(" ", width)+" ...")
				oldNum += skipped
				newNum += skipped
			}
			for _, line := range raw[skipped:] {
				out = append(out, " "+pad(oldNum)+" "+line)
				oldNum++
				newNum++
			}
		default:
			oldNum += len(raw)
			newNum += len(raw)
		}
		lastWasChange = false
	}
	return strings.Join(out, "\n"), firstChanged
}

// ---------------------------------------------------------------------------
// The LCS diff engine (ported from the pre-port builtin/diff.go, with
// common prefix/suffix trimming added so the table stays small for the
// localized edits that actually occur)
// ---------------------------------------------------------------------------

// splitLinesKeep splits like pi's diffLines input: every "\n" starts a new
// element and a trailing newline yields a final "" element (JS
// String.split semantics).
func splitLinesKeep(s string) []string { return strings.Split(s, "\n") }

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

type diffPart struct {
	added, removed bool
	lines          []string
}

// diffParts groups the edit script into runs (pi feeds these to its
// renderer as Diff.diffLines parts).
func diffParts(a, b []string) []diffPart {
	ops, ok := lcsOps(a, b)
	if !ok {
		return nil
	}
	var parts []diffPart
	for _, op := range ops {
		if n := len(parts); n > 0 {
			last := &parts[n-1]
			if (op.kind == '+' && last.added) || (op.kind == '-' && last.removed) || (op.kind == ' ' && !last.added && !last.removed) {
				last.lines = append(last.lines, op.text)
				continue
			}
		}
		parts = append(parts, diffPart{
			added:   op.kind == '+',
			removed: op.kind == '-',
			lines:   []string{op.text},
		})
	}
	return parts
}

// lcsOps produces a minimal edit script. ok is false when the inputs are
// too large for the table even after trimming — callers fall back to a
// summary rather than exhausting memory.
func lcsOps(a, b []string) ([]diffOp, bool) {
	// Common prefix.
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	// Common suffix.
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	am, bm := a[p:len(a)-s], b[p:len(b)-s]
	if len(am)*len(bm) > diffMaxCells {
		return nil, false
	}

	ops := make([]diffOp, 0, p+s+len(am)+len(bm))
	for _, line := range a[:p] {
		ops = append(ops, diffOp{' ', line})
	}
	ops = append(ops, lcsOpsCore(am, bm)...)
	for _, line := range a[len(a)-s:] {
		ops = append(ops, diffOp{' ', line})
	}
	return ops, true
}

// lcsOpsCore is the table-driven core over the trimmed middles.
func lcsOpsCore(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	// table[i][j] = LCS length of a[i:] and b[j:]
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// renderHunks groups the edit script into unified-diff hunks with limited
// context (ported from builtin/diff.go:95-172).
func renderHunks(ops []diffOp, contextLines int) string {
	const maxContext = 4
	if contextLines <= 0 || contextLines > maxContext {
		contextLines = maxContext
	}

	var changed []int
	for i, op := range ops {
		if op.kind != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return ""
	}

	type group struct{ from, to int }
	var groups []group
	cur := group{changed[0], changed[0]}
	for _, idx := range changed[1:] {
		if idx-cur.to <= 2*contextLines+1 {
			cur.to = idx
			continue
		}
		groups = append(groups, cur)
		cur = group{idx, idx}
	}
	groups = append(groups, cur)

	oldBefore := make([]int, len(ops)+1)
	newBefore := make([]int, len(ops)+1)
	for i, op := range ops {
		oldBefore[i+1] = oldBefore[i]
		newBefore[i+1] = newBefore[i]
		if op.kind != '+' {
			oldBefore[i+1]++
		}
		if op.kind != '-' {
			newBefore[i+1]++
		}
	}

	var b strings.Builder
	for _, g := range groups {
		from := g.from - contextLines
		if from < 0 {
			from = 0
		}
		to := g.to + contextLines
		if to > len(ops)-1 {
			to = len(ops) - 1
		}
		oldStart, oldLen := hunkRange(oldBefore, from, to)
		newStart, newLen := hunkRange(newBefore, from, to)
		b.WriteString(fmt.Sprintf("@@ -%s +%s @@\n", hunkHeader(oldStart, oldLen), hunkHeader(newStart, newLen)))
		for i := from; i <= to; i++ {
			b.WriteByte(ops[i].kind)
			b.WriteString(ops[i].text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// hunkRange computes a hunk's start line and length. An empty side starts
// at the line before the hunk, per the unified-diff convention.
func hunkRange(before []int, from, to int) (start, length int) {
	length = before[to+1] - before[from]
	if length == 0 {
		return before[from], 0
	}
	return before[from] + 1, length
}

// hunkHeader renders "-start,length", omitting ",length" when it is 1
// (jsdiff's convention).
func hunkHeader(start, length int) string {
	if length == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, length)
}

// summaryPatch is the fallback for inputs too large to diff exactly.
func summaryPatch(oldLines, newLines []string) string {
	out := fmt.Sprintf("[patch omitted: %d lines before, %d after]", len(oldLines), len(newLines))
	if len(oldLines) > len(newLines) {
		out += fmt.Sprintf(", %d lines shorter", len(oldLines)-len(newLines))
	}
	return out + "\n"
}
