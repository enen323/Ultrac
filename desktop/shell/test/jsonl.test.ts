// jsonl.test.ts — the framing rule, exercised directly. The property that
// matters: only \n ends a record, so a U+2028/U+2029 inside a JSON string
// (legal JSON, and a line terminator to a JavaScript reader) never splits
// one. See src/jsonl.ts and the Go twin internal/rpc/jsonl.go.

import { test } from "node:test";
import assert from "node:assert/strict";
import { JsonlSplitter } from "../src/jsonl.ts";

test("splits on \n only — U+2028 and U+2029 inside a record do not end it", () => {
  const splitter = new JsonlSplitter();
  const lines = splitter.push('{"a":"x\u2028y\u2029z"}\n{"b":1}\n');
  assert.deepEqual(lines, ['{"a":"x\u2028y\u2029z"}', '{"b":1}']);
  assert.equal(splitter.pendingBytes, 0);
});

test("strips at most one trailing \r; a lone \r mid-record is data", () => {
  const splitter = new JsonlSplitter();
  assert.deepEqual(splitter.push('{"a":1}\r\n'), ['{"a":1}']);
  assert.deepEqual(splitter.push('{"a":"x\ry"}\n'), ['{"a":"x\ry"}']);
});

test("holds a partial record until the rest of it arrives", () => {
  const splitter = new JsonlSplitter();
  assert.deepEqual(splitter.push('{"a"'), []);
  assert.equal(splitter.pendingBytes, 4);
  assert.deepEqual(splitter.push(":1}"), []);
  assert.deepEqual(splitter.push("\n"), ['{"a":1}']);
});

test("returns every record one chunk completes, in order", () => {
  const splitter = new JsonlSplitter();
  assert.deepEqual(splitter.push('{"a":1}\n{"b":2}\n{"c":3}\n'), ['{"a":1}', '{"b":2}', '{"c":3}']);
});

test("flush surfaces the trailing partial record at end of stream", () => {
  const splitter = new JsonlSplitter();
  splitter.push('{"a":1}\n{"b":2}');
  assert.equal(splitter.flush(), '{"b":2}');
  assert.equal(splitter.flush(), "");
});

test("empty lines are preserved for the caller to skip", () => {
  const splitter = new JsonlSplitter();
  assert.deepEqual(splitter.push('\n{"a":1}\n\n'), ["", '{"a":1}', ""]);
});

test("a chunk boundary inside a multi-byte character is the decoder's problem, not the splitter's", () => {
  // The splitter only ever sees decoded text; this asserts the rule that
  // makes that safe — a lone surrogate pair (or any non-\n content) stays
  // inside one record.
  const splitter = new JsonlSplitter();
  const lines = splitter.push('{"a":"\u{1F600}\u2028"}\n');
  assert.deepEqual(lines, ['{"a":"\u{1F600}\u2028"}']);
});
