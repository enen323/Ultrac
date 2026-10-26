package mcp

// transport_test.go and client_test.go — the stdio transport and the session
// over it (docs/开发计划/新功能计划/mcp-plan.md P2).
//
// The tests drive a real subprocess, because every property that matters here
// is a property of a real pipe to a real child: ids are demultiplexed across
// overlapping round trips, a cancelled call abandons its wait without killing
// the child, a crash carries the child's stderr, and teardown finishes inside
// its budget. None of those can be asserted against an in-memory fake.
//
// The child is the fake server under testdata/, built once in TestMain. It is
// a real MCP server that misbehaves on purpose: it answers out of order, it
// can die mid-call, it can die during startup, and it can write non-JSON
// noise to stdout.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// fakeServer is the built fake-server binary, shared by every test.
var fakeServer string

func TestMain(m *testing.M) {
	path, err := buildFakeServer()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build fake server:", err)
		os.Exit(1)
	}
	fakeServer = path
	code := m.Run()
	_ = os.Remove(path)
	os.Exit(code)
}

// buildFakeServer compiles the fake server into a temp file once, so twenty
// tests do not pay twenty compilations.
func buildFakeServer() (string, error) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("ultrac-fakeserver-%d.exe", os.Getpid()))
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fakeserver")
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out, nil
}

// fakeSpec builds a Spec that runs the fake server in one of its modes.
//
// The timeouts are short and explicit: a test that hangs should fail in
// seconds rather than after the 30s production default.
func fakeSpec(name, mode string) Spec {
	args := []string{}
	if mode != "" {
		args = append(args, mode)
	}
	return Spec{
		Name:           name,
		Type:           TransportStdio,
		Command:        fakeServer,
		Args:           args,
		StartupTimeout: 10 * time.Second,
		CallTimeout:    10 * time.Second,
	}
}

func TestTransportConcurrentCallsDoNotCrossTalk(t *testing.T) {
	c := mustClient(t, fakeSpec("conc", ""))
	defer c.Close()

	// Twenty overlapping calls, half of them deliberately finishing in the
	// reverse order they were made, so a transport that matched responses to
	// callers by arrival order would scramble every one of them.
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				res, err := c.CallTool(context.Background(), "echo", map[string]any{"text": fmt.Sprintf("t%d", i)})
				if err != nil {
					errs[i] = err
					return
				}
				if got := resultText(res); got != fmt.Sprintf("echo: t%d", i) {
					errs[i] = fmt.Errorf("got %q, want echo: t%d", got, i)
				}
				return
			}
			res, err := c.CallTool(context.Background(), "sleep", map[string]any{"ms": 10 * (n - i)})
			if err != nil {
				errs[i] = err
				return
			}
			if got := resultText(res); got != fmt.Sprintf("slept %dms", 10*(n-i)) {
				errs[i] = fmt.Errorf("got %q, want the sleep for this call", got)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
}

// A cancelled call must abandon its wait without killing the child. Killing
// it would make the next call pay the startup cost all over again, which is
// the whole reason a stdio server is kept alive across turns.
func TestTransportCancelDoesNotKillProcess(t *testing.T) {
	spec := fakeSpec("cancel", "")
	c := mustClient(t, spec)
	defer c.Close()

	tr := c.t.(*stdioTransport)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.CallTool(ctx, "sleep", map[string]any{"ms": 5000})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the cancelled call returned %v, want context.DeadlineExceeded", err)
	}

	// The child is still there, and still answering.
	if tr.cmd.ProcessState != nil {
		t.Fatal("the child was reaped: cancellation killed the process")
	}
	res, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "still here"})
	if err != nil {
		t.Fatalf("the transport is unusable after a cancellation: %v", err)
	}
	if got := resultText(res); got != "echo: still here" {
		t.Errorf("got %q", got)
	}
}

// The cancellation notice reaches the server, and the request it names is the
// one that was abandoned — which is what lets a well-behaved server stop
// working on it.
func TestTransportCancelNotifiesTheServer(t *testing.T) {
	spec := fakeSpec("cancel2", "")
	c := mustClient(t, spec)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "sleep", map[string]any{"ms": 5000}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the cancelled call returned %v", err)
	}
	// The notice is sent from a goroutine, so this only asserts it does not
	// deadlock the caller or tear the transport down.
	if _, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatalf("the transport is unusable after a cancellation: %v", err)
	}
}

// A server that dies mid-call: the error must carry its stderr, because the
// alternative is a bare "EOF" that tells the user nothing.
func TestTransportServerCrashCarriesStderrTail(t *testing.T) {
	c := mustClient(t, fakeSpec("boom", ""))
	defer c.Close()

	_, err := c.CallTool(context.Background(), "boom", nil)
	if err == nil {
		t.Fatal("the call succeeded though the server exited")
	}
	var sub *SubprocessError
	if !errors.As(err, &sub) {
		t.Fatalf("error = %T (%v), want a *SubprocessError carrying the stderr tail", err, err)
	}
	if !strings.Contains(sub.Output, "boom requested") {
		t.Errorf("the stderr tail is missing: %q", sub.Output)
	}
	if !strings.Contains(sub.Error(), "boom requested") {
		t.Errorf("the combined error does not show the tail: %q", sub.Error())
	}
}

// A server that dies during startup is the case the stderr tail exists for:
// its output is the only thing that says why.
func TestTransportStartupCrashCarriesStderrTail(t *testing.T) {
	_, err := newClient(context.Background(), fakeSpec("crash", "crash"))
	if err == nil {
		t.Fatal("a server that exits during startup was accepted")
	}
	var sub *SubprocessError
	if !errors.As(err, &sub) {
		t.Fatalf("error = %T (%v), want a *SubprocessError", err, err)
	}
	if !strings.Contains(sub.Output, "simulated startup failure") {
		t.Errorf("the startup stderr is missing: %q", sub.Output)
	}
}

// A server that never answers must not hold a boot forever: the startup
// timeout is the whole mechanism.
func TestTransportStartupTimeout(t *testing.T) {
	spec := fakeSpec("slow", "slow")
	spec.StartupTimeout = 300 * time.Millisecond

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := newClient(ctx, spec)
	if err == nil {
		t.Fatal("a server that never answered was accepted")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("startup took %s, want it bounded by the timeout plus teardown", elapsed)
	}
}

// A missing command must be reported with the server's name: a user with four
// servers configured needs to know which one.
func TestTransportMissingCommandNamesTheServer(t *testing.T) {
	spec := fakeSpec("missing", "")
	spec.Command = "ultrac-definitely-not-a-real-command-xyz"
	_, err := newClient(context.Background(), spec)
	if err == nil {
		t.Fatal("a missing command was accepted")
	}
	if !strings.Contains(err.Error(), `"missing"`) {
		t.Errorf("error = %q, want it to name the server", err)
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("error = %q, want it to say the command was not found", err)
	}
}

// Teardown has a budget. A server that ignores stdin EOF gets killed, and
// either path must finish quickly enough that a shutdown is never held up by
// one wedged child.
func TestTransportCloseIsBudgeted(t *testing.T) {
	c := mustClient(t, fakeSpec("closeme", ""))
	tr := c.t.(*stdioTransport)

	start := time.Now()
	c.Close()
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("close took %s, want it inside the budget", elapsed)
	}
	if tr.cmd.ProcessState == nil {
		t.Error("the child was never reaped")
	}
}

// Close twice must be a no-op rather than a second kill attempt or a panic.
func TestTransportCloseIsIdempotent(t *testing.T) {
	c := mustClient(t, fakeSpec("closetwice", ""))
	c.Close()
	c.Close()
}

// A server that logs to stdout must not take the connection down: the noise
// is skipped and counted, and it reaches the status surface (R6).
func TestTransportSkipsNonJSONStdoutLines(t *testing.T) {
	c := mustClient(t, fakeSpec("chatty", "chatty"))
	defer c.Close()

	res, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("the call failed on noise: %v", err)
	}
	if got := resultText(res); got != "echo: hi" {
		t.Errorf("got %q", got)
	}
	if got := c.MalformedLines(); got < 1 {
		t.Errorf("malformed lines = %d, want at least 1", got)
	}
}

// The transport reports itself gone instead of hanging after the child dies:
// a later call fails immediately rather than waiting for an answer that can
// never come.
func TestTransportCallsFailFastAfterDeath(t *testing.T) {
	c := mustClient(t, fakeSpec("dead", ""))
	if _, err := c.CallTool(context.Background(), "boom", nil); err == nil {
		t.Fatal("the crash was not reported")
	}
	// The reader goroutine has exited by now, so the next call must not
	// block on a write to a dead pipe.
	done := make(chan error, 1)
	go func() {
		_, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "x"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a call after the server died succeeded")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("a call after the server died blocked past the teardown budget")
	}
	c.Close()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustClient(t *testing.T, spec Spec) *Client {
	t.Helper()
	c, err := newClient(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// resultText is the concatenation of a result's text blocks, which is how the
// model would read it.
func resultText(r agent.Result) string {
	var sb strings.Builder
	for _, b := range r.Content {
		if b.Type == ai.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
