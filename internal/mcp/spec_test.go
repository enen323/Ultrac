package mcp

// spec_test.go — Spec: the runtime shape of one MCP server
// (docs/开发计划/新功能计划/mcp-plan.md P1).
//
// The property under test is refusing rather than guessing. A settings entry
// can be wrong in several ways — a transport this build does not speak, a
// stdio server with nothing to spawn, a definition that came from an HTTP
// example — and each has to become an error the caller can report, because a
// Spec that was guessed at would spawn a command the user never configured.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ultrac/internal/config"
)

func specFrom(t *testing.T, doc string) (config.Settings, []Spec, []error) {
	t.Helper()
	var s config.Settings
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	specs, problems := SpecsFromSettings(s)
	return s, specs, problems
}

func TestFromConfigAppliesDefaults(t *testing.T) {
	_, specs, problems := specFrom(t, `{"mcpServers": {"fs": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "D:/work"],
      "env": {"GITHUB_TOKEN": "ghp-secret"}
    }}}`)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	got := specs[0]
	if got.Name != "fs" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Type != TransportStdio {
		t.Errorf("type = %q, want stdio (the absent default)", got.Type)
	}
	if got.StartupTimeout != DefaultStartupTimeout {
		t.Errorf("startup = %s, want the default %s", got.StartupTimeout, DefaultStartupTimeout)
	}
	if got.CallTimeout != DefaultCallTimeout {
		t.Errorf("call = %s, want the default %s", got.CallTimeout, DefaultCallTimeout)
	}
	if got.Env["GITHUB_TOKEN"] != "ghp-secret" {
		t.Errorf("env = %+v", got.Env)
	}
	if len(got.Args) != 3 {
		t.Errorf("args = %+v", got.Args)
	}
	if got.AutoStart {
		t.Error("autoStart = true, want the unstated false")
	}
}

func TestFromConfigHonoursStatedTimeouts(t *testing.T) {
	_, specs, problems := specFrom(t, `{"mcpServers": {"fs": {
      "command": "npx",
      "startup_timeout_seconds": 5,
      "callTimeoutSeconds": 120
    }}}`)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	got := specs[0]
	if got.StartupTimeout != 5*time.Second {
		t.Errorf("startup = %s, want 5s", got.StartupTimeout)
	}
	if got.CallTimeout != 120*time.Second {
		t.Errorf("call = %s, want 120s", got.CallTimeout)
	}
}

func TestFromConfigRefusesUnsupportedTransport(t *testing.T) {
	for _, typ := range []string{"http", "sse", "streamable-http"} {
		_, specs, problems := specFrom(t, `{"mcpServers": {"fs": {
          "command": "npx", "type": "`+typ+`"}}}`)
		if len(specs) != 0 {
			t.Errorf("%s: a spec was produced for an unsupported transport", typ)
		}
		if len(problems) != 1 {
			t.Fatalf("%s: problems = %v, want one refusal", typ, problems)
		}
		if got := problems[0].Error(); !strings.Contains(got, "stdio only") {
			t.Errorf("%s: refusal = %q, want it to name stdio as the only transport", typ, got)
		}
	}
}

func TestFromConfigRefusesStdioWithoutCommand(t *testing.T) {
	_, specs, problems := specFrom(t, `{"mcpServers": {"fs": {"args": ["-y", "server-fs"]}}}`)
	if len(specs) != 0 {
		t.Error("a spec was produced for a stdio server with no command")
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one refusal", problems)
	}
	if got := problems[0].Error(); !strings.Contains(got, "needs a command") {
		t.Errorf("refusal = %q", got)
	}
}

// A definition copied from an HTTP README carries a url. Serving it would
// spawn a command the user never meant to run, so it is refused rather than
// downgraded.
func TestFromConfigRefusesStdioCarryingURL(t *testing.T) {
	_, specs, problems := specFrom(t, `{"mcpServers": {"fs": {
      "command": "npx", "url": "https://mcp.example.com/mcp"}}}`)
	if len(specs) != 0 {
		t.Error("a spec was produced for a stdio entry carrying a url")
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one refusal", problems)
	}
}

func TestFromConfigRefusesNegativeTimeout(t *testing.T) {
	_, specs, problems := specFrom(t, `{"mcpServers": {"fs": {
      "command": "npx", "startupTimeoutSeconds": -1}}}`)
	if len(specs) != 0 {
		t.Error("a spec was produced for a negative timeout")
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one refusal", problems)
	}
}

// A settings entry that does not parse is refused, and the refusal names the
// server: nothing downstream can, because the entry never reaches the host.
func TestFromConfigRefusesBrokenEntry(t *testing.T) {
	_, specs, problems := specFrom(t, `{"mcpServers": {"fs": {"command": "npx", "args": "oops"}}}`)
	if len(specs) != 0 {
		t.Error("a spec was produced from an unparseable entry")
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one refusal", problems)
	}
	if got := problems[0].Error(); !strings.Contains(got, `"fs"`) {
		t.Errorf("refusal = %q, want it to name the server", got)
	}
}

// One bad server must not cost the others: this is the runtime half of the
// containment the config package guarantees on the file.
func TestSpecsFromSettingsKeepsUsableServers(t *testing.T) {
	_, specs, problems := specFrom(t, `{"mcpServers": {
      "good": {"command": "npx"},
      "bad": {"command": "npx", "type": "http"},
      "alsoGood": {"command": "C:/cg.exe"}
    }}`)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one", problems)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2", len(specs))
	}
	// Lexical order, so a boot that handshakes two servers does it the same
	// way every run and "which one timed out" has one answer.
	if specs[0].Name != "alsoGood" || specs[1].Name != "good" {
		t.Errorf("specs = %s, %s; want alsoGood, good", specs[0].Name, specs[1].Name)
	}
}

func TestDisabledToolNamesAreNormalized(t *testing.T) {
	_, specs, _ := specFrom(t, `{"mcpServers": {"fs": {
      "command": "npx",
      "disabledTools": [" b ", "a", "b", ""]}}}`)
	got := specs[0].DisabledTools
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("disabledTools = %+v, want the sorted, trimmed, deduplicated [a b]", got)
	}
	if !specs[0].Disabled("a") {
		t.Error("Disabled(a) = false")
	}
	if specs[0].Disabled("c") {
		t.Error("Disabled(c) = true")
	}
}

// A Spec must not alias the settings view it was built from: the settings
// layers are re-read at runtime, and a mutation seen through a Spec would be
// a change to a server the process already started.
func TestSpecDoesNotAliasSettingsMaps(t *testing.T) {
	_, specs, _ := specFrom(t, `{"mcpServers": {"fs": {"command": "npx", "args": ["-y"], "env": {"K": "v"}}}}`)
	specs[0].Args[0] = "mutated"
	specs[0].Env["K"] = "mutated"

	_, again, _ := specFrom(t, `{"mcpServers": {"fs": {"command": "npx", "args": ["-y"], "env": {"K": "v"}}}}`)
	if again[0].Args[0] != "-y" {
		t.Errorf("args aliased the settings view: %+v", again[0].Args)
	}
	if again[0].Env["K"] != "v" {
		t.Errorf("env aliased the settings view: %+v", again[0].Env)
	}
}

func TestSpecsFromSettingsEmpty(t *testing.T) {
	specs, problems := SpecsFromSettings(config.Settings{})
	if len(specs) != 0 || len(problems) != 0 {
		t.Errorf("specs = %v, problems = %v; want none", specs, problems)
	}
}
