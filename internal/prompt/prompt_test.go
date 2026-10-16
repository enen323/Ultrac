package prompt

import (
	"os"
	"strings"
	"testing"
)

func sectionByName(sections []Section, name string) (Section, bool) {
	for _, s := range sections {
		if s.Name == name {
			return s, true
		}
	}
	return Section{}, false
}

func TestDefaultSectionOrderAndTagging(t *testing.T) {
	sections, err := BuildSystemPromptSections(Options{CWD: `D:\repo`})
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{SectionPreamble, SectionTools, SectionRules, SectionDocs, SectionCWD}
	if len(sections) != len(wantOrder) {
		t.Fatalf("got %d sections (%v), want %v", len(sections), sectionNames(sections), wantOrder)
	}
	for i, name := range wantOrder {
		if sections[i].Name != name {
			t.Fatalf("section %d = %q, want %q (order: %v)", i, sections[i].Name, name, sectionNames(sections))
		}
	}
	// preamble is untagged text; everything else is wrapped in its own tag.
	if strings.HasPrefix(sections[0].Text, "<preamble>") {
		t.Error("preamble must not be wrapped in a tag")
	}
	for _, s := range sections[1:] {
		if !strings.HasPrefix(s.Text, "<"+s.Name+">\n") || !strings.HasSuffix(s.Text, "\n</"+s.Name+">") {
			t.Errorf("section %q is not wrapped in its tag: %q", s.Name, s.Text)
		}
	}
	// cwd is forward-slashed (pi: system-prompt.ts:183).
	cwd, _ := sectionByName(sections, SectionCWD)
	if cwd.Text != "<cwd>\nD:/repo\n</cwd>" {
		t.Errorf("cwd section = %q", cwd.Text)
	}
}

func sectionNames(sections []Section) []string {
	out := make([]string, len(sections))
	for i, s := range sections {
		out[i] = s.Name
	}
	return out
}

func TestToolsSectionListsSnippetToolsOnly(t *testing.T) {
	// Default selection is read/bash/edit/write, all of which have snippets.
	sections, err := BuildSystemPromptSections(Options{})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := sectionByName(sections, SectionTools)
	for _, name := range DefaultSelectedTools {
		if !strings.Contains(tools.Text, "- "+name+": ") {
			t.Errorf("tools section is missing %q:\n%s", name, tools.Text)
		}
	}
	if !strings.Contains(tools.Text, "\nIn addition to the tools above, you may have access to other custom tools depending on the project.\n</tools>") {
		t.Errorf("tools section lost the custom-tools note:\n%s", tools.Text)
	}

	// A selected tool without a snippet (and no override) is left out, and a
	// selection where nothing has a snippet renders "(none)".
	sections, err = BuildSystemPromptSections(Options{SelectedTools: []string{"read", "ls"}})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ = sectionByName(sections, SectionTools)
	if !strings.Contains(tools.Text, "- read: ") || !strings.Contains(tools.Text, "- ls: ") {
		t.Errorf("tools section = %q", tools.Text)
	}
	sections, err = BuildSystemPromptSections(Options{SelectedTools: []string{"nosuchtool"}})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ = sectionByName(sections, SectionTools)
	if !strings.HasPrefix(tools.Text, "<tools>\n(none)") {
		t.Errorf("expected (none), got %q", tools.Text)
	}
}

func TestRulesSectionShellFallbackAndDedup(t *testing.T) {
	// bash without grep/find/ls gets the shell-fallback rule first.
	sections, err := BuildSystemPromptSections(Options{SelectedTools: []string{"bash", "read"}})
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := sectionByName(sections, SectionRules)
	if !strings.Contains(rules.Text, "- Use bash for file operations like ls, rg, find\n") {
		t.Errorf("missing shell fallback rule:\n%s", rules.Text)
	}
	if !strings.Contains(rules.Text, "- Use read to examine files instead of cat or sed.") {
		t.Errorf("missing read guideline:\n%s", rules.Text)
	}
	if !strings.Contains(rules.Text, "- Be concise in your responses\n- Show file paths clearly when working with files\n</rules>") {
		t.Errorf("standing rules must close the list:\n%s", rules.Text)
	}

	// With a dedicated search tool present the fallback disappears.
	sections, err = BuildSystemPromptSections(Options{SelectedTools: []string{"bash", "grep"}})
	if err != nil {
		t.Fatal(err)
	}
	rules, _ = sectionByName(sections, SectionRules)
	if strings.Contains(rules.Text, "Use bash for file operations") {
		t.Errorf("fallback rule must not appear when grep is selected:\n%s", rules.Text)
	}

	// Duplicates are dropped, first wins.
	sections, err = BuildSystemPromptSections(Options{
		SelectedTools:    []string{"read"},
		PromptGuidelines: []string{"Be concise in your responses", "only once"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules, _ = sectionByName(sections, SectionRules)
	if n := strings.Count(rules.Text, "Be concise in your responses"); n != 1 {
		t.Errorf("guideline appeared %d times, want 1:\n%s", n, rules.Text)
	}
	if !strings.Contains(rules.Text, "- only once") {
		t.Errorf("custom guideline missing:\n%s", rules.Text)
	}
}

func TestHiddenToolsAreExcludedFromListAndRules(t *testing.T) {
	sections, err := BuildSystemPromptSections(Options{
		SelectedTools: []string{"read", "grep"},
		HiddenTools:   []string{"grep"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := sectionByName(sections, SectionTools)
	if strings.Contains(tools.Text, "- grep: ") {
		t.Errorf("hidden tool leaked into the tools section:\n%s", tools.Text)
	}
	rules, _ := sectionByName(sections, SectionRules)
	if strings.Contains(rules.Text, "Use bash for file operations") {
		t.Errorf("hidden grep must still suppress the shell fallback:\n%s", rules.Text)
	}
}

func TestForceSystemPromptIsOpaque(t *testing.T) {
	content, sections, err := BuildSystemPromptState(Options{
		ForceSystemPrompt: "obliterate the prompt",
		CWD:               "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if content != "obliterate the prompt" {
		t.Errorf("content = %q", content)
	}
	if sections != nil {
		t.Errorf("forced prompt must carry no sections, got %v", sectionNames(sections))
	}
	rendered := RenderSystemPrompt(content, sections)
	if rendered != "obliterate the prompt" {
		t.Errorf("rendered = %q", rendered)
	}
}

func TestCustomPromptAppendAndCustomSections(t *testing.T) {
	sections, err := BuildSystemPromptSections(Options{
		CustomPrompt:       "you are custom",
		AppendSystemPrompt: "appended text",
		CWD:                "/repo",
		Sections:           map[string]string{"team_rules": "no fridays", "empty": ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	preamble, _ := sectionByName(sections, SectionPreamble)
	if preamble.Text != "you are custom" {
		t.Errorf("preamble = %q", preamble.Text)
	}
	if _, ok := sectionByName(sections, SectionTools); ok {
		t.Error("a custom prompt replaces the built-in sections (pi: system-prompt.ts:152-170)")
	}
	addendum, ok := sectionByName(sections, SectionAddendum)
	if !ok || addendum.Text != "<addendum>\nappended text\n</addendum>" {
		t.Errorf("addendum = %q", addendum.Text)
	}
	custom, ok := sectionByName(sections, "team_rules")
	if !ok || custom.Text != "<team_rules>\nno fridays\n</team_rules>" {
		t.Errorf("custom section = %q", custom.Text)
	}
	if _, ok := sectionByName(sections, "empty"); ok {
		t.Error("an empty custom section must be dropped (pi: system-prompt.ts:184-186)")
	}
}

func TestInvalidCustomSectionNameRejected(t *testing.T) {
	for _, name := range []string{"preamble", "Preamble", "1abc", "has space", ""} {
		if _, err := BuildSystemPromptSections(Options{Sections: map[string]string{name: "x"}}); err == nil {
			t.Errorf("section name %q must be rejected", name)
		}
	}
}

func TestProjectContextSection(t *testing.T) {
	if got := projectContextSection(nil); got != "" {
		t.Errorf("no files must yield no section, got %q", got)
	}
	got := projectContextSection([]ContextFile{
		{Path: "CLAUDE.md", Content: "be nice"},
		{Path: "AGENTS.md", Content: "  "},
	})
	want := "Project-specific instructions and guidelines:\n\n" +
		`<project_instructions path="CLAUDE.md">` + "\nbe nice\n</project_instructions>"
	if got != want {
		t.Errorf("projectContextSection() =\n%q\nwant\n%q", got, want)
	}
}

func TestBuildSystemPromptRendersReplayShape(t *testing.T) {
	// The rendered text is what the transcript replays: content, then each
	// section, blank-line separated (pi's getSystemMessageText).
	opts := Options{CustomPrompt: "identity", AppendSystemPrompt: "tail", CWD: "/repo"}
	text, err := BuildSystemPrompt(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "identity\n\n<addendum>\ntail\n</addendum>\n\n<cwd>\n/repo\n</cwd>") {
		t.Errorf("rendered prompt:\n%s", text)
	}
}

func TestDiffSystemPromptSections(t *testing.T) {
	str := func(s string) *string { return &s }
	previous := map[string]*string{
		"preamble": str("old identity"),
		"tools":    str("<tools>\nsame\n</tools>"),
		"rules":    str("<rules>\nsame\n</rules>"),
	}
	current := []Section{
		{Name: "preamble", Text: "new identity"},
		{Name: "tools", Text: "<tools>\nsame\n</tools>"},
		{Name: "cwd", Text: "<cwd>\n/repo\n</cwd>"},
	}
	patch := DiffSystemPromptSections(previous, current)
	if patch == nil {
		t.Fatal("expected a patch")
	}
	if v := patch["preamble"]; v == nil || *v != "new identity" {
		t.Errorf("changed section must carry the new text, got %v", v)
	}
	if v, ok := patch["tools"]; ok {
		t.Errorf("unchanged section must not appear in the patch, got %v", v)
	}
	if v := patch["cwd"]; v == nil || *v != "<cwd>\n/repo\n</cwd>" {
		t.Errorf("new section must carry its text, got %v", v)
	}
	if v, ok := patch["rules"]; !ok || v != nil {
		t.Errorf("missing section must patch to nil (delete), got %v ok=%v", v, ok)
	}

	if patch := DiffSystemPromptSections(previous, []Section{
		{Name: "preamble", Text: "old identity"},
		{Name: "tools", Text: "<tools>\nsame\n</tools>"},
		{Name: "rules", Text: "<rules>\nsame\n</rules>"},
	}); patch != nil {
		t.Errorf("no change must yield nil patch, got %v", patch)
	}
}

func TestSectionsMap(t *testing.T) {
	m := SectionsMap([]Section{{Name: "preamble", Text: "id"}, {Name: "cwd", Text: "<cwd>\n/r\n</cwd>"}})
	if len(m) != 2 {
		t.Fatalf("map = %v", m)
	}
	if v := m["preamble"]; v == nil || *v != "id" {
		t.Errorf("preamble = %v", v)
	}
	if SectionsMap(nil) != nil {
		t.Error("nil sections must map to nil")
	}
}

func TestSnippetAndGuidelinesTable(t *testing.T) {
	// Verbatim-from-pi spot checks; the table is the port.
	if got := Snippet("read"); got != "Read file contents" {
		t.Errorf("read snippet = %q", got)
	}
	if got := Snippet("nosuchtool"); got != "" {
		t.Errorf("unknown tool snippet = %q, want empty", got)
	}
	edit := Guidelines("edit")
	if len(edit) != 4 {
		t.Fatalf("edit guidelines = %d lines, want 4", len(edit))
	}
	if !strings.HasPrefix(edit[0], "Use edit for precise changes") {
		t.Errorf("edit guideline[0] = %q", edit[0])
	}
	// The PI_* env-var line is deliberately absent (see tools.go).
	for _, name := range []string{"bash", "powershell"} {
		for _, g := range Guidelines(name) {
			if strings.Contains(g, "PI_") {
				t.Errorf("%s guideline still references PI_* env vars: %q", name, g)
			}
		}
	}
}

func TestLoadContextFiles(t *testing.T) {
	dir := t.TempDir()
	if got := LoadContextFiles(dir); got != nil {
		t.Errorf("empty dir must yield no files, got %v", got)
	}
	write(t, dir+"/CLAUDE.md", "  claude rules  ")
	write(t, dir+"/AGENTS.md", "   ") // blank: skipped
	files := LoadContextFiles(dir)
	if len(files) != 1 || files[0].Path != "CLAUDE.md" || files[0].Content != "claude rules" {
		t.Fatalf("files = %+v", files)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
