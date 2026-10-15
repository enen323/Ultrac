package tool

// The grep tool, ported from pi's
// packages/coding-agent/src/core/tools/grep.ts (324 lines).
//
// # Why this shells out to ripgrep
//
// The pre-port engine walked the filesystem itself with a Go walker and a
// hardcoded ignore list, which does not respect .gitignore and formats
// results differently from pi. The plan (§3.4 DoD, §5.1, R12) puts ripgrep
// back as an external dependency — with one difference from pi: pi
// auto-downloads a managed binary when rg is missing, this project does
// not, so a missing rg produces an install hint instead.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// lookPath resolves an external tool on PATH. It is a variable so tests can
// force the not-installed path deterministically (rg is not present in
// every environment — including the one this was developed on).
var lookPath = exec.LookPath

// ripgrepInstallHint is the message when rg is missing. pi's text claims a
// download was attempted; this project never downloads (plan §5.1).
const ripgrepInstallHint = "ripgrep (rg) is not installed. Install it (https://github.com/BurntSushi/ripgrep#installation) and make sure rg is on PATH."

// GrepInput mirrors pi's grepSchema (grep.ts:21-33).
type GrepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	IgnoreCase *bool  `json:"ignoreCase,omitempty"`
	Literal    *bool  `json:"literal,omitempty"`
	Context    *int   `json:"context,omitempty"`
	Limit      *int   `json:"limit,omitempty"`
}

const grepSchema = `{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Search pattern (regex or literal string)"
    },
    "path": {
      "type": "string",
      "description": "Directory or file to search (default: current directory)"
    },
    "glob": {
      "type": "string",
      "description": "Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'"
    },
    "ignoreCase": {
      "type": "boolean",
      "description": "Case-insensitive search (default: false)"
    },
    "literal": {
      "type": "boolean",
      "description": "Treat pattern as literal string instead of regex (default: false)"
    },
    "context": {
      "type": "number",
      "description": "Number of lines to show before and after each match (default: 0)"
    },
    "limit": {
      "type": "number",
      "description": "Maximum number of matches to return (default: 100)"
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`

// grepDefaultLimit is pi's DEFAULT_LIMIT (grep.ts:41).
const grepDefaultLimit = 100

// GrepDetails is the tool's Details payload (grep.ts:43-47).
type GrepDetails struct {
	Truncation        *TruncationResult `json:"truncation,omitempty"`
	MatchLimitReached int               `json:"matchLimitReached,omitempty"`
	LinesTruncated    bool              `json:"linesTruncated,omitempty"`
}

// NewGrepTool builds the grep tool for cwd.
func NewGrepTool(cwd string) *agent.AgentTool[GrepInput] {
	return &agent.AgentTool[GrepInput]{
		Tool: ai.Tool{
			Name: "grep",
			// pi: grep.ts:78.
			Description: fmt.Sprintf(
				"Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output is truncated to %d matches or %dKB (whichever is hit first). Long lines are truncated to %d chars.",
				grepDefaultLimit, defaultMaxBytes/1024, grepMaxLineLength),
			Parameters: json.RawMessage(grepSchema),
		},
		Label: "grep",
		Execute: func(ctx context.Context, _ string, in GrepInput, _ func(agent.Result)) (agent.Result, error) {
			return executeGrep(ctx, cwd, in), nil
		},
	}
}

func init() {
	Register("grep", func(cwd string) agent.Tool { return agent.Erase(NewGrepTool(cwd)) })
}

// rgMatchEvent is one ripgrep --json event (grep.ts:227-238).
type rgMatchEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       struct{ Text string } `json:"path"`
		Lines      struct{ Text string } `json:"lines"`
		LineNumber int                   `json:"line_number"`
	} `json:"data"`
}

// grepMatch is one collected match before formatting (grep.ts:218).
type grepMatch struct {
	filePath string
	lineNum  int
	lineText string
}

func executeGrep(ctx context.Context, cwd string, in GrepInput) agent.Result {
	if in.Pattern == "" {
		return errResult("grep: pattern is required")
	}
	rgPath, err := lookPath("rg")
	if err != nil {
		return errResult(ripgrepInstallHint)
	}
	searchPath := resolveToCwd(orDefault(in.Path, "."), cwd)

	// pi's ops.isDirectory throws for a missing path, which becomes
	// "Path not found: {searchPath}" (grep.ts:128-132).
	info, statErr := os.Stat(searchPath)
	if statErr != nil {
		return errResult("Path not found: " + searchPath)
	}
	isDirectory := info.IsDir()

	contextValue := 0
	if in.Context != nil && *in.Context > 0 {
		contextValue = *in.Context
	}
	effectiveLimit := grepDefaultLimit
	if in.Limit != nil && *in.Limit > 0 {
		effectiveLimit = *in.Limit
	}

	// --- spawn rg (grep.ts:162-192) --------------------------------------
	args := []string{"--json", "--line-number", "--color=never", "--hidden"}
	if in.IgnoreCase != nil && *in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if in.Literal != nil && *in.Literal {
		args = append(args, "--fixed-strings")
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	args = append(args, "--", in.Pattern, searchPath)

	cmd := exec.Command(rgPath, args...)
	cmd.Dir = cwd
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errResult(fmt.Sprintf("Failed to run ripgrep: %v", err))
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return errResult(fmt.Sprintf("Failed to run ripgrep: %v", err))
	}

	// Abort kills rg; the read loop then sees EOF and reports the abort.
	// watchExited gives the happens-before edge for the aborted flag: the
	// watcher's write is complete before its return, and the return is
	// observed before anything reads the flag.
	aborted := false
	watchDone := make(chan struct{})
	watchExited := make(chan struct{})
	go func() {
		defer close(watchExited)
		select {
		case <-ctx.Done():
			aborted = true
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		case <-watchDone:
		}
	}()
	stopWatch := func() { close(watchDone) }

	var (
		matches           []grepMatch
		matchCount        int
		matchLimitReached bool
		linesTruncated    bool
	)

	// bufio.Reader rather than Scanner: a match line can be arbitrarily
	// long, and Scanner's default 64KB token limit would fail on it.
	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			if matchCount < effectiveLimit {
				var ev rgMatchEvent
				if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "match" {
					if ev.Data.Path.Text != "" && ev.Data.LineNumber > 0 {
						matchCount++
						matches = append(matches, grepMatch{
							filePath: ev.Data.Path.Text,
							lineNum:  ev.Data.LineNumber,
							lineText: ev.Data.Lines.Text,
						})
						if matchCount >= effectiveLimit {
							// Reaching the limit is enough to know there
							// is more; stop rg here (grep.ts:234-237).
							matchLimitReached = true
							if cmd.Process != nil {
								_ = cmd.Process.Kill()
							}
						}
					}
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	stopWatch()
	<-watchExited

	if aborted || ctx.Err() != nil {
		return errResult("Operation aborted")
	}
	// rg exits 1 for "no matches" and 2 for a real error; a kill from the
	// match limit is not an error (grep.ts:251-255).
	if waitErr != nil && !matchLimitReached {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) && ee.ExitCode() == 1 {
			// no matches — handled below
		} else {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = fmt.Sprintf("ripgrep exited with code %d", exitCodeOf(waitErr))
			}
			return errResult(msg)
		}
	}

	if matchCount == 0 {
		return textResult("No matches found")
	}

	// --- format (grep.ts:264-303) -----------------------------------------
	var outputLines []string
	for _, m := range matches {
		if contextValue == 0 && m.lineText != "" {
			sanitized := strings.TrimSuffix(strings.ReplaceAll(strings.ReplaceAll(m.lineText, "\r\n", "\n"), "\r", ""), "\n")
			text, wasTruncated := truncateLine(sanitized, grepMaxLineLength)
			if wasTruncated {
				linesTruncated = true
			}
			outputLines = append(outputLines, fmt.Sprintf("%s:%d: %s", grepFormatPath(m.filePath, isDirectory, searchPath), m.lineNum, text))
			continue
		}
		block, truncated := grepContextBlock(m, contextValue, isDirectory, searchPath)
		if truncated {
			linesTruncated = true
		}
		outputLines = append(outputLines, block...)
	}

	tr := truncateHead(strings.Join(outputLines, "\n"), TruncationOptions{MaxLines: noLineLimit})
	output := tr.Content
	var notices []string
	details := GrepDetails{}
	if matchLimitReached {
		notices = append(notices, fmt.Sprintf("%d matches limit reached. Use limit=%d for more, or refine pattern", effectiveLimit, effectiveLimit*2))
		details.MatchLimitReached = effectiveLimit
	}
	if tr.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", formatSize(defaultMaxBytes)))
		details.Truncation = &tr
	}
	if linesTruncated {
		notices = append(notices, fmt.Sprintf("Some lines truncated to %d chars. Use read tool to see full lines", grepMaxLineLength))
		details.LinesTruncated = true
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}

	res := textResult(output)
	res.Details = details
	return res
}

// grepFormatPath renders a match's path: relative to the search root when
// searching a directory and the result is inside it, the basename
// otherwise, always with forward slashes (grep.ts:136-145).
func grepFormatPath(filePath string, isDirectory bool, searchPath string) string {
	if isDirectory {
		if rel, err := filepath.Rel(searchPath, filePath); err == nil {
			if rel != "" && !strings.HasPrefix(rel, "..") {
				return toSlash(rel)
			}
		}
	}
	return filepath.Base(filePath)
}

// grepContextBlock renders one match with its context lines, reading the
// file for the surrounding lines (grep.ts:197-215).
func grepContextBlock(m grepMatch, contextValue int, isDirectory bool, searchPath string) (block []string, linesTruncated bool) {
	rel := grepFormatPath(m.filePath, isDirectory, searchPath)
	raw, err := os.ReadFile(m.filePath)
	if err != nil {
		return []string{fmt.Sprintf("%s:%d: (unable to read file)", rel, m.lineNum)}, false
	}
	lines := strings.Split(normalizeToLF(string(raw)), "\n")
	start := m.lineNum - contextValue
	if start < 1 {
		start = 1
	}
	end := m.lineNum + contextValue
	if end > len(lines) {
		end = len(lines)
	}
	for current := start; current <= end; current++ {
		text := strings.ReplaceAll(lines[current-1], "\r", "")
		truncatedText, wasTruncated := truncateLine(text, grepMaxLineLength)
		if wasTruncated {
			linesTruncated = true
		}
		if current == m.lineNum {
			block = append(block, fmt.Sprintf("%s:%d: %s", rel, current, truncatedText))
		} else {
			block = append(block, fmt.Sprintf("%s-%d- %s", rel, current, truncatedText))
		}
	}
	return block, linesTruncated
}
