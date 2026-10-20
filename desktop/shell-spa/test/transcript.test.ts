// transcript.test.ts — the reducer, event by event. The wire shapes here are
// transcribed from the kernel (internal/transport/pi_json.go for the event
// envelopes, internal/ai for the assistant events); if a test reads like the
// Go source, that is the point — this file is the page's half of the
// contract, and it fails when the contract drifts.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  applyEvent,
  contextTokens,
  emptyTranscript,
  fromMessages,
} from "../src/state/transcript.ts";
import type { RpcEvent, WireMessage } from "../src/state/wire.ts";

// -- builders ---------------------------------------------------------------

let stamp = 1_700_000_000_000;

function nextStamp(): number {
  stamp += 1000;
  return stamp;
}

function message(role: WireMessage["role"], content: WireMessage["content"], extra: Partial<WireMessage> = {}): WireMessage {
  return { role, content, timestamp: nextStamp(), ...extra };
}

function start(role: WireMessage["role"], content: WireMessage["content"], extra: Partial<WireMessage> = {}): RpcEvent {
  return { type: "message_start", message: message(role, content, extra) };
}

function end(role: WireMessage["role"], content: WireMessage["content"], extra: Partial<WireMessage> = {}): RpcEvent {
  return { type: "message_end", message: message(role, content, extra) };
}

function update(assistantMessageEvent: Record<string, unknown>): RpcEvent {
  return { type: "message_update", usage: null, assistantMessageEvent };
}

const usage = (input: number) => ({
  input,
  output: 10,
  cacheRead: input,
  cacheWrite: 0,
  totalTokens: input * 2 + 10,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
});

// -- messages ---------------------------------------------------------------

test("a user message from message_start is one row, not streaming", () => {
  const state = applyEvent(emptyTranscript(), start("user", "你好"));
  assert.equal(state.messages.length, 1);
  assert.equal(state.messages[0]!.role, "user");
  assert.equal(state.messages[0]!.streaming, false);
  assert.deepEqual(state.messages[0]!.blocks, [{ kind: "text", text: "你好" }]);
  assert.equal(state.streamingKey, null);
});

test("a user message's start and end are one row, not two", () => {
  // The real kernel always emits message_start then message_end for the
  // user message, and the user row is never the streaming row (only
  // assistant messages stream). The end must close the row the start
  // opened — the full-chain Electron check caught this doubling once.
  // Both events carry the same message object, hence one timestamp.
  const user = message("user", "你好");
  let state = applyEvent(emptyTranscript(), { type: "message_start", message: user });
  state = applyEvent(state, { type: "message_end", message: user });
  assert.equal(state.messages.length, 1);
  assert.equal(state.messages[0]!.streaming, false);
  assert.deepEqual(state.messages[0]!.blocks[0], { kind: "text", text: "你好" });
});

test("system messages never become rows", () => {
  let state = applyEvent(emptyTranscript(), start("system", "you are ultrac"));
  state = applyEvent(state, end("system", "you are ultrac"));
  assert.equal(state.messages.length, 0);
});

test("message_start on an assistant message opens the streaming row", () => {
  const state = applyEvent(emptyTranscript(), start("assistant", []));
  assert.equal(state.messages.length, 1);
  assert.equal(state.messages[0]!.streaming, true);
  assert.equal(state.streamingKey, state.messages[0]!.key);
});

// -- the typewriter -----------------------------------------------------------

test("text deltas accumulate; text_end replaces with the authoritative content", () => {
  let state = applyEvent(emptyTranscript(), start("assistant", []));
  state = applyEvent(state, update({ type: "text_start", contentIndex: 0 }));
  state = applyEvent(state, update({ type: "text_delta", contentIndex: 0, delta: "你好" }));
  state = applyEvent(state, update({ type: "text_delta", contentIndex: 0, delta: "，世界" }));
  assert.deepEqual(state.messages[0]!.blocks[0], { kind: "text", text: "你好，世界" });

  state = applyEvent(state, update({ type: "text_end", contentIndex: 0, content: "你好，世界！" }));
  assert.deepEqual(state.messages[0]!.blocks[0], { kind: "text", text: "你好，世界！" });

  state = applyEvent(state, end("assistant", [{ type: "text", text: "你好，世界！" }], { stopReason: "stop" }));
  assert.equal(state.messages[0]!.streaming, false);
  assert.equal(state.streamingKey, null);
  assert.equal(state.messages[0]!.stopReason, "stop");
});

test("a provider that seeds at *_start and sends no deltas still renders (text_end carries the content)", () => {
  let state = applyEvent(emptyTranscript(), start("assistant", []));
  state = applyEvent(state, update({ type: "thinking_start", contentIndex: 0 }));
  state = applyEvent(state, update({ type: "thinking_end", contentIndex: 0, content: "先想一遍" }));
  state = applyEvent(state, update({ type: "text_start", contentIndex: 1 }));
  state = applyEvent(state, update({ type: "text_end", contentIndex: 1, content: "再答" }));
  assert.deepEqual(state.messages[0]!.blocks, [
    { kind: "thinking", text: "先想一遍" },
    { kind: "text", text: "再答" },
  ]);
});

test("thinking deltas accumulate into their own block", () => {
  let state = applyEvent(emptyTranscript(), start("assistant", []));
  state = applyEvent(state, update({ type: "thinking_delta", contentIndex: 0, delta: "让" }));
  state = applyEvent(state, update({ type: "thinking_delta", contentIndex: 0, delta: "我想" }));
  assert.deepEqual(state.messages[0]!.blocks[0], { kind: "thinking", text: "让我想" });
});

// -- tool calls ---------------------------------------------------------------

test("a tool call streams raw argument fragments until toolcall_end parses them", () => {
  let state = applyEvent(emptyTranscript(), start("assistant", []));
  state = applyEvent(state, update({ type: "toolcall_start", contentIndex: 0 }));
  assert.deepEqual(state.messages[0]!.blocks[0], { kind: "toolCall", id: "", name: "", args: {}, rawArgs: "" });

  state = applyEvent(state, update({ type: "toolcall_delta", contentIndex: 0, delta: '{"path":"a.go"' }));
  state = applyEvent(state, update({ type: "toolcall_delta", contentIndex: 0, delta: ',"line":12}' }));
  assert.deepEqual(state.messages[0]!.blocks[0], {
    kind: "toolCall",
    id: "",
    name: "",
    args: {},
    rawArgs: '{"path":"a.go","line":12}',
  });

  state = applyEvent(
    state,
    update({ type: "toolcall_end", contentIndex: 0, toolCall: { id: "call_1", name: "read", arguments: { path: "a.go", line: 12 } } }),
  );
  assert.deepEqual(state.messages[0]!.blocks[0], {
    kind: "toolCall",
    id: "call_1",
    name: "read",
    args: { path: "a.go", line: 12 },
    rawArgs: "",
  });
});

test("tool_execution_* drive the card status, and the toolResult message is its output", () => {
  let state = applyEvent(
    emptyTranscript(),
    end("assistant", [{ type: "toolCall", id: "call_1", name: "bash", arguments: { command: "ls" } }]),
  );
  state = applyEvent(state, { type: "tool_execution_start", toolCallId: "call_1", toolName: "bash", args: { command: "ls" } });
  assert.deepEqual(state.toolRuns["call_1"], { id: "call_1", name: "bash", status: "running", text: "", isError: false, hasResult: false });

  state = applyEvent(state, {
    type: "tool_execution_update",
    toolCallId: "call_1",
    toolName: "bash",
    args: { command: "ls" },
    partialResult: { content: [{ type: "text", text: "a.go\n" }] },
  });
  assert.equal(state.toolRuns["call_1"]!.text, "a.go\n");

  state = applyEvent(state, {
    type: "tool_execution_end",
    toolCallId: "call_1",
    toolName: "bash",
    result: { content: [{ type: "text", text: "a.go" }] },
    isError: false,
    durationMs: 42,
  });
  assert.equal(state.toolRuns["call_1"]!.status, "done");
  assert.equal(state.toolRuns["call_1"]!.durationMs, 42);
  assert.equal(state.toolRuns["call_1"]!.hasResult, false);

  // The toolResult message carries the authoritative output.
  state = applyEvent(state, start("toolResult", [{ type: "text", text: "a.go" }], { toolCallId: "call_1", toolName: "bash" }));
  state = applyEvent(state, end("toolResult", [{ type: "text", text: "a.go" }], { toolCallId: "call_1", toolName: "bash" }));
  assert.equal(state.messages.length, 1, "the tool result is not a row of its own");
  assert.equal(state.toolRuns["call_1"]!.text, "a.go");
  assert.equal(state.toolRuns["call_1"]!.hasResult, true);
});

test("a failed tool is marked isError on its card", () => {
  let state = applyEvent(emptyTranscript(), { type: "tool_execution_start", toolCallId: "c9", toolName: "bash", args: {} });
  state = applyEvent(state, {
    type: "tool_execution_end",
    toolCallId: "c9",
    toolName: "bash",
    result: { content: [{ type: "text", text: "command not found" }] },
    isError: true,
  });
  assert.equal(state.toolRuns["c9"]!.isError, true);
  assert.equal(state.toolRuns["c9"]!.status, "done");
});

// -- errors -------------------------------------------------------------------

test("a failed run lands as an assistant row carrying errorMessage", () => {
  let state = applyEvent(
    emptyTranscript(),
    start("assistant", [{ type: "text", text: "" }], { stopReason: "error", errorMessage: "连接远端失败" }),
  );
  state = applyEvent(state, end("assistant", [{ type: "text", text: "" }], { stopReason: "error", errorMessage: "连接远端失败" }));
  assert.equal(state.messages.length, 1);
  assert.equal(state.messages[0]!.errorMessage, "连接远端失败");
  assert.equal(state.runError, "连接远端失败");
  assert.equal(state.messages[0]!.streaming, false);
});

test("a stream-level error event records the run error without a row", () => {
  let state = applyEvent(emptyTranscript(), start("assistant", []));
  state = applyEvent(state, update({ type: "error", reason: "error", error: { role: "assistant", content: [], timestamp: 0, errorMessage: "过载了" } }));
  assert.equal(state.runError, "过载了");
  assert.equal(state.messages.length, 1);
});

// -- history -------------------------------------------------------------------

test("fromMessages: system rows drop, user strings normalize, tool results route into cards", () => {
  const state = fromMessages([
    message("system", "preamble"),
    message("user", "第一条"),
    message("assistant", [{ type: "text", text: "回复" }, { type: "toolCall", id: "c1", name: "bash", arguments: { command: "ls" } }], {
      usage: usage(500),
    }),
    message("toolResult", [{ type: "text", text: "a.go" }], { toolCallId: "c1", toolName: "bash" }),
    message("user", "第二条"),
  ]);
  assert.deepEqual(
    state.messages.map((row) => row.role),
    ["user", "assistant", "user"],
  );
  assert.deepEqual(state.messages[0]!.blocks[0], { kind: "text", text: "第一条" });
  assert.equal(state.toolRuns["c1"]!.text, "a.go");
  assert.equal(state.toolRuns["c1"]!.hasResult, true);
  assert.equal(contextTokens(state), 1000, "input + cacheRead of the last assistant message");
});

// -- replay: buffered events meeting the snapshot -------------------------------

test("replay dedups completed messages and keeps the in-flight one", () => {
  // The snapshot the kernel reports while a run is mid-flight: the user
  // message is complete and persisted; the assistant message is not (the
  // agent only appends at message_end), so it is absent.
  const user = message("user", "你好");
  const snapshot = fromMessages([user]);

  // The shell buffered the run's early events before the page loaded.
  const buffered: RpcEvent[] = [
    { type: "message_start", message: user },
    { type: "message_end", message: user },
    start("assistant", []), // same message the deltas below belong to
    update({ type: "text_delta", contentIndex: 0, delta: "在" }),
    update({ type: "text_delta", contentIndex: 0, delta: "的" }),
  ];
  let state = snapshot;
  for (const event of buffered) state = applyEvent(state, event, true);

  assert.equal(state.messages.length, 2, "the user message is not duplicated");
  assert.equal(state.messages[0]!.role, "user");
  assert.equal(state.messages[0]!.streaming, false);
  assert.equal(state.messages[1]!.role, "assistant");
  assert.equal(state.messages[1]!.streaming, true, "the in-flight message survives as the streaming row");
  assert.deepEqual(state.messages[1]!.blocks[0], { kind: "text", text: "在的" });

  // Its end finalizes the row the deltas built, rather than appending.
  state = applyEvent(state, end("assistant", [{ type: "text", text: "在的" }]), true);
  assert.equal(state.messages.length, 2);
  assert.equal(state.messages[1]!.streaming, false);
  assert.deepEqual(state.messages[1]!.blocks[0], { kind: "text", text: "在的" });
});

test("replay skips a tool call whose result is already in the snapshot", () => {
  const state = fromMessages([
    message("assistant", [{ type: "toolCall", id: "c1", name: "bash", arguments: { command: "ls" } }]),
    message("toolResult", [{ type: "text", text: "a.go" }], { toolCallId: "c1", toolName: "bash" }),
  ]);
  const after = applyEvent(state, { type: "tool_execution_start", toolCallId: "c1", toolName: "bash", args: {} }, true);
  assert.equal(after.toolRuns["c1"]!.status, "done");
  assert.equal(after.toolRuns["c1"]!.hasResult, true);
});

test("replay keeps a tool call that is still running", () => {
  const state = fromMessages([message("assistant", [{ type: "toolCall", id: "c2", name: "bash", arguments: {} }])]);
  const after = applyEvent(state, { type: "tool_execution_start", toolCallId: "c2", toolName: "bash", args: {} }, true);
  assert.equal(after.toolRuns["c2"]!.status, "running");
  assert.equal(after.toolRuns["c2"]!.hasResult, false);
});

test("session-level events leave the transcript untouched", () => {
  const state = applyEvent(emptyTranscript(), start("user", "hi"));
  for (const type of ["agent_start", "turn_start", "turn_end", "agent_end", "agent_settled"]) {
    assert.equal(applyEvent(state, { type }), state, type);
  }
});
