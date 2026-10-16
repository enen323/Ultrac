// tools.go — the built-in tools' prompt contributions: one line for the tools
// section, guideline bullets for the rules section.
//
// Ported verbatim from pi's per-tool *ToolSystemPromptContribution constants
// (coding-agent/src/core/tools/{read,bash,edit,write,grep,find,ls,powershell}.ts)
// so the rendered rules read the same in both implementations. One deliberate
// deviation: pi's bash/powershell guideline tells the model it can inspect
// PI_* environment variables, which pi's bash tool exposes through
// exposeSessionEnvironment. This port does not expose a session environment
// (P3 did not port it), so the line is dropped rather than left pointing at
// variables that do not exist — a prompt that lies costs a tool call.
package prompt

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultPreamble is the identity text when no custom prompt replaces it.
// pi's says "operating inside pi"; this build says ultrac, because that is
// the harness actually running and the model should not be told otherwise.
// The section name and position are what the transcript format freezes, not
// this sentence.
const DefaultPreamble = "You are an expert coding assistant operating inside ultrac, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files."

type contribution struct {
	snippet    string
	guidelines []string
}

// toolContributions is the built-in table, keyed by tool name. A tool with no
// entry has no snippet and contributes no guideline.
var toolContributions = map[string]contribution{
	"read": {
		snippet:    "Read file contents",
		guidelines: []string{"Use read to examine files instead of cat or sed."},
	},
	"bash": {
		snippet:    "Execute bash commands (ls, grep, find, etc.)",
		guidelines: nil,
	},
	"powershell": {
		snippet:    "Execute PowerShell commands",
		guidelines: nil,
	},
	"edit": {
		snippet: "Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
		guidelines: []string{
			"Use edit for precise changes (edits[].oldText must match exactly)",
			"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
			"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
			"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
		},
	},
	"write": {
		snippet:    "Create or overwrite files",
		guidelines: []string{"Use write only for new files or complete rewrites."},
	},
	"grep": {
		snippet:    "Search file contents for patterns (respects .gitignore)",
		guidelines: nil,
	},
	"find": {
		snippet:    "Find files by glob pattern (respects .gitignore)",
		guidelines: nil,
	},
	"ls": {
		snippet:    "List directory contents",
		guidelines: nil,
	},
}

// Snippet returns the built-in one-line description for a tool, "" when the
// tool has none.
func Snippet(name string) string {
	return toolContributions[name].snippet
}

// Guidelines returns the built-in rule bullets for a tool, nil when it has
// none. The slice is shared; callers must not mutate it.
func Guidelines(name string) []string {
	return toolContributions[name].guidelines
}

// LoadContextFiles reads the project instruction files pi's resource loader
// feeds the prompt: CLAUDE.md and AGENTS.md at the root of workDir. Files
// that are absent or empty are skipped, so a repository with neither gets no
// project_context section.
//
// The old engine/prompt also read .ultrac/CLAUDE.md and .ultrac/rules/*.md.
// Those are Claude Code shaped conventions; under the pi layout the
// equivalent knob is a custom section or appendSystemPrompt, both of which
// Options already carries.
func LoadContextFiles(workDir string) []ContextFile {
	if workDir == "" {
		return nil
	}
	var out []ContextFile
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		data, err := os.ReadFile(filepath.Join(workDir, name))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == "" {
			continue
		}
		out = append(out, ContextFile{Path: name, Content: strings.TrimSpace(string(data))})
	}
	return out
}
