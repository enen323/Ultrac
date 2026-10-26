// Package mcp connects Ultrac to MCP servers: it spawns them, shakes hands,
// and turns their remote tools into the agent.Tool the loop executes
// (docs/开发计划/新功能计划/mcp-plan.md).
//
// # Scope, and what is deliberately missing
//
// One transport (stdio), one capability (tools), one lifecycle (spawn on
// first use, keep until shutdown). Everything else the MCP protocol defines
// — OAuth, HTTP/SSE transports, prompts, resources, sampling, elicitation —
// is out of scope, because Ultrac has no permission system to classify
// against, no plugin market to install from, and no one to ask. A tool the
// model can call is the whole surface.
//
// # Why the tool list is fixed for the process
//
// The single fact this package is built around: the agent's tool set is
// compiled once at startup and cannot be replaced (cmd/ultrac/assemble.go
// builds it; internal/agent has no SetTools). Because the provider's tools
// array is part of the prompt prefix, swapping tools mid-session would
// invalidate the whole cache — so the constraint is also the right design.
// Its consequence is this package's boot shape: a server with a schema
// cache registers its tools immediately and spawns nothing; a server without
// one must finish its handshake before boot returns, because there is no
// second chance to publish the tools it finds (plan D3).
//
// # Layering
//
// mcp depends on agent (the Tool and Result contracts), ai (ContentBlock)
// and config (the settings section and the cache directory). It never
// imports internal/tool: the eight built-in coding tools and the remote ones
// are two parallel registries, and the edge between them would be a cycle
// the moment either side wanted to name the other.
package mcp

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"ultrac/internal/config"
)

// Transport names. stdio is the only one this build speaks (plan D2); the
// others are recognised so a configuration asking for them is reported as
// unsupported instead of being silently downgraded to stdio.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

// Defaults for the two timeouts a server can state. They are applied when
// the Spec is built, never in the file: a settings entry that says nothing
// about timeouts must be written back saying nothing (internal/config/mcp.go).
//
// The startup default is the one that costs: a stdio server reached through
// `npx -y` may be downloading a package. 30s is long enough for a cold npm
// fetch and short enough that a wedged server cannot hold a boot forever.
const (
	DefaultStartupTimeout = 30 * time.Second
	DefaultCallTimeout    = 60 * time.Second
)

// Spec is the runtime shape of one MCP server: a settings entry with its
// defaults applied and its ambiguities resolved.
//
// It is what the rest of this package works with, rather than
// config.MCPServerConfig, so the file shape and the runtime shape can
// diverge: defaults live here, and a Spec is always internally consistent
// (validation happened on the way in). Nothing else in the engine needs to
// know an MCP server can be configured with snake_case keys.
type Spec struct {
	// Name is the settings key, and the middle of every tool name this
	// server contributes ("mcp__<name>__<tool>").
	Name string
	// Type is the transport; always TransportStdio today.
	Type string

	// Command and Args are the spawn line for a stdio server.
	Command string
	Args    []string
	// Env is added to the process environment. Its values are secret-
	// shaped by convention (an API key lives here far more often than a
	// PATH override), so nothing in this package prints or hashes them.
	Env map[string]string

	// DisabledTools names server-local tools that must never be exposed,
	// sorted and deduplicated.
	DisabledTools []string

	// StartupTimeout bounds the boot handshake; CallTimeout bounds one
	// tools/call. Both are always positive after FromConfig.
	StartupTimeout time.Duration
	CallTimeout    time.Duration

	// AutoStart asks for a handshake at boot even when the schema cache
	// would otherwise allow deferring it (plan D3).
	AutoStart bool
}

// FromConfig builds one Spec, applying defaults and rejecting what this
// build cannot serve.
//
// An error is a server that cannot be used, and the caller's contract is to
// skip it and report — never to fall back to a guessed configuration. The
// three things worth refusing are a transport this build does not speak, a
// stdio server with nothing to spawn, and a negative timeout (which would
// expire instantly, or never, depending on how the deadline is applied).
func FromConfig(name string, c config.MCPServerConfig) (Spec, error) {
	if c.Broken() {
		return Spec{}, fmt.Errorf("mcp server %q: entry does not parse", name)
	}

	typ := strings.ToLower(strings.TrimSpace(c.Type))
	switch typ {
	case "":
		typ = TransportStdio
	case TransportStdio:
		// The only supported case.
	default:
		return Spec{}, fmt.Errorf("mcp server %q: transport %q is not supported (Ultrac speaks stdio only)", name, typ)
	}

	if strings.TrimSpace(c.Command) == "" {
		return Spec{}, fmt.Errorf("mcp server %q: stdio transport needs a command", name)
	}
	if strings.TrimSpace(c.URL) != "" && typ == TransportStdio {
		// A stdio entry carrying a URL is a definition copied from an HTTP
		// example. Serving it would spawn a command the user never
		// intended to run.
		return Spec{}, fmt.Errorf("mcp server %q: declares a url but Ultrac speaks stdio only", name)
	}
	if c.StartupTimeoutSeconds < 0 || c.CallTimeoutSeconds < 0 {
		return Spec{}, fmt.Errorf("mcp server %q: timeouts must be >= 0 seconds", name)
	}

	s := Spec{
		Name:           name,
		Type:           typ,
		Command:        c.Command,
		Args:           append([]string(nil), c.Args...),
		Env:            copyStringMap(c.Env),
		DisabledTools:  normalizeToolNames(c.DisabledTools),
		StartupTimeout: DefaultStartupTimeout,
		CallTimeout:    DefaultCallTimeout,
		AutoStart:      c.AutoStart,
	}
	if c.StartupTimeoutSeconds > 0 {
		s.StartupTimeout = time.Duration(c.StartupTimeoutSeconds) * time.Second
	}
	if c.CallTimeoutSeconds > 0 {
		s.CallTimeout = time.Duration(c.CallTimeoutSeconds) * time.Second
	}
	return s, nil
}

// SpecsFromSettings builds every usable Spec in a resolved settings view,
// in lexical name order, plus the ones it had to refuse.
//
// The order is fixed because a boot that handshakes two servers must do it
// the same way on every run: an unstable order would make "which server
// timed out" a different answer each launch.
//
// A refused entry is returned instead of being dropped silently, because the
// user's question ("why does my MCP server not appear") is answered by
// naming it, and nothing downstream can — the server never reaches the
// host.
func SpecsFromSettings(s config.Settings) ([]Spec, []error) {
	names := config.MCPServerNames(s.MCPServers)
	out := make([]Spec, 0, len(names))
	var problems []error
	for _, name := range names {
		spec, err := FromConfig(name, s.MCPServers[name])
		if err != nil {
			problems = append(problems, err)
			continue
		}
		out = append(out, spec)
	}
	return out, problems
}

// copyStringMap clones a map. Configuration is read once and kept, so a
// later mutation of the settings view must not be visible through a Spec.
func copyStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// normalizeToolNames trims, deduplicates and sorts a tool-name list.
//
// Sorted and deduplicated because the list is part of the schema cache key:
// a reordered configuration is the same configuration and must hit the same
// cache entry (see cache.go).
func normalizeToolNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}

// Disabled reports whether a server-local tool name is excluded.
func (s Spec) Disabled(rawName string) bool {
	for _, d := range s.DisabledTools {
		if d == rawName {
			return true
		}
	}
	return false
}
