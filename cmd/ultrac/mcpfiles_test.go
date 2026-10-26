package main

// Tests for `ultrac mcp import` (docs/开发计划/新功能计划/mcp-plan.md P5).
//
// What they pin, in the order the gate asks for it:
//
//   - the file that lands on disk: camelCase keys, the snake_case aliases
//     from an MCP README folded in on the way (I5), the user layer only;
//   - all-or-nothing: one undecodable entry writes nothing at all, because a
//     half-imported file is a configuration nobody wrote;
//   - a well-formed server this build cannot serve is written and flagged,
//     not dropped — the definition is the user's data;
//   - a same-named entry is replaced wholesale and said out loud, since that
//     is where its disabledTools would silently disappear;
//   - --dry-run touches nothing.
//
// The settings file is read back through the real resolver
// (settingsLayers + MergeLayers), not through a hand-built struct, so the
// tests agree with what the engine will see at boot.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importedFile writes a source file into a temp dir and returns its path.
func importedFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeFile(t, path, body)
	return path
}

// userLayerServers reads the mcpServers section the way the engine does.
func userLayerServers(t *testing.T, cwd string) map[string]struct{ Command string } {
	t.Helper()
	out := map[string]struct{ Command string }{}
	for _, l := range settingsLayers(t, cwd) {
		for name, entry := range l.Settings.MCPServers {
			out[name] = struct{ Command string }{Command: entry.Command}
		}
	}
	return out
}

func TestMCPImportWritesCamelCaseIntoUserLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	src := importedFile(t, `{
	  "mcpServers": {
	    "filesystem": {
	      "command": "npx",
	      "args": ["-y", "@modelcontextprotocol/server-filesystem", "D:/work"],
	      "env": {"GITHUB_TOKEN": "s3cret"},
	      "disabled_tools": ["write_file"]
	    },
	    "codegraph": {
	      "command": "codegraph.exe",
	      "args": ["mcp"],
	      "startup_timeout_seconds": 60
	    }
	  }
	}`)

	var stdout, stderr strings.Builder
	if code := runMCP([]string{"import", src}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	raw, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	body := string(raw)
	// The read-side aliases never reach the file: one spelling gets written
	// back, so a file touched by both a human and the tool does not
	// oscillate between two conventions (I5).
	for _, banned := range []string{"disabled_tools", "startup_timeout_seconds"} {
		if strings.Contains(body, banned) {
			t.Errorf("settings.json kept the snake_case spelling %q:\n%s", banned, body)
		}
	}
	for _, want := range []string{`"disabledTools"`, `"startupTimeoutSeconds": 60`, `"GITHUB_TOKEN": "s3cret"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings.json missing %q:\n%s", want, body)
		}
	}

	cwd := t.TempDir()
	servers := userLayerServers(t, cwd)
	if len(servers) != 2 {
		t.Fatalf("user layer declares %d servers, want 2", len(servers))
	}
	if servers["filesystem"].Command != "npx" || servers["codegraph"].Command != "codegraph.exe" {
		t.Errorf("servers = %+v", servers)
	}

	out := stdout.String()
	for _, want := range []string{"filesystem", "added", "codegraph"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// The one thing the command cannot do for the user: the running kernel's
	// tool set is fixed at boot (§3.1).
	if !strings.Contains(out, "restart the kernel") {
		t.Errorf("report does not mention the restart:\n%s", out)
	}
}

func TestMCPImportRefusesAnUndecodableEntryAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// "args" as a string is the classic hand-edit mistake. Every other
	// server in the file is fine, and that is the point: an import that
	// wrote the good ones would leave the user with a file that half
	// matches the source.
	src := importedFile(t, `{
	  "mcpServers": {
	    "good": {"command": "good.exe"},
	    "bad": {"command": "bad.exe", "args": "-y something"}
	  }
	}`)

	var stdout, stderr strings.Builder
	if code := runMCP([]string{"import", src}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %s)", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "cannot decode bad") {
		t.Errorf("stderr does not name the entry:\n%s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings.json exists after a refused import (err = %v)", err)
	}
}

func TestMCPImportKeepsAnUnusableServerAndSaysSo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	src := importedFile(t, `{
	  "mcpServers": {
	    "remote": {"type": "http", "url": "https://example.com/mcp"}
	  }
	}`)

	var stdout, stderr strings.Builder
	if code := runMCP([]string{"import", src}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	// Written: the definition is the user's data, and a later build may
	// speak the transport.
	if _, err := os.Stat(filepath.Join(home, "settings.json")); err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	if _, ok := userLayerServers(t, t.TempDir())["remote"]; !ok {
		t.Error("the remote entry is not in the user layer")
	}
	// Flagged twice: in the per-name block and once as a warning, because
	// the reason is the same judgement boot and `ultrac config` make.
	if !strings.Contains(stdout.String(), "unusable:") || !strings.Contains(stdout.String(), "transport") {
		t.Errorf("stdout does not report the reason:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "written but unusable: remote") {
		t.Errorf("stderr has no warning:\n%s", stderr.String())
	}
}

func TestMCPImportReplacesSameNameWholesale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	// The user layer already declares filesystem with a restriction.
	writeFile(t, filepath.Join(home, "settings.json"), `{
	  "mcpServers": {
	    "filesystem": {"command": "old.exe", "disabledTools": ["write_file"]}
	  }
	}`)
	src := importedFile(t, `{"mcpServers": {"filesystem": {"command": "npx", "args": ["-y", "fs"]}}}`)

	var stdout, stderr strings.Builder
	if code := runMCP([]string{"import", src}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "replaced") {
		t.Errorf("report does not say replaced:\n%s", stdout.String())
	}

	raw, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Wholesale, the merge rule's semantics one level down (I3): the old
	// command AND its disabledTools are gone. Half-merging would produce an
	// entry nobody wrote.
	if strings.Contains(string(raw), "old.exe") || strings.Contains(string(raw), "write_file") {
		t.Errorf("the previous definition survived the replace:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"npx"`) {
		t.Errorf("the imported definition is missing:\n%s", raw)
	}
}

func TestMCPImportDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	src := importedFile(t, `{"mcpServers": {"filesystem": {"command": "npx", "args": ["-y", "fs"]}}}`)

	var stdout, stderr strings.Builder
	if code := runMCP([]string{"import", src, "--dry-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings.json exists after a dry run (err = %v)", err)
	}
	if !strings.Contains(stdout.String(), "dry run") {
		t.Errorf("stdout does not say dry run:\n%s", stdout.String())
	}
	// A dry run still shows the spawn line — that is the part worth reading
	// before agreeing to it.
	if !strings.Contains(stdout.String(), "npx -y fs") {
		t.Errorf("stdout does not show the spawn line:\n%s", stdout.String())
	}
}

func TestMCPImportNeedsTheSectionAndNamesTheAlternative(t *testing.T) {
	t.Setenv("ULTRAC_CONFIG_DIR", t.TempDir())
	// VS Code's spelling. Naming the difference is cheaper than a user
	// staring at a file that plainly has servers in it.
	src := importedFile(t, `{"servers": {"filesystem": {"command": "npx"}}}`)

	var stdout, stderr strings.Builder
	if code := runMCP([]string{"import", src}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `no mcpServers section`) ||
		!strings.Contains(stderr.String(), `"servers"`) {
		t.Errorf("stderr does not explain the shape:\n%s", stderr.String())
	}
}

func TestMCPImportUsage(t *testing.T) {
	t.Setenv("ULTRAC_CONFIG_DIR", t.TempDir())

	var stdout, stderr strings.Builder
	if code := runMCP(nil, &stdout, &stderr); code != 2 {
		t.Errorf("bare `ultrac mcp` exit = %d, want 2", code)
	}
	if code := runMCP([]string{"import"}, &stdout, &stderr); code != 2 {
		t.Errorf("`import` with no file exit = %d, want 2", code)
	}
	if code := runMCP([]string{"transplant", "x"}, &stdout, &stderr); code != 2 {
		t.Errorf("unknown subcommand exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr does not report the unknown subcommand:\n%s", stderr.String())
	}
}
