// jsonl.go — strict JSONL framing for the RPC streams, a port of pi's
// packages/coding-agent/src/modes/rpc/jsonl.ts (58 lines).
//
// # Why not bufio.Scanner / readline
//
// JSON strings may legally contain U+2028 and U+2029, which are line
// terminators to a JavaScript reader and to Node's readline. Splitting on
// anything but \n would cut a record in half; splitting on \n only, and
// stripping at most one trailing \r, is the whole rule (pi's file comment
// says the same). bufio.Scanner's ScanLines matches that rule but caps a
// record at its buffer size — an image-carrying prompt would fail with
// "token too long" — so this is a manual splitter with a growable buffer
// and no record-size ceiling.
//
// # Direction split
//
// Output framing lives in transport.PiJSONWriter (one JSON object per
// line, U+2028/29 escaped) because responses and events share that writer
// with the print/json protocol. This file owns the input direction only.
package rpc

import (
	"io"
	"strings"
)

// LineReader splits a byte stream into JSONL records.
//
// It is not safe for concurrent use: one reader per stream, read from one
// goroutine (the server's command loop).
type LineReader struct {
	r   io.Reader
	buf []byte
	eof bool
}

// NewLineReader attaches a reader to r.
func NewLineReader(r io.Reader) *LineReader {
	return &LineReader{r: r, buf: make([]byte, 0, 64*1024)}
}

// Next returns the next record without its terminator.
//
// A trailing partial record at end of stream is returned before io.EOF —
// pi's onEnd does the same (jsonl.ts:44-49) so a client that forgot the
// final newline still gets its command handled. io.EOF means the stream is
// done; any other error is a read failure the caller surfaces.
func (lr *LineReader) Next() (string, error) {
	for {
		if i := indexByte(lr.buf, '\n'); i >= 0 {
			line := string(lr.buf[:i])
			lr.buf = lr.buf[i+1:]
			return trimCR(line), nil
		}
		if lr.eof {
			if len(lr.buf) == 0 {
				return "", io.EOF
			}
			line := string(lr.buf)
			lr.buf = lr.buf[:0]
			return trimCR(line), nil
		}
		if err := lr.fill(); err != nil {
			if err == io.EOF {
				lr.eof = true
				continue
			}
			return "", err
		}
	}
}

// fill reads more bytes into the buffer. The buffer keeps the unconsumed
// prefix and grows as needed; a record larger than the current capacity
// costs one realloc per doubling, not per read.
func (lr *LineReader) fill() error {
	if cap(lr.buf)-len(lr.buf) < 32*1024 {
		grown := make([]byte, len(lr.buf), max(2*cap(lr.buf), 64*1024))
		copy(grown, lr.buf)
		lr.buf = grown
	}
	n, err := lr.r.Read(lr.buf[len(lr.buf):cap(lr.buf)])
	lr.buf = lr.buf[:len(lr.buf)+n]
	return err
}

// trimCR drops one trailing carriage return — the \r of a CRLF pair, and
// nothing else (jsonl.ts:26). A lone \r mid-record is data, not framing.
func trimCR(line string) string {
	return strings.TrimSuffix(line, "\r")
}

// indexByte is bytes.IndexByte on the buffer's used length.
func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
