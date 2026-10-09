package builtin

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ultrac/internal/contract/tool"
)

// GlobInput mirrors GlobTool's schema.
type GlobInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

const globSchema = `{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "The glob pattern to match files against"
    },
    "path": {
      "type": "string",
      "description": "The directory to search in. If not specified, the current working directory will be used. IMPORTANT: Omit this field to use the default directory. DO NOT enter \"undefined\" or \"null\" - simply omit it for the default behavior. Must be a valid directory path if provided."
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`

const globDescription = `- Fast file pattern matching tool that works with any codebase size
- Supports glob patterns like "**/*.js" or "src/**/*.ts"
- Returns matching file paths sorted by modification time
- Use this tool when you need to find files by name patterns
- When you are doing an open ended search that may require multiple rounds of globbing and grepping, use the Agent tool instead`

// globMaxResults is the cap the TypeScript tool reports as `truncated`.
const globMaxResults = 100

type globTool struct {
	spec[GlobInput]
}

func (globTool) Call(_ context.Context, in GlobInput, uc *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	pattern := strings.TrimSpace(in.Pattern)
	if pattern == "" {
		return errResult("glob: pattern is required"), nil
	}

	root := workDir(uc)
	if strings.TrimSpace(in.Path) != "" {
		root = resolvePath(uc, in.Path)
		info, err := os.Stat(root)
		if err != nil {
			return errResult("glob: directory does not exist: %s", displayPath(root)), nil
		}
		if !info.IsDir() {
			return errResult("glob: %s is not a directory", displayPath(root)), nil
		}
	}

	// Walk from the pattern's literal prefix rather than from root: for
	// `src/internal/**/*.go` that turns a repository-wide walk into a search
	// of two directories.
	start := root
	if lit := globRoot(pattern); lit != "" {
		candidate := filepath.Join(root, filepath.FromSlash(lit))
		if info, err := os.Stat(candidate); err == nil {
			start = candidate
			if info.IsDir() {
				// The literal prefix is part of the pattern; match from its
				// parent so the prefix is still compared.
				start = filepath.Dir(candidate)
			}
		}
	}

	re, err := compileGlob(pattern)
	if err != nil {
		return errResult("glob: invalid pattern %q: %v", pattern, err), nil
	}

	type hit struct {
		rel     string
		modTime int64
	}
	var hits []hit
	truncated := false

	werr := walkFiles(start, func(path string, d os.DirEntry) error {
		rel := relativeToRoot(root, path)
		if !re.MatchString(rel) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		hits = append(hits, hit{rel: rel, modTime: info.ModTime().UnixNano()})
		if len(hits) > globMaxResults {
			// Collect one extra so truncation is distinguishable from an
			// exactly-full result set.
			truncated = true
			hits = hits[:globMaxResults]
			return filepath.SkipAll
		}
		return nil
	})
	if werr != nil && werr != filepath.SkipAll {
		return errResult("glob: %v", werr), nil
	}

	// Most recently modified first, matching the tool's documented order.
	sort.Slice(hits, func(i, j int) bool { return hits[i].modTime > hits[j].modTime })

	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.rel)
	}

	if len(names) == 0 {
		return okResult("No files found", map[string]any{
			"numFiles": 0, "filenames": []string{}, "truncated": false,
		}), nil
	}

	out := strings.Join(names, "\n")
	if truncated {
		out += "\n(Results are truncated. Consider using a more specific path or pattern.)"
	}
	return okResult(out, map[string]any{
		"numFiles":  len(names),
		"filenames": names,
		"truncated": truncated,
		"root":      root,
	}), nil
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[GlobInput](globTool{spec: spec[GlobInput]{
		Name_:           "Glob",
		Hint:            "find files by name pattern or wildcard",
		Desc:            globDescription,
		Schema:          globSchema,
		ReadOnly:        true,
		ConcurrencySafe: true,
		SearchRead:      tool.SearchRead{Search: true, List: true},
	}}))
}
