package config

// mcp_test.go — the typed "mcpServers" section
// (docs/开发计划/新功能计划/mcp-plan.md P1).
//
// Five properties are load-bearing and each has a test here:
//
//	round-trip    a settings document with servers survives read → write with
//	              its other sections intact, and camelCase is what comes out
//	alias         the snake_case spellings the MCP ecosystem writes are read
//	              but never written back
//	containment   one unparseable entry is preserved and every other server
//	              still loads — the whole section only parks when the section
//	              itself is not an object of entries
//	union         a project layer's disabledTools adds to the user layer's
//	              instead of replacing it (I3)

import (
	"encoding/json"
	"strings"
	"testing"
)

// mcpDoc is a realistic settings file: two sections this package already
// typed, one it does not (hooks, so Extra is exercised), and the new
// mcpServers section.
const mcpDoc = `{
  "env": {"ANTHROPIC_MODEL": "deepseek-flash"},
  "hooks": {"Stop": [{"command": "echo done"}]},
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "D:/work"],
      "env": {"GITHUB_TOKEN": "ghp-supersecret"},
      "disabledTools": ["write_file"]
    },
    "codegraph": {
      "command": "C:/Users/ASUS/go/bin/codegraph.exe",
      "args": ["mcp"],
      "startupTimeoutSeconds": 60
    }
  }
}`

func TestMCPServersRoundTripPreservesOtherSections(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(mcpDoc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s.MCPServers) != 2 {
		t.Fatalf("mcpServers = %d entries, want 2", len(s.MCPServers))
	}
	fs := s.MCPServers["filesystem"]
	if fs.Command != "npx" {
		t.Errorf("command = %q", fs.Command)
	}
	if len(fs.Args) != 3 || fs.Args[1] != "@modelcontextprotocol/server-filesystem" {
		t.Fatalf("args = %+v", fs.Args)
	}
	if fs.Env["GITHUB_TOKEN"] != "ghp-supersecret" {
		t.Errorf("env = %+v", fs.Env)
	}
	if len(fs.DisabledTools) != 1 || fs.DisabledTools[0] != "write_file" {
		t.Fatalf("disabledTools = %+v", fs.DisabledTools)
	}
	cg := s.MCPServers["codegraph"]
	if cg.StartupTimeoutSeconds != 60 {
		t.Errorf("startupTimeoutSeconds = %d, want 60", cg.StartupTimeoutSeconds)
	}
	if cg.CallTimeoutSeconds != 0 {
		t.Errorf("callTimeoutSeconds = %d, want 0 (unstated)", cg.CallTimeoutSeconds)
	}

	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The other sections are untouched, and the section came back out with
	// the same two servers.
	for _, want := range []string{
		`"hooks"`,
		`"env":{"ANTHROPIC_MODEL":"deepseek-flash"}`,
		`"mcpServers"`,
		`"disabledTools":["write_file"]`,
		`"startupTimeoutSeconds":60`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}
	// An unstated timeout stays unstated: the engine applies its default
	// when it builds a Spec, not the file.
	if strings.Contains(string(out), "callTimeoutSeconds") {
		t.Errorf("an unstated call timeout was written back:\n%s", out)
	}
}

func TestMCPServersSnakeCaseAliasesReadButNeverWritten(t *testing.T) {
	var s Settings
	doc := `{"mcpServers": {"fs": {
      "command": "npx",
      "disabled_tools": ["write_file"],
      "startup_timeout_seconds": 45,
      "call_timeout_seconds": 90,
      "auto_start": true
    }}}`
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fs := s.MCPServers["fs"]
	if fs.StartupTimeoutSeconds != 45 {
		t.Errorf("startupTimeoutSeconds = %d, want 45 (from startup_timeout_seconds)", fs.StartupTimeoutSeconds)
	}
	if fs.CallTimeoutSeconds != 90 {
		t.Errorf("callTimeoutSeconds = %d, want 90", fs.CallTimeoutSeconds)
	}
	if !fs.AutoStart {
		t.Error("autoStart = false, want true (from auto_start)")
	}
	if len(fs.DisabledTools) != 1 || fs.DisabledTools[0] != "write_file" {
		t.Fatalf("disabledTools = %+v", fs.DisabledTools)
	}

	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, bad := range []string{"disabled_tools", "startup_timeout_seconds", "call_timeout_seconds", "auto_start"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("snake_case key %s was written back:\n%s", bad, out)
		}
	}
	for _, want := range []string{"disabledTools", "startupTimeoutSeconds", "callTimeoutSeconds", "autoStart"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("camelCase key %s missing:\n%s", want, out)
		}
	}
}

// A canonical key next to its own alias wins: the file was edited twice and
// camelCase is what gets written back.
func TestMCPServersCanonicalKeyBeatsItsAlias(t *testing.T) {
	var s Settings
	doc := `{"mcpServers": {"fs": {"command": "npx", "disabled_tools": ["a"], "disabledTools": ["b"]}}}`
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := s.MCPServers["fs"].DisabledTools
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("disabledTools = %+v, want [b]", got)
	}
}

// The containment property: one broken entry is preserved, and every other
// server still loads. Without it a typo anywhere in the section would take
// the whole MCP configuration down.
func TestMCPServersBrokenEntryDoesNotTakeOtherServersDown(t *testing.T) {
	var s Settings
	doc := `{"mcpServers": {
      "good": {"command": "npx", "args": ["-y", "server-fs"]},
      "broken": {"command": "npx", "args": "not-an-array"}
    }}`
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s.MCPServers) != 2 {
		t.Fatalf("mcpServers = %d entries, want 2 (broken one included)", len(s.MCPServers))
	}
	if !s.MCPServers["broken"].Broken() {
		t.Error("the broken entry is not marked broken")
	}
	if s.MCPServers["good"].Broken() {
		t.Error("the good entry is marked broken")
	}
	if s.MCPServers["good"].Command != "npx" {
		t.Errorf("the good entry did not load: %+v", s.MCPServers["good"])
	}
	if _, parked := s.Extra[mcpServersKey]; parked {
		t.Error("the section was parked in Extra for one bad entry")
	}

	// And the broken entry survives the write, verbatim, in the original
	// spelling — otherwise the first save would silently delete it.
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"args":"not-an-array"`) {
		t.Errorf("the broken entry was not preserved:\n%s", out)
	}
	if !strings.Contains(string(out), `"good"`) {
		t.Errorf("the good entry was lost:\n%s", out)
	}
}

// The section itself not being an object of entries is the case that DOES
// park: there is no entry to preserve, and the whole block must survive.
func TestMCPServersNonObjectSectionIsParkedInExtra(t *testing.T) {
	for _, doc := range []string{
		`{"mcpServers": [1, 2]}`,
		`{"mcpServers": "filesystem"}`,
	} {
		var s Settings
		if err := json.Unmarshal([]byte(doc), &s); err != nil {
			t.Fatalf("unmarshal %s: %v", doc, err)
		}
		if len(s.MCPServers) != 0 {
			t.Errorf("%s: typed field = %d entries, want 0", doc, len(s.MCPServers))
		}
		raw, ok := s.Extra[mcpServersKey]
		if !ok {
			t.Errorf("%s: the section was not parked in Extra", doc)
			continue
		}
		// The write goes through json.Marshal, which re-spaces the bytes;
		// what has to survive is the content, verbatim.
		compacted, err := json.Marshal(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("compact: %v", err)
		}
		out, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(out), string(compacted)) {
			t.Errorf("%s: the parked section did not survive the write:\n%s", doc, out)
		}
	}
}

func TestMCPServersMergeDisabledToolsIsAUnion(t *testing.T) {
	user := Settings{MCPServers: map[string]MCPServerConfig{
		"fs": {Command: "npx", DisabledTools: []string{"write_file"}},
	}}
	project := Settings{MCPServers: map[string]MCPServerConfig{
		"fs": {Command: "npx", DisabledTools: []string{"move_file"}},
	}}

	merged := MergeSettings(user, project)
	got := merged.MCPServers["fs"].DisabledTools
	if len(got) != 2 || got[0] != "move_file" || got[1] != "write_file" {
		t.Fatalf("disabledTools = %+v, want the sorted union [move_file write_file]", got)
	}
}

// A project layer that only names a command must not inherit the user
// layer's env — the resulting configuration would be one nobody wrote.
func TestMCPServersMergeReplacesWholeEntry(t *testing.T) {
	user := Settings{MCPServers: map[string]MCPServerConfig{
		"fs": {
			Command: "npx",
			Args:    []string{"-y", "server-fs"},
			Env:     map[string]string{"TOKEN": "secret"},
		},
	}}
	project := Settings{MCPServers: map[string]MCPServerConfig{
		"fs": {Command: "C:/fs.exe"},
	}}

	merged := MergeSettings(user, project)
	got := merged.MCPServers["fs"]
	if got.Command != "C:/fs.exe" {
		t.Errorf("command = %q, want the project layer's", got.Command)
	}
	if len(got.Args) != 0 {
		t.Errorf("args = %+v, want none (the project entry states none)", got.Args)
	}
	if len(got.Env) != 0 {
		t.Errorf("env = %+v, want none (the project entry states none)", got.Env)
	}
}

// The union is the one place a merge is not a replacement, and it has to hold
// through the whole layer stack, not just MergeSettings.
func TestMCPServersMergeLayersKeepsEveryLayer(t *testing.T) {
	user := Layer{Source: SourceUser, Settings: Settings{MCPServers: map[string]MCPServerConfig{
		"fs": {Command: "npx", DisabledTools: []string{"write_file"}},
	}}}
	project := Layer{Source: SourceProject, Settings: Settings{MCPServers: map[string]MCPServerConfig{
		"fs":   {Command: "npx", DisabledTools: []string{"move_file"}},
		"only": {Command: "only.exe"},
	}}}

	merged, problems := MergeLayers([]Layer{user, project})
	if len(problems) != 0 {
		t.Errorf("mcpServers must not produce layer-ownership problems: %+v", problems)
	}
	if len(merged.MCPServers) != 2 {
		t.Fatalf("mcpServers = %d entries, want 2", len(merged.MCPServers))
	}
	got := merged.MCPServers["fs"].DisabledTools
	if len(got) != 2 {
		t.Fatalf("disabledTools = %+v, want the union of both layers", got)
	}
	if _, ok := merged.MCPServers["only"]; !ok {
		t.Error("a project-only server did not survive the merge")
	}
}

// The union must be order-independent: the merged list feeds the schema cache
// key, and a different map iteration order must not produce a different key.
func TestMCPServersUnionIsStable(t *testing.T) {
	a := unionDisabledTools([]string{"b", "a"}, []string{"a", "c"})
	b := unionDisabledTools([]string{"a", "c"}, []string{"b", "a"})
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("union is not stable: %v vs %v", a, b)
	}
	if strings.Join(a, ",") != "a,b,c" {
		t.Errorf("union = %v, want [a b c]", a)
	}
}

func TestMCPServersGetAndSet(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(mcpDoc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := s.Get(mcpServersKey)
	if !ok {
		t.Fatal("Get(mcpServers) reported absent")
	}
	if !strings.Contains(string(raw), "filesystem") {
		t.Errorf("Get returned %s", raw)
	}

	s.Set(mcpServersKey, nil)
	if len(s.MCPServers) != 0 {
		t.Errorf("Set(nil) left %d entries", len(s.MCPServers))
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "mcpServers") {
		t.Errorf("the section was written after deletion:\n%s", out)
	}
}

// A section that will not decode must be parked by Set too, rather than
// dropped on the floor.
func TestMCPServersSetUnparseableParkedInExtra(t *testing.T) {
	var s Settings
	s.Set(mcpServersKey, json.RawMessage(`[1,2]`))
	if len(s.MCPServers) != 0 {
		t.Errorf("typed field = %d entries, want 0", len(s.MCPServers))
	}
	if _, ok := s.Extra[mcpServersKey]; !ok {
		t.Fatal("the unparseable section was not parked")
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"mcpServers":[1,2]`) {
		t.Errorf("the parked section did not survive:\n%s", out)
	}
}

// An empty section must be indistinguishable from an absent one, so a
// resolved view never reports "present but empty".
func TestMCPServersEmptyDecodesToNil(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"mcpServers": {}}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.MCPServers != nil {
		t.Errorf("mcpServers = %+v, want nil", s.MCPServers)
	}
	if _, ok := s.Extra[mcpServersKey]; ok {
		t.Error("an empty section was parked in Extra")
	}
}
