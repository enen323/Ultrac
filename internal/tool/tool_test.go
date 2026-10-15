package tool

// Tests for the shared path resolution and the tool registry, including
// the G4 acceptance criterion: adding a tool must not touch the engine.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

func TestResolveToCwdAbsolute(t *testing.T) {
	got := resolveToCwd(`C:\some\where`, "/cwd")
	if got != filepath.Clean(`C:\some\where`) {
		t.Errorf("absolute path not preserved: %q", got)
	}
}

func TestResolveToCwdRelative(t *testing.T) {
	got := resolveToCwd("sub/file.txt", "/cwd")
	want := filepath.Clean(filepath.Join("/cwd", "sub", "file.txt"))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveToCwdTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := resolveToCwd("~/x", "/cwd"); got != filepath.Join(home, "x") {
		t.Errorf("~ expansion: %q", got)
	}
}

func TestResolveToCwdStripsAtPrefix(t *testing.T) {
	// pi's CLI @file syntax must not leak into paths.
	got := resolveToCwd("@file.txt", "/cwd")
	if got != filepath.Join("/cwd", "file.txt") {
		t.Errorf("@ prefix not stripped: %q", got)
	}
}

func TestResolveToCwdNormalizesUnicodeSpaces(t *testing.T) {
	got := resolveToCwd("a\u00A0b.txt", "/cwd")
	if got != filepath.Join("/cwd", "a b.txt") {
		t.Errorf("unicode space not normalized: %q", got)
	}
}

func TestResolveToCwdWindowsDrivePath(t *testing.T) {
	if !windowsOS {
		t.Skip("Windows-only path form")
	}
	// Git Bash style /d/repo must become D:\repo.
	got := resolveToCwd("/d/repo/file.txt", "/cwd")
	if got != `D:\repo\file.txt` {
		t.Errorf("drive path conversion: %q", got)
	}
}

func TestRegistryHasEightTools(t *testing.T) {
	names := Names()
	want := []string{"bash", "edit", "find", "grep", "ls", "powershell", "read", "write"}
	if len(names) != len(want) {
		t.Fatalf("registered %v, want %v", names, want)
	}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
}

func TestRegistryAllBuildsErasedTools(t *testing.T) {
	dir := t.TempDir()
	tools := All(dir)
	if len(tools) != 8 {
		t.Fatalf("All() built %d tools", len(tools))
	}
	for _, tool := range tools {
		if tool.Name() == "" || tool.Label() == "" || tool.Description() == "" {
			t.Errorf("tool %q missing declaration fields", tool.Name())
		}
		if len(tool.Parameters()) == 0 {
			t.Errorf("tool %q has no parameters schema", tool.Name())
		}
	}
}

func TestRegistryLookup(t *testing.T) {
	dir := t.TempDir()
	tool, ok := Lookup(dir, "read")
	if !ok || tool.Name() != "read" {
		t.Fatalf("lookup read: %v %v", tool, ok)
	}
	if _, ok := Lookup(dir, "nope"); ok {
		t.Error("unknown tool must not be found")
	}
}

// unregisterForTest removes a tool from the process-wide registry.
//
// Register panics on duplicates by design, so a test that adds a tool must
// take it back out again — otherwise a second run in the same process
// (go test -count=2) both panics on the re-registration and leaves the
// demo tool visible to the registry-shape tests.
func unregisterForTest(name string) {
	regMu.Lock()
	defer regMu.Unlock()
	for i, f := range regs {
		if f.name == name {
			regs = append(regs[:i], regs[i+1:]...)
			return
		}
	}
}

// TestG4NewToolWithoutEngineChanges is the plan's G4 gate: a tool added as
// one file plus one Register call runs through the agent runtime with zero
// engine changes. The engine side is exercised for real through
// agent.NewRegistry + Prepare + Run — the same path the loop uses.
func TestG4NewToolWithoutEngineChanges(t *testing.T) {
	Register("g4-demo", func(cwd string) agent.Tool {
		return agent.Erase(&agent.AgentTool[G4Input]{
			Tool: ai.Tool{
				Name:        "g4-demo",
				Description: "demo tool for the G4 gate",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`),
			},
			Label: "g4-demo",
			Execute: func(_ context.Context, _ string, in G4Input, _ func(agent.Result)) (agent.Result, error) {
				return agent.NewTextResult(fmt.Sprintf("n=%d", in.N)), nil
			},
		})
	})
	t.Cleanup(func() { unregisterForTest("g4-demo") })

	// It shows up in this package's registry...
	names := Names()
	found := false
	for _, n := range names {
		if n == "g4-demo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("registered tool missing from Names(): %v", names)
	}

	// ...and runs through the agent runtime's registry — the same
	// agent.NewRegistry + Prepare + Run path the loop uses, with no
	// engine code involved in making the tool available.
	reg := agent.NewRegistry(All(t.TempDir())...)
	tool, ok := reg.Lookup("g4-demo")
	if !ok {
		t.Fatal("agent registry does not see the new tool")
	}
	prepared, err := tool.Prepare(map[string]any{"n": 42})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := tool.Run(context.Background(), "call-1", prepared, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := resultText(res); got != "n=42" {
		t.Errorf("result = %q", got)
	}

	// And the schema gate applies to the new tool like any other.
	if _, err := tool.Prepare(map[string]any{}); err == nil {
		t.Error("required field not enforced for the new tool")
	}
}

// G4Input is the demo tool's input.
type G4Input struct {
	N int `json:"n"`
}
