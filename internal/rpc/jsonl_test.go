package rpc

// jsonl_test.go — the strict-framing rules, asserted at the byte level.
// The one that matters most: U+2028/U+2029 are valid inside JSON strings
// and must never split a record (pi's jsonl.ts exists for exactly this).

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLineReaderSplitsOnLFOnly(t *testing.T) {
	// A JSON string carrying U+2028 (LINE SEPARATOR) and U+2029 (PARAGRAPH
	// SEPARATOR): both are line terminators to a JS reader, neither may
	// split a JSONL record. The escapes keep the separators out of the
	// source file itself — a literal one is one editor normalization away
	// from silently becoming a space and hollowing the test.
	line := "{\"type\":\"prompt\",\"message\":\"a\u2028b\u2029c\"}"
	r := NewLineReader(strings.NewReader(line + "\n" + `{"type":"abort"}` + "\n"))

	got1, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got1 != line {
		t.Errorf("record 1 = %q, want %q — U+2028 split the line", got1, line)
	}
	got2, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got2 != `{"type":"abort"}` {
		t.Errorf("record 2 = %q", got2)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("final Next err = %v, want io.EOF", err)
	}
}

func TestLineReaderStripsTrailingCR(t *testing.T) {
	// A CRLF client (Windows pipe) must not leave a \r that breaks
	// JSON.parse.
	r := NewLineReader(strings.NewReader("{\"a\":1}\r\n{\"b\":2}\r\n"))
	for _, want := range []string{`{"a":1}`, `{"b":2}`} {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got != want {
			t.Errorf("record = %q, want %q", got, want)
		}
	}
}

func TestLineReaderLoneCRIsData(t *testing.T) {
	// Only the \r of a CRLF pair is framing; a lone \r inside a record
	// stays (jsonl.ts:26 strips at most one trailing \r).
	r := NewLineReader(strings.NewReader("a\rb\n"))
	got, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != "a\rb" {
		t.Errorf("record = %q, want %q", got, "a\rb")
	}
}

func TestLineReaderFlushesPartialLineAtEOF(t *testing.T) {
	// A client that forgot the final newline still gets its command
	// handled (pi's onEnd, jsonl.ts:44-49).
	r := NewLineReader(strings.NewReader(`{"type":"get_state"}`))
	got, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != `{"type":"get_state"}` {
		t.Errorf("record = %q", got)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("final Next err = %v, want io.EOF", err)
	}
}

func TestLineReaderEmptyStream(t *testing.T) {
	r := NewLineReader(strings.NewReader(""))
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next err = %v, want io.EOF", err)
	}
}

func TestLineReaderSplitAcrossReads(t *testing.T) {
	// One record delivered in fragments: the reader must buffer until the
	// terminator arrives. chunkReader hands out fixed-size pieces.
	full := `{"type":"prompt","message":"` + strings.Repeat("x", 5000) + `"}`
	r := NewLineReader(&chunkReader{data: []byte(full + "\n"), size: 7})
	got, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != full {
		t.Errorf("record length = %d, want %d", len(got), len(full))
	}
}

// chunkReader is an io.Reader that returns at most size bytes per Read,
// for exercising the buffer growth path.
type chunkReader struct {
	data []byte
	size int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := c.size
	if n > len(p) {
		n = len(p)
	}
	if n > len(c.data) {
		n = len(c.data)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}
