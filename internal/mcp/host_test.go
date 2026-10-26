package mcp

// host_test.go — the boot that decides the agent's tool set, and the pool
// that spawns servers on first use
// (docs/开发计划/新功能计划/mcp-plan.md P3).
//
// The four properties that matter:
//
//	miss       a server with no cache is asked at boot, and its tools are
//	           registered from the answer
//	hit        a server with a cache registers its tools from disk and spawns
//	           nothing
//	failure    a server that cannot be reached contributes NO tools, and
//	           nothing that looks like a tool either
//	one spawn  the first concurrent calls to one server share a subprocess

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ultrac/internal/config"
)

// settingsWith builds a resolved settings view holding one fake server. It
// goes through config's own parser, so these tests exercise the whole path
// from a settings file to a tool set and not a shortcut around it.
func settingsWith(t *testing.T, doc string) config.Settings {
	t.Helper()
	var s config.Settings
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return s
}

// fakeSettings is a settings document with one fake server. args are the
// spawn arguments (its mode, and anything the mode needs) and extra is
// spliced into the entry for timeouts and disabledTools.
func fakeSettings(name string, args []string, extra string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, jsonQuote(a))
	}
	sep := ""
	if extra != "" {
		sep = ", "
	}
	return fmt.Sprintf(`{"mcpServers": {"%s": {"command": %s, "args": [%s]%s%s}}}`,
		name, jsonQuote(fakeServer), strings.Join(quoted, ", "), sep, extra)
}

// jsonQuote renders a string as a JSON string literal, so a Windows path
// with backslashes can be spliced into a settings document.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// cacheHome points the cache at a fresh directory, so a test cannot be
// polluted by another test's snapshots or by the machine's real ones.
func cacheHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ULTRAC_CONFIG_DIR", dir)
	return dir
}

func TestBootWithoutCacheAsksTheServer(t *testing.T) {
	cacheHome(t)
	var warn strings.Builder

	h, tools := NewHost(settingsWith(t, fakeSettings("fs", nil, "")), &warn)
	defer h.Close()

	// Every tool the server declared is in the model's tool set.
	if len(tools) != 7 {
		t.Fatalf("tools = %d, want 7", len(tools))
	}
	for _, tl := range tools {
		if !strings.HasPrefix(tl.Name(), "mcp__fs__") {
			t.Errorf("tool %q is not in the server's namespace", tl.Name())
		}
	}

	st := h.Status()
	if len(st) != 1 {
		t.Fatalf("status = %d entries, want 1", len(st))
	}
	if st[0].State != StateReady {
		t.Errorf("state = %q, want ready (the handshake ran at boot)", st[0].State)
	}
	if st[0].Tools != 7 {
		t.Errorf("tools = %d, want 7", st[0].Tools)
	}
	if st[0].Instructions == "" {
		t.Error("instructions were not recorded")
	}
	if warn.String() != "" {
		t.Errorf("a working server produced a warning: %s", warn.String())
	}
}

// The snapshot a boot writes is what the next boot reads. This is the whole
// point of the cache: the second start pays a file read instead of a spawn.
func TestBootWritesTheCacheTheNextBootReads(t *testing.T) {
	home := cacheHome(t)

	first, _ := NewHost(settingsWith(t, fakeSettings("fs", nil, "")), &strings.Builder{})
	firstTools := first.servers["fs"].snapshot().Tools
	first.Close()

	path := filepath.Join(home, "cache", "mcp", "fs.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no cache file was written: %v", err)
	}

	var warn strings.Builder
	second, tools := NewHost(settingsWith(t, fakeSettings("fs", nil, "")), &warn)
	defer second.Close()

	// Registered from the snapshot: the server was never contacted, so the
	// client is nil and the state says so.
	if second.servers["fs"].client != nil {
		t.Error("the second boot spawned the server")
	}
	if got := second.Status()[0].State; got != StateCached {
		t.Errorf("state = %q, want cached", got)
	}
	if warn.String() != "" {
		t.Errorf("a cache hit produced a warning: %s", warn.String())
	}

	// The tool set is identical, which is the property the cache exists for:
	// the provider's tool array must not change between two runs.
	if len(tools) != len(firstTools) {
		t.Fatalf("tools = %d, want %d", len(tools), len(firstTools))
	}
	firstNames := map[string]bool{}
	for _, tl := range firstTools {
		firstNames[tl.Name()] = true
	}
	for _, tl := range tools {
		if !firstNames[tl.Name()] {
			t.Errorf("tool %q is not the one registered on the first boot", tl.Name())
		}
	}
}

// A cached server spawns only when the model actually asks for one of its
// tools — the payoff for a boot that paid a file read.
func TestCachedServerSpawnsOnFirstCall(t *testing.T) {
	cacheHome(t)

	warm, _ := NewHost(settingsWith(t, fakeSettings("fs", nil, "")), &strings.Builder{})
	warm.Close()

	h, _ := NewHost(settingsWith(t, fakeSettings("fs", nil, "")), &strings.Builder{})
	defer h.Close()
	if got := h.Status()[0].State; got != StateCached {
		t.Fatalf("state = %q, want cached before any call", got)
	}

	res, err := h.Call(context.Background(), "fs", "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("the first call failed: %v", err)
	}
	if got := resultText(res); got != "echo: hi" {
		t.Errorf("got %q", got)
	}
	if got := h.Status()[0].State; got != StateReady {
		t.Errorf("state = %q, want ready after the first call", got)
	}

	// The second call goes over the same connection, not a new one.
	c1 := h.servers["fs"].client
	if _, err := h.Call(context.Background(), "fs", "echo", map[string]any{"text": "again"}); err != nil {
		t.Fatalf("the second call failed: %v", err)
	}
	if h.servers["fs"].client != c1 {
		t.Error("the second call used another connection")
	}
}

// The first concurrent calls to one server must share a subprocess. Without
// the per-server lock, a turn that calls three tools at once would spawn
// three of them and pay three handshakes.
func TestConcurrentFirstCallsShareOneSpawn(t *testing.T) {
	cacheHome(t)
	count := filepath.Join(t.TempDir(), "spawns")

	doc := fakeSettings("fs", []string{"count", count}, "")

	warm, _ := NewHost(settingsWith(t, doc), &strings.Builder{})
	warm.Close()
	// The warm boot spawned once to fill the cache. Only what happens after
	// it is what this test is about.
	if err := os.WriteFile(count, nil, 0o644); err != nil {
		t.Fatalf("reset the spawn counter: %v", err)
	}

	h, _ := NewHost(settingsWith(t, doc), &strings.Builder{})
	defer h.Close()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := h.Call(context.Background(), "fs", "echo", map[string]any{"text": fmt.Sprint(i)}); err != nil {
				t.Errorf("call %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	b, err := os.ReadFile(count)
	if err != nil {
		t.Fatalf("read the spawn counter: %v", err)
	}
	if got := strings.Count(string(b), "start"); got != 1 {
		t.Errorf("%d spawns for 6 concurrent first calls, want 1", got)
	}
}

// A server that cannot be reached contributes nothing — and it contributes no
// connect stub either. A stub would be a dead end here: the model would call
// it, the handshake would succeed, and the real tools would still have nowhere
// to go (I1).
func TestBootFailureRegistersNoTools(t *testing.T) {
	cacheHome(t)
	var warn strings.Builder

	h, tools := NewHost(settingsWith(t, fakeSettings("bad", []string{"crash"}, "")), &warn)
	defer h.Close()

	if len(tools) != 0 {
		t.Fatalf("tools = %d for a server that could not be reached, want 0", len(tools))
	}
	st := h.Status()
	if len(st) != 1 {
		t.Fatalf("status = %d entries, want 1", len(st))
	}
	if st[0].State != StateFailed {
		t.Errorf("state = %q, want failed", st[0].State)
	}
	if st[0].Error == "" {
		t.Error("the failure has no reason on the status surface")
	}
	if !strings.Contains(warn.String(), "bad") {
		t.Errorf("the warning does not name the server: %q", warn.String())
	}
	if !strings.Contains(warn.String(), "unavailable this run") {
		t.Errorf("the warning does not say the server is out for this run: %q", warn.String())
	}
}

// Several cache misses make boot serial, and the cost is the sum of the
// timeouts. Saying so up front is what keeps "the app takes a minute to open"
// from being a mystery (I2).
func TestBootWarnsAboutSeveralColdServers(t *testing.T) {
	cacheHome(t)
	var warn strings.Builder

	doc := fmt.Sprintf(`{"mcpServers": {
      "a": {"command": %s},
      "b": {"command": %s}
    }}`, jsonQuote(fakeServer), jsonQuote(fakeServer))

	h, _ := NewHost(settingsWith(t, doc), &warn)
	defer h.Close()

	if !strings.Contains(warn.String(), "no cached tool list") {
		t.Errorf("no cold-start warning: %q", warn.String())
	}
	if !strings.Contains(warn.String(), "a, b") {
		t.Errorf("the warning does not name both servers: %q", warn.String())
	}
}

// A server declared with an unusable definition contributes nothing and says
// why. This is the runtime half of what `ultrac config` prints.
func TestBootRejectsUnusableDefinitions(t *testing.T) {
	cacheHome(t)
	var warn strings.Builder

	doc := `{"mcpServers": {"remote": {"command": "npx", "type": "http"}}}`
	h, tools := NewHost(settingsWith(t, doc), &warn)
	defer h.Close()

	if len(tools) != 0 {
		t.Errorf("tools = %d for an unsupported transport, want 0", len(tools))
	}
	if !strings.Contains(warn.String(), "stdio only") {
		t.Errorf("the warning does not say what is supported: %q", warn.String())
	}
	// A server that was refused never reaches the pool: there is nothing to
	// report on the status surface and nothing to retry.
	if len(h.Status()) != 0 {
		t.Errorf("status = %+v, want none", h.Status())
	}
}

// autoStart opts into paying the spawn at boot instead of at the first call.
func TestAutoStartSpawnsAtBoot(t *testing.T) {
	cacheHome(t)

	doc := fakeSettings("fs", nil, `"autoStart": true`)
	warm, _ := NewHost(settingsWith(t, doc), &strings.Builder{})
	warm.Close()

	h, _ := NewHost(settingsWith(t, doc), &strings.Builder{})
	defer h.Close()

	if got := h.Status()[0].State; got != StateReady {
		t.Errorf("state = %q, want ready: autoStart asked for the spawn at boot", got)
	}
}

func TestDisabledToolsAreNeverRegistered(t *testing.T) {
	cacheHome(t)

	doc := fakeSettings("fs", nil, `"disabledTools": ["echo", "sleep"]`)
	h, tools := NewHost(settingsWith(t, doc), &strings.Builder{})
	defer h.Close()

	for _, tl := range tools {
		if strings.HasSuffix(tl.Name(), "__echo") || strings.HasSuffix(tl.Name(), "__sleep") {
			t.Errorf("disabled tool %q was registered", tl.Name())
		}
	}
	if len(tools) != 5 {
		t.Errorf("tools = %d, want 7 minus the 2 disabled", len(tools))
	}

	// A disabled tool stays disabled through the cache: the next boot reads
	// the snapshot and must not resurrect it.
	h.Close()
	second, tools2 := NewHost(settingsWith(t, doc), &strings.Builder{})
	defer second.Close()
	if len(tools2) != 5 {
		t.Errorf("tools = %d on the cached boot, want 5", len(tools2))
	}
	for _, tl := range tools2 {
		if strings.HasSuffix(tl.Name(), "__echo") {
			t.Errorf("a disabled tool came back through the cache: %q", tl.Name())
		}
	}
}

func TestStatusIsSortedByName(t *testing.T) {
	cacheHome(t)

	doc := fmt.Sprintf(`{"mcpServers": {
      "zeta": {"command": %s},
      "alpha": {"command": %s}
    }}`, jsonQuote(fakeServer), jsonQuote(fakeServer))
	h, _ := NewHost(settingsWith(t, doc), &strings.Builder{})
	defer h.Close()

	st := h.Status()
	if len(st) != 2 {
		t.Fatalf("status = %d entries, want 2", len(st))
	}
	if st[0].Name != "alpha" || st[1].Name != "zeta" {
		t.Errorf("status = %s, %s; want alpha, zeta", st[0].Name, st[1].Name)
	}
}

// Shutdown drains the subprocesses: a server left running after the kernel
// exits is a leak the user cannot see.
func TestCloseReapsEverySpawnedChild(t *testing.T) {
	cacheHome(t)

	h, _ := NewHost(settingsWith(t, fakeSettings("fs", nil, "")), &strings.Builder{})
	if _, err := h.Call(context.Background(), "fs", "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	tr := h.servers["fs"].client.t.(*stdioTransport)

	h.Close()
	// The entry's connection is gone and the child it owned was reaped
	// inside the teardown budget.
	if h.servers["fs"].client != nil {
		t.Error("the connection survived Close")
	}
	if tr.cmd.ProcessState == nil {
		t.Error("the child was never reaped")
	}
}

// A server that fails on its first real use keeps the tools it registered —
// the status can then say both that the tools exist and that the server is
// unreachable, which is the honest answer. Nothing is un-registered: the
// registration is pinned for the process, and the model's view must not
// change under it.
func TestCachedServerThatFailsKeepsItsToolsRegistered(t *testing.T) {
	cacheHome(t)

	// A server that no longer exists on disk, with the snapshot a boot would
	// have written while it did. The cache key is computed from the parsed
	// settings, so it matches what the boot below will look for.
	const missing = "ultrac-not-a-real-command"
	doc := fmt.Sprintf(`{"mcpServers": {"gone": {"command": %s}}}`, jsonQuote(missing))
	settings := settingsWith(t, doc)
	specs, problems := SpecsFromSettings(settings)
	if len(problems) != 0 || len(specs) != 1 {
		t.Fatalf("specs = %d, problems = %v", len(specs), problems)
	}
	if err := SaveCachedSchema("gone", CachedSchema{
		CacheKey: SchemaCacheKey(specs[0]),
		Tools: []CachedTool{{
			Name:        "read_file",
			Description: "reads a file",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}

	h, tools := NewHost(settings, &strings.Builder{})
	defer h.Close()
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1 (registered from the snapshot)", len(tools))
	}
	if got := h.Status()[0].State; got != StateCached {
		t.Fatalf("state = %q, want cached", got)
	}

	// The spawn fails: the command is gone. The registration stands.
	if _, err := h.Call(context.Background(), "gone", "read_file", map[string]any{"path": "x"}); err == nil {
		t.Fatal("the call succeeded with the command missing")
	}
	st := h.Status()[0]
	if st.State != StateFailed {
		t.Errorf("state = %q, want failed", st.State)
	}
	if st.Error == "" {
		t.Error("the failure has no reason on the status surface")
	}
	if st.Tools != 1 {
		t.Errorf("tools = %d, want the 1 that is still registered", st.Tools)
	}
}

// A tool call failing is not the server being unreachable, and the connection
// must survive it.
func TestFailedToolCallKeepsTheConnection(t *testing.T) {
	cacheHome(t)

	h, _ := NewHost(settingsWith(t, fakeSettings("fs", nil, "")), &strings.Builder{})
	defer h.Close()

	if _, err := h.Call(context.Background(), "fs", "fail", nil); err != nil {
		t.Fatalf("a reported tool failure came back as a Go error: %v", err)
	}
	if _, err := h.Call(context.Background(), "fs", "echo", map[string]any{"text": "still here"}); err != nil {
		t.Fatalf("the connection did not survive a failed call: %v", err)
	}
	if got := h.Status()[0].State; got != StateReady {
		t.Errorf("state = %q, want ready", got)
	}
}

func TestNoServersConfiguredCostsNothing(t *testing.T) {
	cacheHome(t)
	var warn strings.Builder

	h, tools := NewHost(config.Settings{}, &warn)
	defer h.Close()
	if len(tools) != 0 {
		t.Errorf("tools = %d, want 0", len(tools))
	}
	if len(h.Status()) != 0 {
		t.Errorf("status = %+v, want none", h.Status())
	}
	if warn.String() != "" {
		t.Errorf("an empty configuration produced a warning: %s", warn.String())
	}
}
