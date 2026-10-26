package mcp

// host.go — the connection pool: one entry per configured server, spawned on
// first use, and the boot that decides what the agent's tool set is
// (docs/开发计划/新功能计划/mcp-plan.md §3.1, §3.6).
//
// # Why boot is a synchronous thing here
//
// The agent's tool set is fixed for the process lifetime (see the package
// doc). So a server whose tools are not known at boot has to be asked before
// the agent exists, and there is no second chance: there is no later moment at
// which its tools could be published.
//
// That makes boot two-shaped, and each shape has a different cost:
//
//	cache hit    the tools are registered from the snapshot, no subprocess is
//	             spawned, and the boot pays a file read
//	cache miss   the handshake runs synchronously, bounded by
//	             startupTimeoutSeconds, and its tools are registered from the
//	             answer — or nothing is registered and the server is failed
//
// The failure case registers no "connect" stub. A stub only works in a runtime
// that can swap its tool registry afterwards; here the model would call the
// stub, the handshake would succeed, and the real tools would still have
// nowhere to go (plan §3.1, I1).
//
// # What is accepted as the cost
//
// A boot with N servers that have no cache is serial, worst case N × the
// startup timeout. Servers are visited in lexical name order so "which server
// is slow" has one answer, and more than one cache miss is worth a warning
// (I2).

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"ultrac/internal/agent"
	"ultrac/internal/config"
)

// State is where one server is in its lifecycle. The status surface's
// vocabulary, so a client can tell "configured but never contacted" from
// "tried and broken".
type State string

const (
	// StateCached means the tools are registered from the cache and the
	// server has not been spawned. It is not a claim that the server works —
	// only that it worked at some point and nothing has tried since.
	StateCached State = "cached"
	// StateSpawning means a handshake is in flight right now.
	StateSpawning State = "spawning"
	// StateReady means the handshake completed in this process.
	StateReady State = "ready"
	// StateFailed means the last attempt to reach it did not work. A cached
	// server that fails on its first real use moves here, and keeps the tools
	// it registered, so the status can say both things at once.
	StateFailed State = "failed"
)

// ServerStatus is one server as the status surface reports it
// (docs/开发计划/新功能计划/mcp-plan.md §3.7).
type ServerStatus struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	State     State  `json:"state"`
	Tools     int    `json:"tools"`
	Error     string `json:"error,omitempty"`
	// Instructions is kept even for a server that was never contacted: it is
	// the only thing anywhere that can say what a configured server is for.
	Instructions string `json:"instructions,omitempty"`
	// MalformedLines counts the non-JSON lines the server wrote to stdout.
	// Non-zero means it is logging to stdout, a common misconfiguration worth
	// surfacing rather than hiding (R6).
	MalformedLines int64 `json:"malformedLines,omitempty"`
}

// server is one configured server: its spec, its connection, and what is
// known about it.
//
// mu guards the connection and the lifecycle fields. It is held across a
// spawn, which is the point: without it, the first turn that calls three tools
// on one server would spawn three subprocesses.
type server struct {
	spec Spec

	mu     sync.Mutex
	client *Client
	state  State
	err    string
	tools  []agent.Tool
	// instructions comes from the cache until a handshake replaces it with
	// the authoritative answer.
	instructions string
}

// Host owns every configured server's connection.
type Host struct {
	mu      sync.Mutex
	servers map[string]*server
	warn    io.Writer
}

// NewHost builds a host over a resolved settings view and returns the tools
// every server contributes.
//
// This is the boot path. Everything expensive it does happens here, before the
// agent exists, because the slice it produces is final: it is what the model
// will be offered for the whole process.
//
// warn receives one line per unusable server. It is a parameter rather than a
// package-level writer because the two callers disagree about where warnings
// go (the print mode's stderr, the RPC mode's diagnostics channel).
func NewHost(settings config.Settings, warn io.Writer) (*Host, []agent.Tool) {
	specs, problems := SpecsFromSettings(settings)
	for _, p := range problems {
		fmt.Fprintf(warn, "warning: %v\n", p)
	}

	h := &Host{servers: make(map[string]*server, len(specs)), warn: warn}

	// One read per server, before any handshake starts, so the warning below
	// is printed while the user is still waiting rather than after the wait.
	snapshots := make(map[string]*CachedSchema, len(specs))
	var misses []string
	for _, spec := range specs {
		if cs, ok := LoadCachedSchema(spec.Name, SchemaCacheKey(spec)); ok {
			snapshots[spec.Name] = cs
			continue
		}
		misses = append(misses, spec.Name)
	}
	if len(misses) > 1 {
		// I2: several cache misses make boot serial, and the cost is the sum
		// of the timeouts. Saying so turns "the editor takes 90 seconds to
		// open" into a diagnosable problem.
		fmt.Fprintf(warn,
			"warning: %d MCP servers have no cached tool list (%s); boot waits for each handshake, up to %s each. A second start reads the cache instead.\n",
			len(misses), strings.Join(misses, ", "), DefaultStartupTimeout)
	}

	tools := make([]agent.Tool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, h.boot(spec, snapshots[spec.Name])...)
	}
	return h, tools
}

// boot registers one server's tools — the only place the decision about how a
// server enters the process is made.
func (h *Host) boot(spec Spec, cs *CachedSchema) []agent.Tool {
	s := &server{spec: spec}
	h.mu.Lock()
	h.servers[spec.Name] = s
	h.mu.Unlock()

	if cs != nil {
		s.mu.Lock()
		s.tools = newRemoteToolsFromCache(h, spec, cs.Tools)
		s.instructions = cs.Instructions
		s.state = StateCached
		s.mu.Unlock()
		// autoStart opts into paying the spawn at boot. Everything else
		// waits for the first tool call.
		if spec.AutoStart {
			h.spawn(s)
		}
		return s.snapshot().Tools
	}

	// No cache: the tools have to come from the server, and this is the only
	// moment they can be registered. A failure registers nothing and leaves
	// the server failed — never a stub (I1).
	up := h.spawn(s)
	snap := s.snapshot()
	if !up {
		fmt.Fprintf(h.warn, "warning: mcp server %q is unavailable this run: %s\n", spec.Name, snap.Error)
		return nil
	}
	return snap.Tools
}

// spawn brings a server up: handshake, list tools, publish.
//
// It holds the entry's lock for the whole thing, so two concurrent first calls
// share one subprocess.
func (h *Host) spawn(s *server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawnLocked(h)
}

// spawnLocked is the body of spawn, for callers that already hold the lock.
func (s *server) spawnLocked(h *Host) bool {
	if s.client != nil {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.spec.StartupTimeout)
	defer cancel()

	s.state = StateSpawning
	c, err := newClient(ctx, s.spec)
	if err != nil {
		s.state = StateFailed
		s.err = cleanError(err)
		return false
	}

	defs, err := c.ListTools(ctx)
	if err != nil {
		c.Close()
		s.state = StateFailed
		s.err = cleanError(err)
		return false
	}

	s.client = c
	s.state = StateReady
	s.err = ""
	s.tools = newRemoteTools(h, s.spec, defs)
	s.instructions = c.Instructions()

	// The snapshot is what the NEXT boot reads. Saving it here is how a
	// server's tool list heals across sessions: this session keeps the tools
	// it registered (the registration is pinned), and the next one gets the
	// current list.
	_ = SaveCachedSchema(s.spec.Name, CachedSchema{
		CacheKey:     SchemaCacheKey(s.spec),
		Instructions: c.Instructions(),
		Tools:        cacheableToolsOf(s.tools),
	})
	return true
}

// Call invokes one tool by its server and raw name, spawning the server if
// this is the first thing asked of it.
//
// That is the payoff for the cache: a boot that registered a server's tools
// from disk spawned nothing, and the spawn happens here, only if the model
// actually asked for one of its tools.
func (h *Host) Call(ctx context.Context, serverName, rawName string, args map[string]any) (agent.Result, error) {
	h.mu.Lock()
	s := h.servers[serverName]
	h.mu.Unlock()
	if s == nil {
		return agent.Result{}, fmt.Errorf("mcp: no server %q is configured", serverName)
	}

	// Per-server mutual exclusion: the first concurrent batch of calls to one
	// server shares a single spawn.
	s.mu.Lock()
	if s.client == nil && !s.spawnLocked(h) {
		s.mu.Unlock()
		return agent.Result{}, fmt.Errorf("mcp server %q: %s", serverName, s.err)
	}
	c := s.client
	s.mu.Unlock()

	// A tool call that fails is not the server being unreachable: the
	// connection may be perfectly good and the tool may have timed out. The
	// entry stays ready so the next call can use it.
	return c.CallTool(ctx, rawName, args)
}

// Status is the snapshot a client pulls (plan D6), sorted by name so it does
// not depend on map iteration order.
func (h *Host) Status() []ServerStatus {
	h.mu.Lock()
	names := make([]string, 0, len(h.servers))
	for name := range h.servers {
		names = append(names, name)
	}
	h.mu.Unlock()
	slices.Sort(names)

	out := make([]ServerStatus, 0, len(names))
	for _, name := range names {
		h.mu.Lock()
		s := h.servers[name]
		h.mu.Unlock()
		if s == nil {
			continue
		}
		out = append(out, s.status())
	}
	return out
}

// Close tears every spawned connection down.
//
// It is concurrent because it is a shutdown path: five servers that each
// ignore stdin EOF should cost one teardown budget, not five. Each close has
// its own internal budget, so a wedged child still cannot hold the exit.
func (h *Host) Close() {
	h.mu.Lock()
	servers := make([]*server, 0, len(h.servers))
	for _, s := range h.servers {
		servers = append(servers, s)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range servers {
		s.mu.Lock()
		c := s.client
		s.client = nil
		s.mu.Unlock()
		if c == nil {
			continue
		}
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			c.Close()
		}(c)
	}
	wg.Wait()
}

// status is one server's snapshot, taken under its own lock: a status read
// races with a spawn unless it is.
func (s *server) status() ServerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ServerStatus{
		Name:         s.spec.Name,
		Transport:    s.spec.Type,
		State:        s.state,
		Tools:        len(s.tools),
		Error:        s.err,
		Instructions: s.instructions,
	}
	if s.client != nil {
		st.MalformedLines = s.client.MalformedLines()
	}
	return st
}

// serverSnapshot is the subset of a server a caller that holds no lock may
// read. The tools slice is shared, and it is never mutated after boot — the
// registration is pinned for the process (see the file comment).
type serverSnapshot struct {
	Tools []agent.Tool
	Error string
}

func (s *server) snapshot() serverSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return serverSnapshot{Tools: s.tools, Error: s.err}
}

// cleanError collapses an error to one line. A status field and a warning line
// are read at a glance, and a multi-line stderr tail in them is noise; the
// tail belongs in the error the caller of the tool sees, where it is useful.
func cleanError(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	const max = 300
	if len(msg) > max {
		msg = msg[:max] + "..."
	}
	return msg
}
