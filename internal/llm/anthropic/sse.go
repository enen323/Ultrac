package anthropic

import (
	"bufio"
	"bytes"
	"strings"
)

// sseData extracts the payload of one SSE line.
//
// Server-sent events are `field: value` lines, with a bare `field` meaning an
// empty value. The API sends `event: <type>` followed by `data: <json>`, and
// the event name is redundant with the payload's own "type" field, so only
// data is read.
//
// A multi-line data field is joined with newlines per the SSE spec. The API
// does not currently send one, but a client that concatenates without the
// newline would corrupt a payload if that changed.
func sseData(line []byte) ([]byte, bool) {
	// A leading space after the colon is part of the framing, not the value.
	trimmed := bytes.TrimLeft(line, " \t")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	value := bytes.TrimPrefix(trimmed, []byte("data:"))
	value = bytes.TrimPrefix(value, []byte(" "))

	// The API's end-of-stream sentinel. Passing it to the JSON decoder would
	// produce a parse error on every single request.
	if len(bytes.TrimSpace(value)) == 0 {
		return nil, false
	}
	return value, true
}

// sseScanner is a convenience for tests and any future non-scanner reader.
func newSSEScanner(r *bufio.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return sc
}

// eventName extracts the `event:` field of an SSE block, if present. Exposed
// for diagnostics: when a stream misbehaves, the event name is often the only
// clue about what the server actually sent.
func eventName(line string) string {
	if !strings.HasPrefix(line, "event:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "event:"))
}
