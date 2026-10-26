package config

// mcp.go — the typed "mcpServers" section
// (docs/开发计划/新功能计划/mcp-plan.md D1).
//
// # One source, because the file is the contract
//
// settings.json is the only place an MCP server is declared (D1): no
// .mcp.json, no global config file, no second format. The entry shape is
// Claude's mcpServers reduced to what this build can actually use — stdio
// only (D2) — and the field names are camelCase to match the rest of the
// settings file (providers.baseUrl).
//
// The snake_case spellings the MCP ecosystem documents
// (startup_timeout_seconds, disabled_tools, auto_start) are accepted on
// read and never written. That asymmetry is deliberate: a server definition
// pasted out of any MCP README works, and what Ultrac writes back is one
// spelling, so a file touched by both a human and the tool does not
// oscillate between two conventions (I5).

import (
	"encoding/json"
	"slices"
	"strings"
)

// mcpServersKey is the top-level key the MCP section occupies — the fourth
// typed carve-out beside env, compaction and providers.
const mcpServersKey = "mcpServers"

// mcpFieldAliases maps the snake_case field names the MCP ecosystem writes
// onto the camelCase names this file is written back in. Read-only: nothing
// ever produces the left column.
var mcpFieldAliases = map[string]string{
	"disabled_tools":          "disabledTools",
	"startup_timeout_seconds": "startupTimeoutSeconds",
	"call_timeout_seconds":    "callTimeoutSeconds",
	"auto_start":              "autoStart",
	"tool_timeout_seconds":    "toolTimeoutSeconds",
}

// MCPServerConfig is one entry of the "mcpServers" section: everything
// needed to start one MCP server.
//
// URL and Headers belong to the HTTP transports this build does not speak.
// They are declared anyway, for two reasons: a definition carrying them is
// recognised as unsupported instead of being mangled into a stdio one, and a
// server's identity for the schema cache is the whole definition, not the
// subset this build currently reads (see internal/mcp's cache key).
//
// There is no "enabled" flag: deleting the entry disables the server (D1).
type MCPServerConfig struct {
	// Type is the transport. Absent or "stdio" means stdio, the only one
	// this build speaks.
	Type string `json:"type,omitempty"`

	// Command is the executable a stdio server is spawned from.
	Command string `json:"command,omitempty"`
	// Args are the spawn arguments, after the command.
	Args []string `json:"args,omitempty"`
	// Env is the added environment. Its values are the most likely place a
	// credential hides, so nothing prints or hashes them (I6).
	Env map[string]string `json:"env,omitempty"`

	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// DisabledTools names server-local tools that must never be exposed.
	DisabledTools []string `json:"disabledTools,omitempty"`

	// StartupTimeoutSeconds bounds the boot handshake; CallTimeoutSeconds
	// bounds one tools/call. Zero means "not stated" and the engine
	// defaults apply when the spec is built, not here: a file that never
	// mentioned them must stay byte-identical when written back.
	StartupTimeoutSeconds int `json:"startupTimeoutSeconds,omitempty"`
	CallTimeoutSeconds    int `json:"callTimeoutSeconds,omitempty"`

	// AutoStart selects eager versus lazy: false means the cached tool
	// placeholders are registered but nothing spawns until a tool is
	// called (D3).
	AutoStart bool `json:"autoStart,omitempty"`

	// raw carries an entry that did not decode into this shape, verbatim,
	// and broken marks that it did. Unexported: a preservation mechanism,
	// not a field. Two fields rather than one because an empty valid entry
	// and an unparseable one would otherwise be indistinguishable on a
	// zero-length raw.
	raw    json.RawMessage
	broken bool
}

// Broken reports whether this entry did not decode into the typed shape and
// is being carried byte-for-byte so a write cannot destroy it.
func (c MCPServerConfig) Broken() bool { return c.broken }

// UnmarshalJSON decodes one entry, rewriting the snake_case aliases and
// capturing anything it cannot decode.
//
// It never fails, and that is the whole point: the section is a map of
// entries, so an error returned here would propagate to the map decode and
// park the whole mcpServers block in Extra — one typo in one server taking
// every other server down with it. A non-object value (a number where an
// entry belongs) is captured the same way, so "the section did not decode"
// and "one entry did not decode" stay separate cases: the first is I4 and
// parks the section, the second keeps every other server alive.
func (c *MCPServerConfig) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		c.capture(data)
		return nil
	}

	renamed := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		canon, aliased := mcpFieldAliases[k]
		if !aliased {
			renamed[k] = v
			continue
		}
		// The canonical spelling next to its own alias wins: the file was
		// probably edited twice, and camelCase is what gets written back.
		if _, dup := obj[canon]; dup {
			continue
		}
		renamed[canon] = v
	}

	buf, err := json.Marshal(renamed)
	if err != nil {
		c.capture(data)
		return nil
	}
	type plain MCPServerConfig
	var p plain
	if err := json.Unmarshal(buf, &p); err != nil {
		c.capture(data)
		return nil
	}
	*c = MCPServerConfig(p)
	return nil
}

func (c *MCPServerConfig) capture(data []byte) {
	c.raw = append(json.RawMessage(nil), data...)
	c.broken = true
}

// MarshalJSON re-emits a broken entry verbatim and a good one in canonical
// camelCase.
//
// The receiver is a VALUE and that is load-bearing: entries are map values,
// which are not addressable, so a pointer receiver would be invisible to the
// encoder and every broken entry would come back out as an empty object —
// silent data loss on the next save. (See the mixed-receiver note on
// Settings.MarshalJSON for the same trap one level up.)
func (c MCPServerConfig) MarshalJSON() ([]byte, error) {
	if c.broken && len(c.raw) > 0 {
		return c.raw, nil
	}
	type plain MCPServerConfig
	return json.Marshal(plain(c))
}

// decodeMCPServers parses the "mcpServers" section into the typed field.
//
// The park-on-failure rule matches env / compaction / providers: a section
// that is not an object of entries stays in Extra byte-for-byte so a later
// write cannot erase it, and the typed field stays nil (the layer declares
// no servers, which is the safe reading). Note how narrow that is — this is
// the SECTION failing to decode. One unparseable entry is carried inside the
// map by MCPServerConfig, and every other server still loads.
func decodeMCPServers(raw json.RawMessage, s *Settings) map[string]MCPServerConfig {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]MCPServerConfig
	if err := json.Unmarshal(raw, &m); err != nil {
		if s.Extra != nil {
			s.Extra[mcpServersKey] = raw
		}
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// mergeMCPServers merges the typed mcpServers section per server name: a
// later layer's entry replaces the earlier one wholesale (I3).
//
// Not field-level, for the same reason providers are not
// (settings-unify-plan.md D2): a project-layer entry naming only a command
// must not inherit the user layer's env or timeouts, because the result
// would be a combination nobody configured.
//
// DisabledTools is the one exception and it is a union. A project file that
// declares a server would otherwise silently lift every restriction the user
// set on it — and a repository is exactly where a cloned entry comes from
// (I3).
//
// The union is sorted and deduplicated because the merged list feeds the
// schema cache key: two identical configurations must not hash differently
// just because a map iterated in another order.
func mergeMCPServers(base, over map[string]MCPServerConfig) map[string]MCPServerConfig {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]MCPServerConfig, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if existing, ok := out[k]; ok && !existing.broken && !v.broken {
			v.DisabledTools = unionDisabledTools(existing.DisabledTools, v.DisabledTools)
		}
		out[k] = v
	}
	return out
}

// unionDisabledTools merges two disabled-tool lists, sorted and deduplicated.
// Entries are trimmed rather than dropped on empty: a trailing space in a
// hand-edited name would otherwise become a second entry that never matches.
func unionDisabledTools(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, name := range slices.Concat(a, b) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}

// MCPServerNames lists a section's server names in lexical order, so a
// diagnostic is stable across runs. Broken entries are included: they are
// still entries, and the one the user is trying to fix is the one most likely
// to be broken.
func MCPServerNames(m map[string]MCPServerConfig) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
