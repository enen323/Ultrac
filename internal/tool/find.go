package tool

// The find tool, ported from pi's
// packages/coding-agent/src/core/tools/find.ts (319 lines).
//
// Like grep, this shells out to an external binary (fd) and, unlike pi,
// does not download one when it is missing (plan §5.1, R12).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// fdInstallHint is the message when fd is missing.
const fdInstallHint = "fd is not installed. Install it (https://github.com/sharkdp/fd#installation) and make sure fd is on PATH."

// FindInput mirrors pi's findSchema (find.ts:26-32).
type FindInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Limit   *int   `json:"limit,omitempty"`
}

const findSchema = `{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'"
    },
    "path": {
      "type": "string",
      "description": "Directory to search in (default: current directory)"
    },
    "limit": {
      "type": "number",
      "description": "Maximum number of results (default: 1000)"
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`

// findDefaultLimit is pi's DEFAULT_LIMIT (find.ts:41).
const findDefaultLimit = 1000

// FindDetails is the tool's Details payload (find.ts:45-48).
type FindDetails struct {
	Truncation         *TruncationResult `json:"truncation,omitempty"`
	ResultLimitReached int               `json:"resultLimitReached,omitempty"`
}

// NewFindTool builds the find tool for cwd.
func NewFindTool(cwd string) *agent.AgentTool[FindInput] {
	return &agent.AgentTool[FindInput]{
		Tool: ai.Tool{
			Name: "find",
			// pi: find.ts:78.
			Description: fmt.Sprintf(
				"Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to %d results or %dKB (whichever is hit first).",
				findDefaultLimit, defaultMaxBytes/1024),
			Parameters: json.RawMessage(findSchema),
		},
		Label: "find",
		Execute: func(ctx context.Context, _ string, in FindInput, _ func(agent.Result)) (agent.Result, error) {
			return executeFind(ctx, cwd, in), nil
		},
	}
}

func init() {
	Register("find", func(cwd string) agent.Tool { return agent.Erase(NewFindTool(cwd)) })
}

func executeFind(ctx context.Context, cwd string, in FindInput) agent.Result {
	pattern := strings.TrimSpace(in.Pattern)
	if pattern == "" {
		return errResult("find: pattern is required")
	}
	fdPath, err := lookPath("fd")
	if err != nil {
		return errResult(fdInstallHint)
	}
	searchPath := resolveToCwd(orDefault(in.Path, "."), cwd)
	if _, err := os.Stat(searchPath); err != nil {
		return errResult("Path not found: " + searchPath)
	}

	effectiveLimit := findDefaultLimit
	if in.Limit != nil && *in.Limit > 0 {
		effectiveLimit = *in.Limit
	}

	// --- spawn fd (find.ts:182-215) ---------------------------------------
	args := []string{"--glob", "--color=never", "--hidden"}

	// fd ignores .gitignore outside git repos, so --no-require-git is only
	// needed there. Inside a repo, fd's default git-aware behavior keeps
	// parent .gitignore rules from leaking past nested repo boundaries
	// (find.ts:186-198).
	if !insideGitRepo(searchPath) {
		args = append(args, "--no-require-git")
	}
	args = append(args, "--max-results", fmt.Sprintf("%d", effectiveLimit))

	// fd --glob matches against the basename unless --full-path is set; in
	// --full-path mode it matches against the absolute candidate path, so a
	// path-containing pattern needs a leading "**/" to match anything
	// (find.ts:201-213).
	effectivePattern := pattern
	if strings.Contains(pattern, "/") {
		args = append(args, "--full-path")
		if !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "**/") && pattern != "**" {
			effectivePattern = "**/" + pattern
		}
		// fd matches full paths using native separators on Windows.
		if windowsOS {
			effectivePattern = strings.ReplaceAll(effectivePattern, "/", `[/\]`)
		}
	}
	args = append(args, "--", effectivePattern, searchPath)

	cmd := exec.Command(fdPath, args...)
	cmd.Dir = cwd
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errResult(fmt.Sprintf("Failed to run fd: %v", err))
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return errResult(fmt.Sprintf("Failed to run fd: %v", err))
	}

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

	var lines []string
	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			lines = append(lines, line)
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
	if waitErr != nil {
		// fd exits non-zero for real errors; a non-zero exit WITH output
		// still yields usable results (find.ts:251-257).
		if len(lines) == 0 {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = fmt.Sprintf("fd exited with code %d", exitCodeOf(waitErr))
			}
			return errResult(msg)
		}
	}

	// --- format (find.ts:268-299) ------------------------------------------
	var relativized []string
	for _, rawLine := range lines {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\n"))
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		relativized = append(relativized, relativizeFindResultPath(line, searchPath))
	}
	if len(relativized) == 0 {
		return textResult("No files found matching pattern")
	}

	resultLimitReached := len(relativized) >= effectiveLimit
	tr := truncateHead(strings.Join(relativized, "\n"), TruncationOptions{MaxLines: noLineLimit})
	output := tr.Content
	var notices []string
	details := FindDetails{}
	if resultLimitReached {
		notices = append(notices, fmt.Sprintf("%d results limit reached. Use limit=%d for more, or refine pattern", effectiveLimit, effectiveLimit*2))
		details.ResultLimitReached = effectiveLimit
	}
	if tr.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", formatSize(defaultMaxBytes)))
		details.Truncation = &tr
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}

	res := textResult(output)
	res.Details = details
	return res
}

// insideGitRepo reports whether searchPath or any ancestor contains a .git
// entry (find.ts:189-197).
func insideGitRepo(searchPath string) bool {
	current := searchPath
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

// relativizeFindResultPath makes a find result relative to the search root
// and posix-separated, preserving a trailing separator (find.ts:14-24).
func relativizeFindResultPath(resultPath, searchPath string) string {
	hadTrailing := strings.HasSuffix(resultPath, string(filepath.Separator)) ||
		(windowsOS && strings.HasSuffix(resultPath, "/"))
	rel := resultPath
	if filepath.IsAbs(resultPath) {
		if r, err := filepath.Rel(searchPath, resultPath); err == nil {
			rel = r
		}
	}
	posix := filepath.ToSlash(rel)
	if hadTrailing && !strings.HasSuffix(posix, "/") {
		posix += "/"
	}
	return posix
}
