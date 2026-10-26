// Command fakeserver is a deliberately small MCP server used by
// internal/mcp's tests (docs/开发计划/新功能计划/mcp-plan.md P2).
//
// It exists because the four behaviours a transport has to survive are all
// things a well-behaved server never does: a tool that never answers, a
// server that dies mid-call, a server that dies at startup, and a server that
// writes non-JSON noise to stdout. Every one of them is a real thing real
// servers do.
//
// It lives under testdata/ so `go build ./...` never compiles it, and the
// tests reach it with `go run`.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// usage:
//
//	fakeserver                  a server that works
//	fakeserver crash            dies during startup, after writing to stderr
//	fakeserver chatty           prefixes every response with a non-JSON line
//	fakeserver slow             answers nothing for 5 minutes (startup timeouts)
//	fakeserver count <path>     works, and appends a line to <path> on startup,
//	                            which is how a test counts spawns

type request struct {
	JSONRPC string `json:"jsonrpc"`
	// ID is a pointer because a notification has no id at all, and a fake
	// server that answered one would be teaching the transport to expect
	// traffic a real never sends.
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "crash":
		// A server that prints a traceback and dies before the handshake
		// can even start. The stderr text is what makes the failure
		// diagnosable instead of a bare "exit status 1".
		fmt.Fprintln(os.Stderr, "fake: simulated startup failure")
		fmt.Fprintln(os.Stderr, "fake:   at Module._compile (node:internal/modules:1105:14)")
		os.Exit(3)
	case "slow":
		// Never answers. The startup timeout is what saves the caller.
		time.Sleep(5 * time.Minute)
		return
	case "count":
		// Records the spawn. Two concurrent first calls to one server must
		// produce one line, not two.
		if len(os.Args) > 2 {
			if f, err := os.OpenFile(os.Args[2], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintln(f, "start")
				f.Close()
			}
		}
	}

	chatty := mode == "chatty"
	out := bufio.NewWriter(os.Stdout)
	var mu sync.Mutex

	// Each request is handled on its own goroutine, so responses may come
	// back out of order — which is exactly what the transport's id
	// demultiplexing has to survive.
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		go handle(&mu, out, chatty, req)
	}
}

func handle(mu *sync.Mutex, out *bufio.Writer, chatty bool, req request) {
	if req.ID == nil {
		return // a notification: no reply, ever
	}
	result, fail := dispatch(req)

	mu.Lock()
	defer mu.Unlock()
	if chatty {
		// A server that logs to stdout before every response. The
		// transport must skip the noise, not fail the connection.
		fmt.Fprint(out, "this is not json\n")
	}
	switch {
	case fail != nil:
		write(out, map[string]any{
			"jsonrpc": "2.0",
			"id":      *req.ID,
			"error":   map[string]any{"code": -32603, "message": fail.Error()},
		})
	default:
		write(out, map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
	out.Flush()
}

// dispatch produces the result for one request, or an error to report.
// A nil id (a notification) is answered by doing nothing.
func dispatch(req request) (result any, fail error) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "fakeserver", "version": "1.0.0"},
			"instructions":    "a fake server that exercises the transport",
		}, nil

	case "tools/list":
		return map[string]any{"tools": []any{
			tool("echo", "echoes the text it is given", "text"),
			tool("sleep", "sleeps for the given ms, then answers", "ms"),
			tool("boom", "writes to stderr and exits", ""),
			tool("image", "returns a tiny PNG", ""),
			tool("structured", "returns only structuredContent", ""),
			tool("resource", "returns a text resource", ""),
			tool("fail", "reports a tool error", ""),
		}}, nil

	case "tools/call":
		return callTool(req.Params)

	default:
		return nil, fmt.Errorf("method not found: %s", req.Method)
	}
}

func tool(name, description, required string) map[string]any {
	props := map[string]any{}
	var requiredList []string
	switch required {
	case "text":
		props["text"] = map[string]any{"type": "string"}
		requiredList = []string{"text"}
	case "ms":
		props["ms"] = map[string]any{"type": "integer"}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(requiredList) > 0 {
		schema["required"] = requiredList
	}
	return map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": schema,
	}
}

func callTool(params json.RawMessage) (any, error) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}

	switch p.Name {
	case "echo":
		text, _ := p.Arguments["text"].(string)
		return result(map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "echo: " + text},
		}}), nil

	case "sleep":
		ms := 0
		switch v := p.Arguments["ms"].(type) {
		case float64:
			ms = int(v)
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return result(map[string]any{"content": []any{
			map[string]any{"type": "text", "text": fmt.Sprintf("slept %dms", ms)},
		}}), nil

	case "boom":
		// Dies mid-call. The response never arrives, and the stderr text is
		// the only thing that explains why. The return below is unreachable
		// and exists only because the compiler does not know os.Exit does
		// not come back.
		fmt.Fprintln(os.Stderr, "fake: boom requested, exiting")
		os.Exit(7)
		return nil, nil

	case "image":
		return result(map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "before"},
			map[string]any{
				"type":     "image",
				"data":     fakePNG,
				"mimeType": "image/png",
			},
			map[string]any{"type": "text", "text": "after"},
		}}), nil

	case "structured":
		return result(map[string]any{
			"content":           []any{},
			"structuredContent": map[string]any{"answer": 42},
		}), nil

	case "resource":
		return result(map[string]any{"content": []any{
			map[string]any{
				"type": "resource",
				"resource": map[string]any{
					"uri":      "file:///tmp/notes.txt",
					"mimeType": "text/plain",
					"text":     "the contents",
				},
			},
		}}), nil

	case "fail":
		return result(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "the tool failed on purpose"}},
			"isError": true,
		}), nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", p.Name)
	}
}

// result shapes a successful tools/call result.
func result(v map[string]any) map[string]any {
	if _, ok := v["content"]; !ok {
		v["content"] = []any{}
	}
	if _, ok := v["isError"]; !ok {
		v["isError"] = false
	}
	return v
}

func write(out *bufio.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	out.Write(b)
	out.WriteByte('\n')
}

// fakePNG is a 1x1 transparent PNG, the smallest image an MCP server can
// return. It is a real image so a provider receiving the block would accept
// it.
const fakePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
