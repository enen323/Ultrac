package mcp

// tool.go — the agent.Tool a remote MCP tool becomes
// (docs/开发计划/新功能计划/mcp-plan.md §3.3).

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"sync"

	"ultrac/internal/agent"
)

// ToolPrefix is the model-visible namespace every tool from a server lives in.
//
// It is Claude Code's and the whole ecosystem's spelling, not an invention:
// a user's existing permission rules, muscle memory and documentation all
// name tools this way, and a second prefix would make every one of them miss
// (plan D5).
func ToolPrefix(server string) string {
	return "mcp__" + normalizeName(server) + "__"
}

// ToolName is the canonical model-visible name for one remote tool.
func ToolName(server, raw string) string {
	return ToolPrefix(server) + normalizeName(raw)
}

// invalidNameChars is everything a tool name may not contain. The model's
// tool-call name is an identifier in the provider's wire format, and a space
// or a dot in it is a request the provider will reject.
var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// normalizeName maps a server's or tool's own name onto an identifier.
//
// A name sanitisation changed gets a six-character hash of the original
// appended. Without it, two tools called "read file" and "read-file" would
// collide on "read_file", and the second one's registration would be dropped
// — a tool the model was promised and can never call.
func normalizeName(s string) string {
	raw := s
	s = strings.Trim(invalidNameChars.ReplaceAllString(s, "_"), "_")
	if s == "" {
		s = "unnamed"
	}
	if s != raw {
		s += "_" + shortNameHash(raw)
	}
	return s
}

func shortNameHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())[:6]
}

// remoteTool is one MCP tool as the agent loop sees it.
//
// It implements agent.Tool directly instead of going through Erase: its
// schema is a value that arrives at runtime, and Erase's type parameter is a
// compile-time one (plan A2). Everything that makes a tool a tool — the name,
// the description, the schema, the validation — is answered from what the
// server or the cache said, and what the server is told is the raw name it
// knows.
type remoteTool struct {
	host *Host
	// server is the settings key of the server this tool belongs to.
	server string
	// name is the model-visible "mcp__<server>__<tool>".
	name string
	// rawName is what tools/call is spoken with.
	rawName string
	desc    string
	schema  json.RawMessage

	// The schema is compiled once, on the first Prepare. Compiling a remote
	// schema per call is a real cost, and a schema that does not compile is
	// a per-call error rather than a crash at registration (I8).
	compileOnce sync.Once
	compiled    *agent.CompiledSchema
	compileErr  error
}

func newRemoteTool(h *Host, server, rawName, desc string, schema json.RawMessage) *remoteTool {
	return &remoteTool{
		host:    h,
		server:  server,
		name:    ToolName(server, rawName),
		rawName: rawName,
		desc:    desc,
		schema:  schema,
	}
}

// newRemoteTools builds the tools a server contributes, skipping the ones the
// configuration disabled and the ones whose names collide.
//
// Disabled tools are filtered by their RAW name, because that is what the
// configuration names: a user disables "write_file", not
// "mcp__fs__write_file" (D8).
func newRemoteTools(h *Host, spec Spec, defs []ToolDef) []agent.Tool {
	out := make([]agent.Tool, 0, len(defs))
	seen := make(map[string]bool, len(defs))
	for _, def := range defs {
		if spec.Disabled(def.Name) {
			continue
		}
		t := newRemoteTool(h, spec.Name, def.Name, def.Description, def.InputSchema)
		if seen[t.name] {
			continue
		}
		seen[t.name] = true
		out = append(out, t)
	}
	return out
}

func (t *remoteTool) Name() string                { return t.name }
func (t *remoteTool) Description() string         { return t.desc }
func (t *remoteTool) Parameters() json.RawMessage { return t.schema }

// Label is the human-readable name. It keeps the server in it because a
// transcript showing six tools called "read_file" tells the reader nothing
// about which server answered.
func (t *remoteTool) Label() string { return t.server + "." + t.rawName }

// ExecutionMode returns the empty string: this tool takes the loop's default.
//
// MCP has a per-server serial-execution option, and it is deliberately not
// mapped. The default is parallel, and a server already gets one connection
// with one reader demultiplexing by id, so serialising at the tool level
// would add a queue that protects against nothing (I7).
func (t *remoteTool) ExecutionMode() agent.ToolExecutionMode { return "" }

// Prepare validates the model's arguments against the remote schema.
//
// The value handed back is the raw map, unchanged. Re-encoding it into a Go
// struct and back out would be two marshals per call and would silently drop
// every property the schema does not mention — and a remote schema is not
// something this package knows the shape of in advance.
func (t *remoteTool) Prepare(raw map[string]any) (any, error) {
	if err := t.validate(raw); err != nil {
		return nil, fmt.Errorf("mcp tool %s: %w", t.name, err)
	}
	return raw, nil
}

func (t *remoteTool) validate(raw map[string]any) error {
	if len(t.schema) == 0 {
		// A server that declares no schema gets its arguments passed
		// through: it is the only thing that knows what it wants.
		return nil
	}
	t.compileOnce.Do(func() {
		t.compiled, t.compileErr = agent.CompileSchema(t.schema)
	})
	if t.compileErr != nil {
		// A schema this build cannot compile is a per-call error, never a
		// panic: a bad server must not be able to take the process down.
		return t.compileErr
	}
	return t.compiled.Validate(raw)
}

// Run invokes the tool. The connection is made here if this is the first call
// to the server, which is the whole point of the cache: boot pays for a file
// read, and the spawn happens only if the model actually asks for a tool.
func (t *remoteTool) Run(ctx context.Context, toolCallID string, in any, onUpdate func(agent.Result)) (agent.Result, error) {
	args, _ := in.(map[string]any)
	return t.host.Call(ctx, t.server, t.rawName, args)
}

// cachedTools converts a cache snapshot into the tool declarations a boot
// registers.
func newRemoteToolsFromCache(h *Host, spec Spec, cached []CachedTool) []agent.Tool {
	defs := make([]ToolDef, 0, len(cached))
	for _, c := range cached {
		defs = append(defs, ToolDef{Name: c.Name, Description: c.Description, InputSchema: c.Schema})
	}
	return newRemoteTools(h, spec, defs)
}

// cacheableToolsOf is the inverse: the persistable subset of a server's live
// tools. Disabled tools are already absent — newRemoteTools filtered them
// out — so a tool the user turned off cannot come back through the cache.
func cacheableToolsOf(tools []agent.Tool) []CachedTool {
	out := make([]CachedTool, 0, len(tools))
	for _, t := range tools {
		rt, ok := t.(*remoteTool)
		if !ok {
			continue
		}
		out = append(out, CachedTool{
			Name:        rt.rawName,
			Description: rt.desc,
			Schema:      rt.schema,
		})
	}
	return out
}
