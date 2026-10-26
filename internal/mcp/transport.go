package mcp

// transport.go — the stdio transport: newline-delimited JSON-RPC 2.0 over a
// subprocess's stdin/stdout (docs/开发计划/新功能计划/mcp-plan.md §1.2).
//
// # The four things this file gets right
//
//  1. A single reader goroutine owns stdout and demultiplexes each response
//     to the call waiting on its id. No lock spans a round trip, so twenty
//     overlapping calls share one pipe and one slow tool cannot hold it
//     against the others.
//  2. A cancelled call abandons its wait, it does not kill the child. The
//     child outlives the turn — killing it would make the next call pay the
//     startup cost all over again.
//  3. stderr is kept as a 16KB tail. Without it, "my MCP server does not
//     work" has exactly one symptom, `exit status 1`, and no cause.
//  4. Close is budgeted: stdin EOF first, a short grace period, then the
//     whole process tree. One wedged server must never stall a boot or a
//     shutdown.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol revision Ultrac advertises during initialize. MCP servers are
// supposed to answer with the version they can speak; transport.go records
// what they said and the client negotiates against it.
const protocolVersion = "2025-11-25"

const (
	// tailLimit is how much stderr is kept. 16KB is enough for a Python
	// traceback or an npm resolution failure and small enough that a
	// server logging forever cannot grow the process without bound.
	tailLimit = 16 * 1024

	// gracefulCloseWait is how long stdin EOF gets to produce a clean exit
	// before the tree is killed. Short, because a server that is going to
	// exit on EOF does so immediately.
	gracefulCloseWait = 750 * time.Millisecond
	// closeWait is the total budget for teardown, kill included.
	closeWait = 5 * time.Second

	// replyQueueBound caps buffered replies to server→client requests. The
	// queue only backs up while the reply writer is stuck behind a jammed
	// stdin pipe, and a dropped reply degrades to the server's own timeout
	// — a blocked read loop could deadlock both pipes.
	replyQueueBound = 16
)

// transport is one live connection to a server. The interface exists so
// remoteTool does not name a concrete transport: HTTP/SSE would be a second
// implementation and nothing in the tool or the host would change (plan D2).
type transport interface {
	// call sends a request and waits for its response.
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// notify sends a request that has no response.
	notify(ctx context.Context, method string, params any) error
	// close tears the connection down within a budget, always.
	close()
}

// ---------------------------------------------------------------------------
// JSON-RPC wire shapes
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"` // omitted for notifications; 0 is unused
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcInbound is the shape every inbound line is first decoded into, because
// the same stream carries three kinds of message and the id/method
// combination is what tells them apart. ID is a pointer so an absent id (a
// notification) is distinguishable from id 0.
type rpcInbound struct {
	Method string          `json:"method"`
	ID     *int            `json:"id,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// rpcError is a JSON-RPC error object.
// rpcErrorResponse is a JSON-RPC response carrying only an error, which is
// how a request the client cannot serve is answered.
type rpcErrorResponse struct {
	JSONRPC string   `json:"jsonrpc"`
	ID      int      `json:"id"`
	Error   rpcError `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("MCP error %d", e.Code)
	}
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
}

// ---------------------------------------------------------------------------
// stdioTransport
// ---------------------------------------------------------------------------

// stdioTransport speaks NDJSON JSON-RPC 2.0 over one child process.
type stdioTransport struct {
	name string
	cmd  *exec.Cmd

	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *tailBuffer

	// writeMu guards stdin only. It is never held across a round trip,
	// which is what lets calls overlap.
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResponse
	readErr error // set once the reader goroutine exits; later calls fail fast

	// malformed counts stdout lines that were not JSON. Some servers log to
	// stdout before their first response, and failing the connection for a
	// log line is the wrong trade (R6).
	malformed atomic.Int64

	waitOnce sync.Once
	closed   atomic.Bool
}

// openStdio starts one stdio server.
//
// The child is deliberately NOT tied to a context: it must outlive the
// individual calls made over it, and a cancelled call must not kill it
// (see the file comment). Teardown is close()'s job, and close() is always
// called by the host.
func openStdio(spec Spec) (*stdioTransport, error) {
	exe, err := resolveCommand(spec.Command)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", spec.Name, err)
	}
	cmd := exec.Command(exe, spec.Args...)
	cmd.Env = mergeEnv(os.Environ(), spec.Env)
	prepareProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: stdin pipe: %w", spec.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: stdout pipe: %w", spec.Name, err)
	}
	stderr := &tailBuffer{limit: tailLimit}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp server %q: start %s: %w", spec.Name, spec.Command, err)
	}

	t := &stdioTransport{
		name:    spec.Name,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		stderr:  stderr,
		pending: make(map[int]chan rpcResponse),
	}
	go t.readLoop()
	return t, nil
}

// resolveCommand turns a configured command into one exec can start.
//
// The lookup is done here rather than left to exec.Command for one reason:
// its own failure is "exec: %q: executable file not found in $PATH", which
// does not say which MCP server was being started. A user with four servers
// configured needs the name.
func resolveCommand(command string) (string, error) {
	if strings.ContainsAny(command, `/\`) {
		return command, nil
	}
	exe, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("command %q not found on PATH: %w", command, err)
	}
	return exe, nil
}

// readLoop is the only reader of stdout, for the transport's whole life. It
// routes each response to the call waiting on its id, answers server requests
// it cannot serve, ignores notifications, and on any read error unblocks
// every pending call and exits.
func (t *stdioTransport) readLoop() {
	// Replies go through a queue and a writer goroutine, never straight to
	// stdin from here: this goroutine is the only thing draining stdout, and
	// if it blocked on a jammed stdin write while the server was blocked on
	// a full stdout, both pipes would deadlock.
	replies := make(chan any, replyQueueBound)
	defer close(replies)
	go t.replyLoop(replies)

	for {
		line, readErr := t.stdout.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			t.handleInboundLine(line, replies)
		}
		if readErr != nil {
			t.failAll(readErr)
			return
		}
	}
}

func (t *stdioTransport) replyLoop(replies <-chan any) {
	var dead bool
	for msg := range replies {
		if dead {
			continue // keep draining so readLoop never blocks
		}
		if t.write(msg) != nil {
			dead = true
		}
	}
}

func (t *stdioTransport) handleInboundLine(line []byte, replies chan<- any) {
	var probe rpcInbound
	if err := json.Unmarshal(line, &probe); err != nil {
		// A log line on stdout, most likely. Count it and keep going: the
		// transport is still usable and the count reaches the status
		// surface (R6).
		t.malformed.Add(1)
		return
	}

	if probe.Method != "" {
		if probe.ID != nil {
			// A server→client request. Ultrac advertises no capability
			// that would make one legitimate (no roots, no sampling, no
			// elicitation), and leaving it unanswered would hang a server
			// that asked, so it is refused at once.
			t.queueUnsupported(replies, *probe.ID)
			return
		}
		return // a notification: nothing to do
	}

	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.malformed.Add(1)
		return
	}
	t.mu.Lock()
	ch := t.pending[resp.ID]
	delete(t.pending, resp.ID)
	t.mu.Unlock()
	if ch != nil {
		// Buffered(1): never blocks, even if the caller already gave up and
		// its goroutine is gone.
		ch <- resp
	}
}

// queueUnsupported answers a server request with JSON-RPC -32601 (method not
// found). Dropped silently when the reply queue is full — an unanswered
// request degrades to the server's own timeout, whereas a blocked read loop
// could deadlock both pipes (see replyQueueBound).
func (t *stdioTransport) queueUnsupported(replies chan<- any, id int) {
	select {
	case replies <- rpcErrorResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   rpcError{Code: -32601, Message: "method not found"},
	}:
	default:
	}
}

// failAll records the terminal read error and unblocks every pending call by
// closing its channel. A caller tells a real response from this by the
// closed-channel receive.
func (t *stdioTransport) failAll(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.readErr == nil {
		t.readErr = err
	}
	for id, ch := range t.pending {
		close(ch)
		delete(t.pending, id)
	}
}

func (t *stdioTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	t.mu.Lock()
	if t.readErr != nil {
		t.mu.Unlock()
		// Nothing was written for this request, so the failure is not
		// ambiguous: the transport is gone.
		return nil, t.withStderr(&readFailure{server: t.name, method: method, cause: t.readErr})
	}
	t.nextID++
	id := t.nextID
	ch := make(chan rpcResponse, 1)
	t.pending[id] = ch
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
	}()

	if err := t.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		// Abandon the wait; do NOT kill the child (see the file comment).
		// The cancel notice is sent from a goroutine because the caller is
		// leaving right now and stdin may be busy.
		go func() {
			_ = t.notify(context.Background(), "notifications/cancelled", map[string]any{
				"requestId": id,
				"reason":    ctx.Err().Error(),
			})
		}()
		// The deadline is the caller's; the child may still answer and the
		// response is dropped on the floor by the delete above.
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, t.withStderr(&readFailure{server: t.name, method: method, cause: t.readErr})
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp server %q: %w", t.name, resp.Error)
		}
		return resp.Result, nil
	}
}

func (t *stdioTransport) notify(ctx context.Context, method string, params any) error {
	_ = ctx
	return t.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (t *stdioTransport) write(v any) error {
	b, err := json.Marshal(v) // marshalled JSON never contains a literal newline
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(append(b, '\n')); err != nil {
		return t.withStderr(fmt.Errorf("mcp server %q: write: %w", t.name, err))
	}
	return nil
}

// withStderr attaches the child's stderr tail to an error. Without it the
// only symptom of a server that prints a traceback and dies is `EOF`, which
// tells the user nothing they can act on.
//
// It reaps the child first, with a budget: the stderr copy goroutine only
// flushes when the child's pipes close, and an unbounded wait here would
// strand the very call that is trying to report why the server went away.
func (t *stdioTransport) withStderr(err error) error {
	if t.stderr == nil {
		return err
	}
	waitWithBudget(t.wait, closeWait)
	if tail := t.stderr.String(); tail != "" {
		return &SubprocessError{Cause: err, Output: tail}
	}
	return err
}

// MalformedLines counts the stdout lines that were not JSON. It belongs to the
// status surface: a non-zero count means the server is logging to stdout and
// every so often a real response is being dropped next to the noise.
func (t *stdioTransport) MalformedLines() int64 { return t.malformed.Load() }

// wait reaps the child exactly once. It blocks until the stderr and stdout
// copy goroutines finish, which is what makes the tail buffer settled by the
// time anyone reads it.
func (t *stdioTransport) wait() {
	t.waitOnce.Do(func() {
		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Wait()
		}
	})
}

// close offers the child a stdin-EOF grace period, then kills the whole
// process tree. Both steps are budgeted.
func (t *stdioTransport) close() {
	if !t.closed.CompareAndSwap(false, true) {
		return
	}
	if t.stdin != nil {
		_ = t.stdin.Close()
	}
	if t.cmd == nil || t.cmd.Process == nil {
		return
	}
	if waitFinishedWithinBudget(t.wait, gracefulCloseWait) {
		return
	}
	// A surviving grandchild can keep the inherited pipes open, so the kill
	// has to be the whole tree, not just the child.
	killProcessTree(t.cmd.Process.Pid)
	waitWithBudget(t.wait, closeWait)
}

func waitWithBudget(wait func(), budget time.Duration) {
	_ = waitFinishedWithinBudget(wait, budget)
}

func waitFinishedWithinBudget(wait func(), budget time.Duration) bool {
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// SubprocessError carries the child's stderr tail alongside the underlying
// failure. The tail is the only thing that ever explains a server that died
// during startup.
type SubprocessError struct {
	Cause  error
	Output string
}

func (e *SubprocessError) Error() string {
	if e.Output == "" {
		return e.Cause.Error()
	}
	return e.Cause.Error() + "\n" + e.Output
}

func (e *SubprocessError) Unwrap() error { return e.Cause }

// readFailure is a server whose stdout reached EOF before the answer did —
// the server crashed, or exited on its own.
type readFailure struct {
	server string
	method string
	cause  error
}

func (e *readFailure) Error() string {
	if errors.Is(e.cause, io.EOF) {
		return fmt.Sprintf("mcp server %q: exited while %s was in flight", e.server, e.method)
	}
	return fmt.Sprintf("mcp server %q: %s failed: %v", e.server, e.method, e.cause)
}

func (e *readFailure) Unwrap() error { return e.cause }

// ---------------------------------------------------------------------------
// Process environment
// ---------------------------------------------------------------------------

// mergeEnv layers a server's declared environment over the parent's.
//
// A server almost always needs the parent's PATH and HOME, so this is an
// overlay, not a replacement. Values are key-matched case-insensitively on
// Windows, where environment keys are case-insensitive.
func mergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overrides))
	applied := make(map[string]bool, len(overrides))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			if v, want := envLookup(overrides, k); want {
				out = append(out, k+"="+v)
				applied[envKey(k)] = true
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		if applied[envKey(k)] {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

func envLookup(m map[string]string, key string) (string, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	target := envKey(key)
	for k, v := range m {
		if envKey(k) == target {
			return v, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// stderr tail buffer
// ---------------------------------------------------------------------------

// tailBuffer keeps the last `limit` bytes written to it.
//
// A tail rather than the whole output: a server that logs forever must not
// grow this process without bound, and what is needed when a server dies is
// the end of its output, not the beginning.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
	cut   bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if b.limit > 0 && len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
		b.cut = true
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(string(b.buf))
	if b.cut {
		return "...(truncated)...\n" + s
	}
	return s
}
