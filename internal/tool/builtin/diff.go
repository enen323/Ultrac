package builtin

import (
	"strings"
)

// diffMaxCells bounds the dynamic-programming table.
//
// The LCS table is O(n·m) in both time and memory. Two 2000-line files is four
// million cells, which is fine; two 20000-line files is four hundred million,
// which is not. Past the cap the diff degrades to a summary rather than
// hanging — a slow tool is worse than an imprecise one here, because the model
// only needs the diff to confirm the edit landed where it meant.
const diffMaxCells = 4_000_000

// unifiedDiff renders a unified diff between two texts.
//
// Written by hand rather than pulled in, because the only consumer is a tool
// result that a human or a model reads to confirm a change. It needs to be
// correct and small, not to reproduce git's exact hunk-splitting heuristics.
func unifiedDiff(oldText, newText string, contextLines int) string {
	if oldText == newText {
		return ""
	}
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)

	if len(oldLines)*len(newLines) > diffMaxCells {
		return summaryDiff(oldLines, newLines)
	}

	ops := lcsOps(oldLines, newLines)
	return renderHunks(ops, contextLines)
}

// diffOp is one line of the edit script.
type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

// lcsOps produces a minimal edit script by longest-common-subsequence.
func lcsOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
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

	ops := make([]diffOp, 0, n+m)
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

// renderHunks groups the edit script into hunks with limited surrounding
// context, the way a unified diff does.
//
// Two passes: first find which ops changed and merge nearby changes into
// groups, then render each group. Line numbers come from prefix sums over the
// script rather than from state carried through the walk — the earlier version
// carried counters and had to rewind them when a hunk closed, which is how
// off-by-context bugs get in.
func renderHunks(ops []diffOp, contextLines int) string {
	const maxContext = 3
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

	// oldBefore[i] is the number of a-lines consumed before op i; likewise for
	// newBefore. They turn "which line is this" into a lookup.
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

		oldStart := oldBefore[from] + 1
		newStart := newBefore[from] + 1
		oldLen := oldBefore[to+1] - oldBefore[from]
		newLen := newBefore[to+1] - newBefore[from]

		b.WriteString("@@ -")
		b.WriteString(itoa(oldStart))
		b.WriteString(",")
		b.WriteString(itoa(oldLen))
		b.WriteString(" +")
		b.WriteString(itoa(newStart))
		b.WriteString(",")
		b.WriteString(itoa(newLen))
		b.WriteString(" @@\n")

		for i := from; i <= to; i++ {
			b.WriteByte(ops[i].kind)
			b.WriteString(ops[i].text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// summaryDiff is the fallback for inputs too large to diff exactly.
func summaryDiff(oldLines, newLines []string) string {
	changed := 0
	if len(oldLines) > len(newLines) {
		changed = len(oldLines) - len(newLines)
	}
	out := "[diff omitted: " + itoa(len(oldLines)) + " lines before, " +
		itoa(len(newLines)) + " after"
	if changed > 0 {
		out += ", " + itoa(changed) + " lines shorter"
	}
	return out + "]\n"
}
