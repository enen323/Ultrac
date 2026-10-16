// Package prompt assembles the system prompt as ordered, independently
// replaceable sections — pi's core/system-prompt.ts, landed by P4
// (docs/pi-core-harness-plan.md §3.5).
//
// # Why sections instead of one string
//
// pi does not rebuild the prompt from scratch each turn: the transcript's
// leading system message carries named sections, and a later system message
// patches only the sections that changed (diffSystemPromptSections). A
// tool-set change or a cwd change then costs one small message instead of a
// re-statement of the whole prompt, and the model can relate an update to the
// section it belongs to. That mechanism is the point of this package; the
// single-text Build path exists for callers that want the rendered prompt
// (the P5 print mode, diagnostics) and renders exactly what the transcript
// replays.
//
// # Section vocabulary (frozen names, pi's)
//
//	preamble        untagged identity text (always first)
//	tools           the tool list, one "- name: snippet" line each
//	rules           behavioural bullets
//	docs            where the harness's own documentation lives
//	addendum        user append text (--append-system-prompt)
//	project_context <project_instructions> blocks from context files
//	skills          (reserved; the skills system is a non-goal, plan §2.2)
//	cwd             the working directory, forward-slashed
//
// Every section except preamble is wrapped in a tag of the same name
// (<tools>…</tools>) so the model can match a later update to it.
//
// # What is deliberately not ported
//
// The skills loader (pi's formatSkillsForPrompt) — skills are a non-goal. The
// extension hooks that feed customSections/appendSystemPrompt exist as plain
// Options fields, so the shape is ready when the extension model lands.
package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Section names — pi's SystemPromptSections keys (system-prompt.ts:51-58).
// These strings are the patch vocabulary: a system message that sets
// sections["tools"] replaces the whole tools section, and a nil value
// deletes it. Renaming one breaks replay of every existing transcript.
const (
	SectionPreamble       = "preamble"
	SectionTools          = "tools"
	SectionRules          = "rules"
	SectionDocs           = "docs"
	SectionAddendum       = "addendum"
	SectionProjectContext = "project_context"
	SectionSkills         = "skills"
	SectionCWD            = "cwd"
)

// sectionNamePattern is pi's SYSTEM_PROMPT_SECTION_NAME
// (system-prompt.ts:58): lowercase start, then lowercase/digits/-/_.
var sectionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// DefaultSelectedTools is pi's default tool selection
// (system-prompt.ts:64): the four coding tools.
var DefaultSelectedTools = []string{"read", "bash", "edit", "write"}

// ContextFile is one pre-loaded project instruction file.
type ContextFile struct {
	Path    string
	Content string
}

// Options are the inputs to prompt assembly — pi's BuildSystemPromptOptions
// (system-prompt.ts:9-37), with the skills field dropped (non-goal).
type Options struct {
	// CustomPrompt replaces the default preamble text.
	CustomPrompt string
	// ForceSystemPrompt replaces the entire prompt. The result is opaque:
	// it lives in the system message's content with no sections, so later
	// diffs have nothing to patch.
	ForceSystemPrompt string
	// SelectedTools are the tools the prompt describes. Empty means
	// DefaultSelectedTools.
	SelectedTools []string
	// HiddenTools are selected but undeclared tools (reachable only through
	// another tool). They are left out of the tool list and the rules.
	HiddenTools []string
	// ToolSnippets overrides the built-in one-line tool descriptions, keyed
	// by tool name. Tools without a snippet are left out of the list.
	ToolSnippets map[string]string
	// ToolGuidelines overrides the built-in per-tool rule bullets.
	ToolGuidelines map[string][]string
	// PromptGuidelines are extra rule bullets appended after the tool ones.
	PromptGuidelines []string
	// AppendSystemPrompt is user text that becomes the addendum section.
	AppendSystemPrompt string
	// Sections are custom XML-wrapped sections keyed by tag name. Names must
	// match sectionNamePattern and must not be "preamble".
	Sections map[string]string
	// CWD is the working directory, reported in the cwd section.
	CWD string
	// ContextFiles are the project instruction files behind project_context.
	ContextFiles []ContextFile
}

// Section is one named piece of the prompt, in build order.
type Section struct {
	Name string
	Text string
}

// ValidateSectionName reports whether name may be used as a custom section
// name (pi: system-prompt.ts:144-148).
func ValidateSectionName(name string) error {
	if name == SectionPreamble || !sectionNamePattern.MatchString(name) {
		return fmt.Errorf("prompt: invalid system prompt section name: %q", name)
	}
	return nil
}

// BuildSystemPromptSections assembles the ordered sections for opts.
//
// The order is pi's insertion order (system-prompt.ts:151-191): preamble,
// tools, rules, docs, then addendum, project_context, cwd, then custom
// sections. Order matters twice over: it is the rendered prompt's order, and
// it is the first-declaration order the transcript replay tracks.
func BuildSystemPromptSections(opts Options) ([]Section, error) {
	for name := range opts.Sections {
		if err := ValidateSectionName(name); err != nil {
			return nil, err
		}
	}

	selected := opts.SelectedTools
	if len(selected) == 0 {
		selected = DefaultSelectedTools
	}
	hidden := make(map[string]bool, len(opts.HiddenTools))
	for _, name := range opts.HiddenTools {
		hidden[name] = true
	}
	declared := make([]string, 0, len(selected))
	for _, name := range selected {
		if !hidden[name] {
			declared = append(declared, name)
		}
	}

	var sections []Section
	if custom := strings.TrimSpace(opts.CustomPrompt); custom != "" {
		sections = append(sections, Section{SectionPreamble, custom})
	} else {
		sections = append(sections,
			Section{SectionPreamble, DefaultPreamble},
			Section{SectionTools, toolsSection(declared, opts.ToolSnippets)},
			Section{SectionRules, rulesSection(declared, opts.ToolGuidelines, opts.PromptGuidelines)},
			Section{SectionDocs, docsSection()},
		)
	}
	if addendum := strings.TrimSpace(opts.AppendSystemPrompt); addendum != "" {
		sections = append(sections, Section{SectionAddendum, addendum})
	}
	if ctx := projectContextSection(opts.ContextFiles); ctx != "" {
		sections = append(sections, Section{SectionProjectContext, ctx})
	}
	if cwd := strings.TrimSpace(opts.CWD); cwd != "" {
		sections = append(sections, Section{SectionCWD, filepath.ToSlash(cwd)})
	}
	for _, name := range sortedKeys(opts.Sections) {
		if text := opts.Sections[name]; text != "" {
			sections = append(sections, Section{name, text})
		}
	}

	// Wrap every section except the preamble in its own tag (pi:
	// system-prompt.ts:188-191). preamble is untagged text.
	for i := range sections {
		if sections[i].Name == SectionPreamble {
			continue
		}
		sections[i].Text = fmt.Sprintf("<%s>\n%s\n</%s>", sections[i].Name, sections[i].Text, sections[i].Name)
	}
	return sections, nil
}

// BuildSystemPromptState returns the complete prompt state for opts: either an
// opaque forced prompt with no sections, or empty content plus the structured
// sections (pi's buildSystemPromptState, system-prompt.ts:199-205).
func BuildSystemPromptState(opts Options) (content string, sections []Section, err error) {
	if opts.ForceSystemPrompt != "" {
		return opts.ForceSystemPrompt, nil, nil
	}
	sections, err = BuildSystemPromptSections(opts)
	return "", sections, err
}

// BuildSystemPrompt renders the prompt text exactly as the transcript's
// system message replays it (pi's buildSystemPrompt, system-prompt.ts:208-210):
// content, then each section, joined by blank lines.
func BuildSystemPrompt(opts Options) (string, error) {
	content, sections, err := BuildSystemPromptState(opts)
	if err != nil {
		return "", err
	}
	return RenderSystemPrompt(content, sections), nil
}

// RenderSystemPrompt joins content and sections the way pi's
// getSystemMessageText does (packages/ai/src/utils/text.ts:14-21): every
// non-empty part, blank-line separated, sections in order.
func RenderSystemPrompt(content string, sections []Section) string {
	parts := make([]string, 0, len(sections)+1)
	if content != "" {
		parts = append(parts, content)
	}
	for _, s := range sections {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// DiffSystemPromptSections computes the patch that turns previous into
// current: changed and new sections carry their text, sections missing from
// current carry nil (delete). It returns nil when nothing changed, which is
// the signal that no system message needs to be appended at all.
//
// pi: system-prompt.ts:217-229. previous comes from replaying the
// transcript's system messages (ai.GetCurrentSystemMessage), so it is never
// null per-section; a section the replay dropped is simply absent from the
// map and re-appears here as an addition.
func DiffSystemPromptSections(previous map[string]*string, current []Section) map[string]*string {
	inCurrent := make(map[string]bool, len(current))
	patch := make(map[string]*string)
	for _, s := range current {
		inCurrent[s.Name] = true
		prev, ok := previous[s.Name]
		if ok && prev != nil && *prev == s.Text {
			continue
		}
		text := s.Text
		patch[s.Name] = &text
	}
	for name := range previous {
		if !inCurrent[name] {
			patch[name] = nil
		}
	}
	if len(patch) == 0 {
		return nil
	}
	return patch
}

// SectionsMap converts an ordered section list into the map an ai.Message
// carries. preamble is a section like any other — the system message's
// content stays empty, exactly as pi's buildSystemPromptState leaves it.
func SectionsMap(sections []Section) map[string]*string {
	if len(sections) == 0 {
		return nil
	}
	out := make(map[string]*string, len(sections))
	for _, s := range sections {
		text := s.Text
		out[s.Name] = &text
	}
	return out
}

// toolsSection renders the tool list: one "- name: snippet" line per declared
// tool that has a snippet, "(none)" when there are none, then the standing
// note that custom tools may exist (pi: system-prompt.ts:157-160).
func toolsSection(declared []string, snippets map[string]string) string {
	var lines []string
	for _, name := range declared {
		snippet := snippets[name]
		if snippet == "" {
			snippet = Snippet(name)
		}
		if snippet == "" {
			continue
		}
		lines = append(lines, "- "+name+": "+snippet)
	}
	list := "(none)"
	if len(lines) > 0 {
		list = strings.Join(lines, "\n")
	}
	return list + "\n\nIn addition to the tools above, you may have access to other custom tools depending on the project."
}

// rulesSection builds the bullet list (pi's buildRules, system-prompt.ts:
// 88-125): the shell-fallback rule when no dedicated search/list tool is
// selected, then each declared tool's guidelines in selection order, then the
// caller's guidelines, then the two standing rules. Duplicates are dropped,
// first wins.
func rulesSection(declared []string, toolGuidelines map[string][]string, promptGuidelines []string) string {
	has := func(name string) bool {
		for _, t := range declared {
			if t == name {
				return true
			}
		}
		return false
	}

	var rules []string
	seen := make(map[string]bool)
	addRule := func(rule string) {
		normalized := strings.TrimSpace(rule)
		if normalized == "" || seen[normalized] {
			return
		}
		seen[normalized] = true
		rules = append(rules, normalized)
	}

	if (has("bash") || has("powershell")) && !has("grep") && !has("find") && !has("ls") {
		switch {
		case has("bash") && has("powershell"):
			addRule("Use bash or PowerShell for file operations like listing, searching, and finding files")
		case has("powershell"):
			addRule("Use PowerShell for file operations like listing, searching, and finding files")
		default:
			addRule("Use bash for file operations like ls, rg, find")
		}
	}
	for _, name := range declared {
		guidelines := toolGuidelines[name]
		if guidelines == nil {
			guidelines = Guidelines(name)
		}
		for _, rule := range guidelines {
			addRule(rule)
		}
	}
	for _, rule := range promptGuidelines {
		addRule(rule)
	}
	addRule("Be concise in your responses")
	addRule("Show file paths clearly when working with files")

	lines := make([]string, 0, len(rules))
	for _, rule := range rules {
		lines = append(lines, "- "+rule)
	}
	return strings.Join(lines, "\n")
}

// projectContextSection renders the context files behind
// <project_instructions> blocks (pi's renderProjectContext,
// system-prompt.ts:79-86).
func projectContextSection(files []ContextFile) string {
	if len(files) == 0 {
		return ""
	}
	parts := []string{"Project-specific instructions and guidelines:"}
	for _, f := range files {
		if strings.TrimSpace(f.Content) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("<project_instructions path=%q>\n%s\n</project_instructions>", f.Path, f.Content))
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// docsSection points at the harness's own documentation.
//
// pi's version lists the whole docs/ tree it ships (system-prompt.ts:162-169).
// This build does not ship that tree, so the section names the three asset
// roots resolved next to the executable and keeps the resolution rule: the
// paths are absolute, so a docs/... reference is read from there, not from
// the working directory. When the Ultrac docs land next to the binary the
// same section covers them without a prompt change.
func docsSection() string {
	dir := executableDir()
	return strings.Join([]string{
		"Ultrac documentation (read only when the user asks about ultrac itself, its harness, or the Go rewrite):",
		"- Main documentation: " + filepath.Join(dir, "README.md"),
		"- Design docs: " + filepath.Join(dir, "docs"),
		"- These paths are absolute: resolve them as given, not against the current working directory",
		"- Read referenced .md files completely and follow their cross-references before implementing",
	}, "\n")
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Insertion order is not recoverable from a Go map; sorted is the only
	// deterministic choice, and it keeps two builds of the same options
	// byte-identical.
	sortStrings(keys)
	return keys
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
