#!/usr/bin/env node
/**
 * fake-kernel.mjs — a scriptable stand-in for `ultrac --mode rpc`, for the
 * shell's unit tests. It speaks the same JSONL contract on the same pipes,
 * so the client under test cannot tell the difference. What the scenarios
 * buy is control over what the real kernel makes hard to observe: silence,
 * early death, out-of-order answers, byte-level framing.
 *
 * Scenarios (argv[2]):
 *   respond                 answer every command; get_state returns a canned state
 *   respond-reverse         answer the second command before the first
 *   reject-probe            answer get_state with success:false
 *   silent                  read everything, answer nothing
 *   slow-respond            answer after 200 ms
 *   malformed-then-respond  write one non-JSON line, then answer normally
 *   exit-early              die immediately, before reading anything
 *   hold-open               answer the probe, then live until stdin EOF (exit 0)
 *   framing                 answer the probe in crafted chunks (U+2028, CRLF, splits)
 *
 * The fake keeps the protocol honest: strict \n-only framing on input, ids
 * echoed on output, nothing written until asked.
 */

import process from "node:process";

const scenario = process.argv[2];

// The lease, before anything else: a kernel that dies without reading has
// still honoured the contract (exit, don't hang).
if (scenario === "exit-early") process.exit(3);

const CANNED_STATE = {
  model: {
    id: "fake-model",
    name: "Fake Model",
    api: "anthropic-messages",
    provider: "fake",
    baseUrl: "https://fake.invalid/anthropic",
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    reasoning: false,
    contextWindow: 1000,
    maxTokens: 100,
  },
  thinkingLevel: "off",
  isStreaming: false,
  isCompacting: false,
  steeringMode: "one-at-a-time",
  followUpMode: "one-at-a-time",
  sessionFile: "C:\\fake\\sessions\\fake-session-0001.jsonl",
  sessionId: "fake-session-0001",
  autoCompactionEnabled: false,
  messageCount: 1,
  pendingMessageCount: 0,
};

function writeLine(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

// The implemented set, so an unknown command fails the way the real
// kernel's dispatch does (internal/rpc/server.go's default branch) instead
// of being echoed back as a success.
const IMPLEMENTED = new Set([
  "prompt", "steer", "follow_up", "abort", "clear_queue", "new_session",
  "get_state", "set_model", "get_available_models", "set_thinking_level",
  "get_available_thinking_levels", "set_steering_mode", "set_follow_up_mode",
  "get_messages", "get_last_assistant_text", "set_session_name", "get_entries",
  "get_commands", "list_sessions", "resume_session",
]);

function respondTo(cmd) {
  if (cmd.type === "get_state") {
    writeLine({ id: cmd.id, type: "response", command: "get_state", success: true, data: CANNED_STATE });
    return;
  }
  if (IMPLEMENTED.has(cmd.type)) {
    writeLine({ id: cmd.id, type: "response", command: cmd.type, success: true, data: { echo: cmd.type } });
    return;
  }
  writeLine({ id: cmd.id, type: "response", command: cmd.type, success: false, error: `Unknown command: ${cmd.type}` });
}

const reverseQueue = [];
let answeredHoldOpen = false;
let framingDone = false;

function handleLine(line) {
  let cmd;
  try {
    cmd = JSON.parse(line);
  } catch {
    // The kernel answers a parse failure and keeps serving; so does the fake.
    writeLine({ type: "response", command: "parse", success: false, error: "Failed to parse command" });
    return;
  }
  switch (scenario) {
    case "respond":
      respondTo(cmd);
      break;
    case "respond-reverse":
      reverseQueue.push(cmd);
      if (reverseQueue.length === 2) {
        const [first, second] = reverseQueue;
        reverseQueue.length = 0;
        respondTo(second);
        setTimeout(() => respondTo(first), 5);
      }
      break;
    case "reject-probe":
      writeLine({ id: cmd.id, type: "response", command: cmd.type, success: false, error: "the kernel is not ready" });
      break;
    case "silent":
      break;
    case "slow-respond":
      setTimeout(() => respondTo(cmd), 200);
      break;
    case "malformed-then-respond":
      process.stdout.write("this is not json\n");
      respondTo(cmd);
      break;
    case "hold-open":
      if (!answeredHoldOpen) {
        answeredHoldOpen = true;
        respondTo(cmd);
      }
      break;
    case "framing":
      if (framingDone) break;
      framingDone = true;
      writeFramedAnswer(cmd);
      break;
    default:
      respondTo(cmd);
      break;
  }
}

// The answer is three records' worth of hostile bytes: an event whose JSON
// string contains a literal U+2028 and U+2029 (line terminators to a
// JavaScript reader, data to this one), a CRLF ending, a response split
// across two chunks — and the first chunk cuts the U+2028 character's
// three UTF-8 bytes in half, so even the decoder is exercised.
function writeFramedAnswer(cmd) {
  const eventLine = JSON.stringify({ type: "probe_event", note: "x\u2028y\u2029z" });
  const responseLine = JSON.stringify({
    id: cmd.id,
    type: "response",
    command: "get_state",
    success: true,
    data: CANNED_STATE,
  });
  const payload = Buffer.from(`${eventLine}\r\n${responseLine}\n`, "utf8");
  const eventBytes = Buffer.byteLength(eventLine, "utf8");
  const cut1 = Buffer.byteLength(eventLine.slice(0, eventLine.indexOf("\u2028")), "utf8") + 1;
  const cut2 = eventBytes + 2 + 30; // 2 for the CRLF, 30 lands mid response record
  const writeBytes = (buf, delayMs) => setTimeout(() => process.stdout.write(buf), delayMs);
  writeBytes(payload.subarray(0, cut1), 5);
  writeBytes(payload.subarray(cut1, cut2), 15);
  writeBytes(payload.subarray(cut2), 25);
}

// One strict \n-only line reader — the same rule the kernel implements.
let buffer = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  buffer += chunk;
  for (;;) {
    const nl = buffer.indexOf("\n");
    if (nl < 0) break;
    const line = buffer.slice(0, nl).replace(/\r$/, "");
    buffer = buffer.slice(nl + 1);
    if (line !== "") handleLine(line);
  }
});
// stdin EOF is the lease: the parent is gone, so are we (exit 0, as the
// real kernel does).
process.stdin.on("end", () => process.exit(0));
