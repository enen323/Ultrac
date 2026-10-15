package tool

// The ls tool, ported from pi's
// packages/coding-agent/src/core/tools/ls.ts (176 lines).
//
// This tool has no pre-port counterpart — the old engine answered directory
// listings through bash — so it is a straight port.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// LsInput mirrors pi's lsSchema (ls.ts:11-15).
type LsInput struct {
	Path  string `json:"path,omitempty"`
	Limit *int   `json:"limit,omitempty"`
}

const lsSchema = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Directory to list (default: current directory)"
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of entries to return (default: 500)"
    }
  },
  "additionalProperties": false
}`

// lsDefaultLimit is pi's DEFAULT_LIMIT (ls.ts:23).
const lsDefaultLimit = 500

// LsDetails carries the truncation accounting for logs and UI.
type LsDetails struct {
	Truncation        *TruncationResult `json:"truncation,omitempty"`
	EntryLimitReached int               `json:"entryLimitReached,omitempty"`
}

// NewLsTool builds the ls tool for cwd.
func NewLsTool(cwd string) *agent.AgentTool[LsInput] {
	return &agent.AgentTool[LsInput]{
		Tool: ai.Tool{
			Name: "ls",
			// pi: ls.ts:62.
			Description: fmt.Sprintf(
				"List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to %d entries or %dKB (whichever is hit first).",
				lsDefaultLimit, defaultMaxBytes/1024),
			Parameters: json.RawMessage(lsSchema),
		},
		Label: "ls",
		Execute: func(ctx context.Context, _ string, in LsInput, _ func(agent.Result)) (agent.Result, error) {
			return executeLs(ctx, cwd, in), nil
		},
	}
}

func init() {
	Register("ls", func(cwd string) agent.Tool { return agent.Erase(NewLsTool(cwd)) })
}

func executeLs(ctx context.Context, cwd string, in LsInput) agent.Result {
	if err := ctx.Err(); err != nil {
		return errResult("Operation aborted")
	}
	dirPath := resolveToCwd(orDefault(in.Path, "."), cwd)
	effectiveLimit := lsDefaultLimit
	if in.Limit != nil && *in.Limit > 0 {
		effectiveLimit = *in.Limit
	}

	// pi reports the resolved absolute path in these two errors
	// (ls.ts:88, 95).
	if _, err := os.Stat(dirPath); err != nil {
		if os.IsNotExist(err) {
			return errResult("Path not found: " + dirPath)
		}
		return errResult(err.Error())
	}
	info, err := os.Stat(dirPath)
	if err != nil {
		return errResult(err.Error())
	}
	if !info.IsDir() {
		return errResult("Not a directory: " + dirPath)
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return errResult(fmt.Sprintf("Cannot read directory: %v", err))
	}
	if err := ctx.Err(); err != nil {
		return errResult("Operation aborted")
	}

	// Alphabetical, case-insensitive — pi's
	// a.toLowerCase().localeCompare(b.toLowerCase()) (ls.ts:109). Go's
	// approximation compares lowercased forms and breaks ties on the raw
	// name; ICU collation differences on non-ASCII names are out of scope.
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := strings.ToLower(entries[i].Name()), strings.ToLower(entries[j].Name())
		if a != b {
			return a < b
		}
		return entries[i].Name() < entries[j].Name()
	})

	var results []string
	entryLimitReached := false
	for _, entry := range entries {
		if len(results) >= effectiveLimit {
			entryLimitReached = true
			break
		}
		suffix := ""
		if entry.IsDir() {
			suffix = "/"
		} else if fi, err := entry.Info(); err == nil && fi.IsDir() {
			// A symlink to a directory still gets the marker, matching
			// pi's per-entry stat.
			suffix = "/"
		}
		results = append(results, entry.Name()+suffix)
	}

	if len(results) == 0 {
		return textResult("(empty directory)")
	}

	// Byte truncation only: the entry cap already bounds rows, so the line
	// limit is disabled (pi passes Number.MAX_SAFE_INTEGER, ls.ts:141).
	tr := truncateHead(strings.Join(results, "\n"), TruncationOptions{MaxLines: noLineLimit})
	output := tr.Content

	var notices []string
	details := LsDetails{}
	if entryLimitReached {
		notices = append(notices, fmt.Sprintf("%d entries limit reached. Use limit=%d for more", effectiveLimit, effectiveLimit*2))
		details.EntryLimitReached = effectiveLimit
	}
	if tr.Truncated {
		notices = append(notices, fmt.Sprintf("%s limit reached", formatSize(defaultMaxBytes)))
		details.Truncation = &tr
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}

	res := textResult(output)
	if details.Truncation != nil || details.EntryLimitReached != 0 {
		res.Details = details
	}
	return res
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
