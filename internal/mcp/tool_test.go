package mcp

// tool_test.go — the agent.Tool a remote tool becomes
// (docs/开发计划/新功能计划/mcp-plan.md §3.3).

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolNameIsTheEcosystemSpelling(t *testing.T) {
	// mcp__<server>__<tool>, which is what Claude Code, Reasonix and every
	// MCP document name them. A user's existing permission rules and habits
	// spell it this way, and a second spelling would make every one of them
	// miss (D5).
	if got := ToolName("codegraph", "codegraph_explore"); got != "mcp__codegraph__codegraph_explore" {
		t.Errorf("ToolName = %q", got)
	}
	if got := ToolPrefix("codegraph"); got != "mcp__codegraph__" {
		t.Errorf("ToolPrefix = %q", got)
	}
}

// Two tools whose names sanitise to the same identifier must not collide: the
// second one's registration would be dropped and the model would be calling a
// tool it was promised and can never reach.
func TestToolNameSeparatesNamesThatSanitiseAlike(t *testing.T) {
	a := ToolName("fs", "read file")
	b := ToolName("fs", "read-file")
	if a == b {
		t.Errorf("two tools share the name %q", a)
	}
	for _, n := range []string{a, b} {
		if !strings.HasPrefix(n, "mcp__fs__") {
			t.Errorf("%q is not in the server's namespace", n)
		}
	}
	if strings.ContainsAny(a, " .") {
		t.Errorf("%q contains a character a tool name may not", a)
	}
}

func TestNormalizeNameKeepsLegalNames(t *testing.T) {
	for _, want := range []string{"read_file", "readFile", "read-1", "Read"} {
		if got := normalizeName(want); got != want {
			t.Errorf("normalizeName(%q) = %q, want it unchanged", want, got)
		}
	}
	// An empty name becomes "unnamed", and because sanitisation changed it
	// it also gets the hash: a server with two nameless tools must not
	// register them under one name.
	if got := normalizeName(""); !strings.HasPrefix(got, "unnamed_") {
		t.Errorf("normalizeName(\"\") = %q, want the unnamed placeholder with a hash", got)
	}
}

// Disabled tools are named by their RAW name in the configuration: a user
// disables "write_file", not "mcp__fs__write_file" (D8).
func TestDisabledToolsAreFilteredByRawName(t *testing.T) {
	cacheHome(t)
	h := &Host{servers: map[string]*server{}}
	spec := Spec{Name: "fs", DisabledTools: []string{"write_file", "move file"}}

	defs := []ToolDef{
		{Name: "read_file", Description: "reads", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "write_file", Description: "writes", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "move file", Description: "moves", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	tools := newRemoteTools(h, spec, defs)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if tools[0].Name() != "mcp__fs__read_file" {
		t.Errorf("the surviving tool is %q", tools[0].Name())
	}
}

func TestRemoteToolShape(t *testing.T) {
	h := &Host{servers: map[string]*server{}}
	spec := Spec{Name: "fs"}
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	tools := newRemoteTools(h, spec, []ToolDef{
		{Name: "read_file", Description: "reads a file", InputSchema: schema},
	})
	tl := tools[0]

	if tl.Name() != "mcp__fs__read_file" {
		t.Errorf("name = %q", tl.Name())
	}
	if tl.Description() != "reads a file" {
		t.Errorf("description = %q", tl.Description())
	}
	if string(tl.Parameters()) != string(schema) {
		t.Errorf("parameters = %s, want the raw schema", tl.Parameters())
	}
	// The label carries the server, because a transcript showing six tools
	// called "read_file" tells the reader nothing.
	if tl.Label() != "fs.read_file" {
		t.Errorf("label = %q", tl.Label())
	}
	// The loop default, not a per-server serial queue (I7).
	if tl.ExecutionMode() != "" {
		t.Errorf("execution mode = %q, want the empty default", tl.ExecutionMode())
	}
}

func TestPrepareValidatesAgainstTheRemoteSchema(t *testing.T) {
	h := &Host{servers: map[string]*server{}}
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	tl := newRemoteTool(h, "fs", "read_file", "reads", schema)

	in, err := tl.Prepare(map[string]any{"path": "a.txt"})
	if err != nil {
		t.Fatalf("valid arguments were rejected: %v", err)
	}
	// The value is the raw map, not a re-encoded struct: a remote schema is
	// not something this package knows the shape of in advance.
	args, ok := in.(map[string]any)
	if !ok {
		t.Fatalf("prepared value = %T, want the raw map", in)
	}
	if args["path"] != "a.txt" {
		t.Errorf("prepared args = %+v", args)
	}

	if _, err := tl.Prepare(map[string]any{}); err == nil {
		t.Error("a missing required property was accepted")
	}
	if _, err := tl.Prepare(map[string]any{"path": 42}); err == nil {
		t.Error("a wrong-typed property was accepted")
	}
}

// A schema this build cannot compile costs the tool its calls, not the process
// its life. A bad server must not be able to take the engine down (I8).
func TestBrokenSchemaIsAPerCallErrorNotAPanic(t *testing.T) {
	h := &Host{servers: map[string]*server{}}
	tl := newRemoteTool(h, "fs", "weird", "weird", json.RawMessage(`{"type":"nonsense"}`))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a broken schema panicked: %v", r)
		}
	}()
	if _, err := tl.Prepare(map[string]any{"x": 1}); err == nil {
		t.Error("a broken schema was accepted")
	}
}

// A server that declares no schema has its arguments passed through: it is the
// only thing that knows what it wants.
func TestNoSchemaPassesArgumentsThrough(t *testing.T) {
	h := &Host{servers: map[string]*server{}}
	tl := newRemoteTool(h, "fs", "loose", "loose", nil)

	in, err := tl.Prepare(map[string]any{"anything": "goes"})
	if err != nil {
		t.Fatalf("arguments were rejected for a tool with no schema: %v", err)
	}
	if _, ok := in.(map[string]any); !ok {
		t.Fatalf("prepared value = %T", in)
	}
}

// The cache must record the RAW name, because that is what tools/call is
// spoken with — a snapshot holding the model-visible name could never be used
// to make a call.
func TestCacheableToolsRecordRawNames(t *testing.T) {
	h := &Host{servers: map[string]*server{}}
	tools := newRemoteTools(h, Spec{Name: "fs"}, []ToolDef{
		{Name: "read file", Description: "reads", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
	cached := cacheableToolsOf(tools)
	if len(cached) != 1 {
		t.Fatalf("cached = %d, want 1", len(cached))
	}
	if cached[0].Name != "read file" {
		t.Errorf("cached name = %q, want the raw one", cached[0].Name)
	}
	// And the round trip back to a tool produces the model-visible name.
	back := newRemoteToolsFromCache(h, Spec{Name: "fs"}, cached)
	// The model-visible name is the sanitized raw one, hash suffix and all.
	if !strings.HasPrefix(back[0].Name(), "mcp__fs__read_file") {
		t.Errorf("name = %q, want the sanitized raw name", back[0].Name())
	}
}

// The tool set the pool produces is the one the prompt advertises, so the
// names have to be the ecosystem spelling end to end.
func TestToolsCarryTheirServerNamespace(t *testing.T) {
	cacheHome(t)
	h, tools := NewHost(settingsWith(t, fakeSettings("fs", nil, "")), &strings.Builder{})
	defer h.Close()

	for _, tl := range tools {
		if !strings.HasPrefix(tl.Name(), "mcp__fs__") {
			t.Errorf("tool %q is not namespaced", tl.Name())
		}
	}
	if len(tools) == 0 {
		t.Fatal("no tools were produced")
	}
}
