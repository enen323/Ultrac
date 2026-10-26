// mcpharness_test.go — MCP at the harness level
// (docs/开发计划/新功能计划/mcp-plan.md P3).
//
// These tests seed the schema cache directly instead of running a server, so
// they exercise the boot path a real cache hit takes without paying for a
// subprocess. The two things they assert are the ones the plan names: the
// server's tools are in the harness's tool set, and the prompt advertises
// them to the model.

package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"ultrac/internal/config"
	"ultrac/internal/mcp"
)

// seedMCPCache writes the snapshot a previous boot would have left, so the
// boot under test takes the cache-hit path and spawns nothing. It returns the
// working directory to assemble against.
func seedMCPCache(t *testing.T, doc string) string {
	t.Helper()
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	var settings config.Settings
	if err := json.Unmarshal([]byte(doc), &settings); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	writeFile(t, filepath.Join(home, "settings.json"), doc)

	specs, problems := mcp.SpecsFromSettings(settings)
	if len(problems) != 0 {
		t.Fatalf("spec problems: %v", problems)
	}
	for _, spec := range specs {
		if err := mcp.SaveCachedSchema(spec.Name, mcp.CachedSchema{
			CacheKey:     mcp.SchemaCacheKey(spec),
			Instructions: "a fake server",
			Tools: []mcp.CachedTool{
				{Name: "read_file", Description: "reads a file",
					Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
				{Name: "list_directory", Description: "lists a directory",
					Schema: json.RawMessage(`{"type":"object"}`)},
			},
		}); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
	}
	return cwd
}

// mcpHarnessSettings is the settings document these tests assemble from. The
// provider is there because the harness fails fast on a model with no
// credential — it is not what any of them are about.
const mcpHarnessSettings = `{
  "providers": {
    "my-proxy": {
      "baseUrl": "https://proxy.example.com/v1",
      "api": "openai-completions",
      "apiKey": "sk-x",
      "models": [{"id": "my-model"}]
    }
  },
  "mcpServers": {
    "filesystem": {"command": "ultrac-fake-mcp", "args": ["mcp"]}
  }
}`

func TestHarnessCarriesTheMCPTools(t *testing.T) {
	cwd := seedMCPCache(t, mcpHarnessSettings)

	h, code := buildHarness(&agentFlags{Mode: "text", Model: "my-proxy/my-model"}, cwd, io.Discard, nil)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	defer h.Close()

	var mcpNames, builtinNames []string
	for _, tl := range h.tools {
		if strings.HasPrefix(tl.Name(), "mcp__") {
			mcpNames = append(mcpNames, tl.Name())
			continue
		}
		builtinNames = append(builtinNames, tl.Name())
	}

	// The two the server declared, under the ecosystem's spelling.
	for _, want := range []string{"mcp__filesystem__read_file", "mcp__filesystem__list_directory"} {
		if !containsOne(mcpNames, want) {
			t.Errorf("tool %q is not in the harness: %v", want, mcpNames)
		}
	}
	// The built-ins survived: MCP is appended beside them, not instead of
	// them.
	if len(builtinNames) == 0 {
		t.Error("the built-in coding tools were displaced")
	}

	// The tools are agent.Tool by construction — the loop cannot tell a
	// remote tool from a built-in one, which is the point.
	_ = mcpNames
}

// The tool list the prompt advertises is built from the harness's tool set, so
// the names have to reach it — a tool the model can call but the prompt does
// not mention is a tool it will never try.
func TestMCPToolsReachThePromptToolList(t *testing.T) {
	cwd := seedMCPCache(t, mcpHarnessSettings)

	h, code := buildHarness(&agentFlags{Mode: "text", Model: "my-proxy/my-model"}, cwd, io.Discard, nil)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	defer h.Close()

	selected := h.promptOptions().SelectedTools
	for _, want := range []string{"mcp__filesystem__read_file", "mcp__filesystem__list_directory"} {
		if !containsOne(selected, want) {
			t.Errorf("tool %q is not in the prompt's tool list (%d entries)", want, len(selected))
		}
	}
	// And the built-ins are still there: the list is the union.
	if !containsOne(selected, "read") {
		t.Error("the built-in tools are missing from the prompt's tool list")
	}
}

// A server that cannot be used contributes nothing to the harness, and its
// reason lands on the caller's warning writer — which in a real boot is the
// process's stderr.
func TestHarnessDropsAnUnusableServer(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)

	doc := `{
      "providers": {
        "my-proxy": {
          "baseUrl": "https://proxy.example.com/v1",
          "api": "openai-completions",
          "apiKey": "sk-x",
          "models": [{"id": "my-model"}]
        }
      },
      "mcpServers": {"remote": {"command": "ultrac-fake-mcp", "type": "http"}}
    }`
	writeFile(t, filepath.Join(home, "settings.json"), doc)

	var stderr strings.Builder
	h, code := buildHarness(&agentFlags{Mode: "text", Model: "my-proxy/my-model"}, cwd, &stderr, nil)
	if code != 0 {
		t.Fatalf("buildHarness exit = %d, want 0", code)
	}
	defer h.Close()

	for _, tl := range h.tools {
		if strings.HasPrefix(tl.Name(), "mcp__") {
			t.Errorf("an unusable server contributed %q", tl.Name())
		}
	}
	if !strings.Contains(stderr.String(), "stdio only") {
		t.Errorf("the refusal is not on the warning channel: %q", stderr.String())
	}
}

// containsOne reports whether a name list carries one entry.
func containsOne(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}
