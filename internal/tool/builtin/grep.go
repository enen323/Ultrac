package builtin

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"ultrac/internal/contract/tool"
)

// GrepInput mirrors GrepTool's schema, including the ripgrep-style flags.
//
// The field names are ripgrep's on purpose: the model already knows them, and
// a rename to `before`/`after` would make every prompt that mentions `-A` wrong.
type GrepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	OutputMode string `json:"output_mode,omitempty"`

	Before      *int  `json:"-B,omitempty"`
	After       *int  `json:"-A,omitempty"`
	ContextC    *int  `json:"-C,omitempty"`
	ContextFlat *int  `json:"context,omitempty"`
	LineNumbers *bool `json:"-n,omitempty"`
	IgnoreCase  *bool `json:"-i,omitempty"`

	Type      string `json:"type,omitempty"`
	HeadLimit *int   `json:"head_limit,omitempty"`
	Offset    *int   `json:"offset,omitempty"`
	Multiline *bool  `json:"multiline,omitempty"`
}

const grepSchema = `{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "The regular expression pattern to search for in file contents"
    },
    "path": {
      "type": "string",
      "description": "File or directory to search in (rg PATH). Defaults to current working directory."
    },
    "glob": {
      "type": "string",
      "description": "Glob pattern to filter files (e.g. \"*.js\", \"*.{ts,tsx}\") - maps to rg --glob"
    },
    "output_mode": {
      "type": "string",
      "enum": ["content", "files_with_matches", "count"],
      "description": "Output mode: \"content\" shows matching lines (supports -A/-B/-C context, -n line numbers, head_limit), \"files_with_matches\" shows file paths (supports head_limit), \"count\" shows match counts (supports head_limit). Defaults to \"files_with_matches\"."
    },
    "-B": { "type": "number", "description": "Number of lines to show before each match (rg -B). Requires output_mode: \"content\", ignored otherwise." },
    "-A": { "type": "number", "description": "Number of lines to show after each match (rg -A). Requires output_mode: \"content\", ignored otherwise." },
    "-C": { "type": "number", "description": "Alias for context." },
    "context": { "type": "number", "description": "Number of lines to show before and after each match (rg -C). Requires output_mode: \"content\", ignored otherwise." },
    "-n": { "type": "boolean", "description": "Show line numbers in output (rg -n). Requires output_mode: \"content\", ignored otherwise. Defaults to true." },
    "-i": { "type": "boolean", "description": "Case insensitive search (rg -i)" },
    "type": { "type": "string", "description": "File type to search (rg --type). Common types: js, py, rust, go, java, etc. More efficient than include for standard file types." },
    "head_limit": { "type": "number", "description": "Limit output to first N lines/entries, equivalent to \"| head -N\". Works across all output modes. Defaults to 250 when unspecified. Pass 0 for unlimited (use sparingly - large result sets waste context)." },
    "offset": { "type": "number", "description": "Skip first N lines/entries before applying head_limit, equivalent to \"| tail -n +N | head -N\". Works across all output modes. Defaults to 0." },
    "multiline": { "type": "boolean", "description": "Enable multiline mode where . matches newlines and patterns can span lines (rg -U --multiline-dotall). Default: false." }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`

const grepDescription = `A powerful search tool built on a native Go regexp engine

Usage:
- ALWAYS use Grep for search tasks. NEVER invoke ` + "`grep`" + ` or ` + "`rg`" + ` as a Bash command.
- Supports full regex syntax (e.g., "log.*Error", "function\\s+\\w+")
- Filter files with glob parameter (e.g., "*.js", "**/*.tsx") or type parameter (e.g., "js", "py", "rust")
- Output modes: "content" shows matching lines, "files_with_matches" shows only file paths (default), "count" shows match counts
- Use the Agent tool for open-ended searches requiring multiple rounds
- Pattern syntax: Uses Go regexp (RE2). Literal braces need escaping (use ` + "`interface\\{\\}`" + ` to find ` + "`interface{}`" + ` in Go code)
- Multiline matching: By default patterns match within single lines only. For cross-line patterns like ` + "`struct \\{[\\s\\S]*?field`" + `, use multiline: true
- Note: this engine walks the filesystem itself rather than delegating to ripgrep, so .gitignore files are not consulted; VCS and dependency directories are skipped by name`

const grepDefaultHeadLimit = 250

// maxGrepFileBytes caps how large a file may be before it is skipped.
// A generated bundle or a data dump is never what a code search wants, and
// reading one costs more than the whole rest of the walk.
const maxGrepFileBytes = 5 * 1024 * 1024

// grepTypeExtensions maps the `type` parameter onto extensions.
//
// ripgrep keeps a large table here; this covers the languages this engine is
// actually used on, and an unknown type is an error rather than a silent
// no-match — "0 results" and "I ignored your filter" must not look the same.
var grepTypeExtensions = map[string][]string{
	"go":     {".go"},
	"js":     {".js", ".mjs", ".cjs", ".jsx"},
	"ts":     {".ts", ".tsx", ".mts", ".cts"},
	"tsx":    {".tsx"},
	"jsx":    {".jsx"},
	"py":     {".py", ".pyi"},
	"rust":   {".rs"},
	"java":   {".java"},
	"c":      {".c", ".h"},
	"cpp":    {".cc", ".cpp", ".cxx", ".hpp", ".hxx", ".h"},
	"cs":     {".cs"},
	"rb":     {".rb"},
	"php":    {".php"},
	"sh":     {".sh", ".bash", ".zsh"},
	"sql":    {".sql"},
	"json":   {".json"},
	"yaml":   {".yaml", ".yml"},
	"toml":   {".toml"},
	"md":     {".md", ".markdown"},
	"html":   {".html", ".htm"},
	"css":    {".css", ".scss", ".sass", ".less"},
	"xml":    {".xml"},
	"proto":  {".proto"},
	"swift":  {".swift"},
	"kotlin": {".kt", ".kts"},
	"lua":    {".lua"},
	"vim":    {".vim"},
}

type grepTool struct {
	spec[GrepInput]
}

func (grepTool) Call(_ context.Context, in GrepInput, uc *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	pattern := in.Pattern
	if pattern == "" {
		return errResult("grep: pattern is required"), nil
	}

	flags := ""
	if in.IgnoreCase != nil && *in.IgnoreCase {
		flags += "i"
	}
	if in.Multiline != nil && *in.Multiline {
		flags += "s"
	}
	source := pattern
	if flags != "" {
		source = "(?" + flags + ")" + pattern
	}
	re, err := regexp.Compile(source)
	if err != nil {
		return errResult("grep: invalid pattern %q: %v", pattern, err), nil
	}

	mode := in.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	switch mode {
	case "content", "files_with_matches", "count":
	default:
		return errResult("grep: output_mode must be content, files_with_matches or count; got %q", mode), nil
	}

	var extFilter []string
	if in.Type != "" {
		exts, ok := grepTypeExtensions[strings.ToLower(in.Type)]
		if !ok {
			return errResult("grep: unknown type %q", in.Type), nil
		}
		extFilter = exts
	}
	var fileRe *regexp.Regexp
	if in.Glob != "" {
		fileRe, err = compileGlob(in.Glob)
		if err != nil {
			return errResult("grep: invalid glob %q: %v", in.Glob, err), nil
		}
	}

	target := workDir(uc)
	if in.Path != "" {
		target = resolvePath(uc, in.Path)
	}
	info, serr := os.Stat(target)
	if serr != nil {
		return errResult("grep: %s does not exist", displayPath(target)), nil
	}

	root := target
	single := !info.IsDir()
	if single {
		root = filepath.Dir(target)
	}

	headLimit := grepDefaultHeadLimit
	unlimited := false
	if in.HeadLimit != nil {
		if *in.HeadLimit == 0 {
			unlimited = true
		} else if *in.HeadLimit > 0 {
			headLimit = *in.HeadLimit
		}
	}
	skip := 0
	if in.Offset != nil && *in.Offset > 0 {
		skip = *in.Offset
	}
	// collect is how many raw results to gather: everything the caller asked
	// for, plus one. The extra one is what makes "the results were truncated"
	// distinguishable from "this is exactly all of them", and without it a
	// paged result set reports full when it is not.
	collect := 0
	if !unlimited {
		collect = headLimit + skip + 1
	}

	before, after := contextLines(in)
	showNums := in.LineNumbers == nil || *in.LineNumbers

	switch mode {
	case "content":
		return grepContent(re, target, root, single, fileRe, extFilter, before, after, showNums, collect, headLimit, skip, unlimited)
	case "count":
		return grepCount(re, target, root, single, fileRe, extFilter, collect, headLimit, skip, unlimited)
	default:
		return grepFiles(re, target, root, single, fileRe, extFilter, collect, headLimit, skip, unlimited)
	}
}

func contextLines(in GrepInput) (before, after int) {
	if in.ContextFlat != nil {
		before, after = *in.ContextFlat, *in.ContextFlat
	}
	if in.ContextC != nil {
		before, after = *in.ContextC, *in.ContextC
	}
	if in.Before != nil {
		before = *in.Before
	}
	if in.After != nil {
		after = *in.After
	}
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	return before, after
}

// grepCandidate decides whether a file is worth reading.
func grepCandidate(path, root string, single bool, fileRe *regexp.Regexp, exts []string) bool {
	if single {
		return true
	}
	if len(exts) > 0 {
		ext := strings.ToLower(filepath.Ext(path))
		match := false
		for _, e := range exts {
			if ext == e {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if fileRe != nil {
		if !fileRe.MatchString(relativeToRoot(root, path)) {
			return false
		}
	}
	return true
}

func grepFiles(re *regexp.Regexp, target, root string, single bool, fileRe *regexp.Regexp, exts []string, collect, page, skip int, unlimited bool) (*tool.Result, error) {
	var files []string
	walkErr := walkFiles(root, func(path string, d os.DirEntry) error {
		if single && path != target {
			return nil
		}
		if !grepCandidate(path, root, single, fileRe, exts) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxGrepFileBytes || info.Size() == 0 {
			return nil
		}
		if isProbablyBinary(path) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if re.Match(raw) {
			files = append(files, relativeToRoot(root, path))
			if !unlimited && len(files) >= collect {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return errResult("grep: %v", walkErr), nil
	}

	page2, wasTruncated := paginate(files, skip, page, unlimited)

	if len(page2) == 0 {
		return okResult("No files found", map[string]any{
			"mode": "files_with_matches", "numFiles": 0, "filenames": []string{},
		}), nil
	}
	out := strings.Join(page2, "\n")
	if wasTruncated {
		out += "\n(Results are truncated. Consider a more specific pattern.)"
	}
	return okResult(out, map[string]any{
		"mode": "files_with_matches", "numFiles": len(page2), "filenames": page2,
		"truncated": wasTruncated,
	}), nil
}

func grepCount(re *regexp.Regexp, target, root string, single bool, fileRe *regexp.Regexp, exts []string, collect, page, skip int, unlimited bool) (*tool.Result, error) {
	type fileCount struct {
		rel string
		n   int
	}
	var counts []fileCount
	walkErr := walkFiles(root, func(path string, d os.DirEntry) error {
		if single && path != target {
			return nil
		}
		if !grepCandidate(path, root, single, fileRe, exts) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxGrepFileBytes || info.Size() == 0 {
			return nil
		}
		if isProbablyBinary(path) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		n := len(re.FindAll(raw, -1))
		if n > 0 {
			counts = append(counts, fileCount{rel: relativeToRoot(root, path), n: n})
			if !unlimited && len(counts) >= collect {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return errResult("grep: %v", walkErr), nil
	}

	sort.Slice(counts, func(i, j int) bool { return counts[i].n > counts[j].n })
	rels := make([]string, 0, len(counts))
	total := 0
	for _, c := range counts {
		rels = append(rels, c.rel)
		total += c.n
	}
	page2, wasTruncated := paginate(rels, skip, page, unlimited)

	lines := make([]string, 0, len(page2))
	sum := 0
	for _, rel := range page2 {
		for _, c := range counts {
			if c.rel == rel {
				lines = append(lines, rel+":"+itoa(c.n))
				sum += c.n
				break
			}
		}
	}
	if len(lines) == 0 {
		return okResult("No matches found", map[string]any{
			"mode": "count", "numFiles": 0, "numMatches": 0,
		}), nil
	}
	out := strings.Join(lines, "\n")
	out += "\n\nFound " + itoa(sum) + " occurrences across " + itoa(len(page2)) + " files."
	if wasTruncated || total > sum {
		out += " (truncated; " + itoa(total) + " files have matches in total)"
	}
	return okResult(out, map[string]any{
		"mode": "count", "numFiles": len(page2), "numMatches": sum, "totalMatches": total,
		"truncated": wasTruncated,
	}), nil
}

func grepContent(re *regexp.Regexp, target, root string, single bool, fileRe *regexp.Regexp, exts []string, before, after int, showNums bool, collect, headLimit, skip int, unlimited bool) (*tool.Result, error) {
	var lines []string

	walkErr := walkFiles(root, func(path string, d os.DirEntry) error {
		if single && path != target {
			return nil
		}
		if !grepCandidate(path, root, single, fileRe, exts) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxGrepFileBytes || info.Size() == 0 {
			return nil
		}
		if isProbablyBinary(path) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(raw)
		fileLines := splitLines(text)
		matched := make([]bool, len(fileLines))
		any := false
		for i, line := range fileLines {
			if re.MatchString(line) {
				matched[i] = true
				any = true
			}
		}
		if !any {
			return nil
		}

		rel := relativeToRoot(root, path)
		lastEmitted := -1
		for i := range fileLines {
			if !matched[i] {
				continue
			}
			start := i - before
			if start < 0 {
				start = 0
			}
			if start <= lastEmitted {
				start = lastEmitted + 1
			}
			end := i + after
			if end >= len(fileLines) {
				end = len(fileLines) - 1
			}
			for j := start; j <= end; j++ {
				sep := "-"
				if matched[j] {
					sep = ":"
				}
				prefix := rel + sep
				if showNums {
					prefix = rel + sep + itoa(j+1) + sep
				}
				lines = append(lines, prefix+fileLines[j])
				lastEmitted = j
			}
			if !unlimited && len(lines) >= collect {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return errResult("grep: %v", walkErr), nil
	}

	page2, wasTruncated := paginate(lines, skip, headLimit, unlimited)
	if len(page2) == 0 {
		return okResult("No matches found", map[string]any{
			"mode": "content", "numLines": 0,
		}), nil
	}
	out := strings.Join(page2, "\n")
	if wasTruncated {
		out += "\n\n[Showing results with pagination = limit:" + itoa(headLimit) + ", offset:" + itoa(skip) + "]"
	}
	return okResult(out, map[string]any{
		"mode": "content", "numLines": len(page2), "truncated": wasTruncated,
	}), nil
}

// paginate applies offset then limit, reporting whether anything was dropped.
//
// wasTruncated is true only when a result was actually withheld, which is why
// the caller collects one extra: an exactly-full page and a truncated page
// otherwise look identical, and the model cannot tell whether to narrow its
// search.
func paginate(items []string, skip, limit int, unlimited bool) ([]string, bool) {
	if skip >= len(items) {
		return nil, false
	}
	rest := items[skip:]
	if unlimited || len(rest) <= limit {
		return rest, false
	}
	return rest[:limit], true
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[GrepInput](grepTool{spec: spec[GrepInput]{
		Name_:           "Grep",
		Hint:            "search file contents with regex",
		Desc:            grepDescription,
		Schema:          grepSchema,
		ReadOnly:        true,
		ConcurrencySafe: true,
		SearchRead:      tool.SearchRead{Search: true},
	}}))
}
