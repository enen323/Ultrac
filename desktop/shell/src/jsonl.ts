// jsonl.ts — strict JSONL record splitting, the client half of the framing
// rule the kernel already implements on its input (internal/rpc/jsonl.go)
// and its output (transport.PiJSONWriter): split on \n only, and strip at
// most one trailing \r.
//
// # Why not node:readline
//
// JSON strings may legally contain U+2028 and U+2029 — which are line
// terminators to a JavaScript reader. Splitting on anything but \n cuts a
// record in half; a session name or a tool result containing one would
// corrupt the stream. This is the same rule pi's jsonl.ts states and the
// same reason the Go side hand-rolls its splitter.
//
// The splitter is stateful and single-directional: one per stream, fed
// from one 'data' handler. flush() surfaces a trailing partial record at
// end of stream, so a kernel that died mid-line still gets read.

/** Splits a byte stream (already decoded to text) into JSONL records. */
export class JsonlSplitter {
  #buffer = "";

  /** Feed one decoded chunk; returns every record it completed, in order. */
  push(chunk: string): string[] {
    this.#buffer += chunk;
    const lines: string[] = [];
    let start = 0;
    for (;;) {
      const nl = this.#buffer.indexOf("\n", start);
      if (nl < 0) break;
      lines.push(trimCR(this.#buffer.slice(start, nl)));
      start = nl + 1;
    }
    this.#buffer = this.#buffer.slice(start);
    return lines;
  }

  /** The trailing partial record, or "" when the stream ended on a boundary. */
  flush(): string {
    const rest = trimCR(this.#buffer);
    this.#buffer = "";
    return rest;
  }

  /** Bytes held for an incomplete record — a wedged kernel must not grow us. */
  get pendingBytes(): number {
    return this.#buffer.length;
  }
}

/** Drops one trailing carriage return — the \r of a CRLF pair, nothing else. */
function trimCR(line: string): string {
  return line.endsWith("\r") ? line.slice(0, -1) : line;
}
