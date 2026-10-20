// bridge.test.ts — the page's door, driven by a stub window. No DOM, no
// kernel: the bridge is injected, which is the whole reason port/bridge.ts
// takes a window-like object instead of reading the global.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  BridgeError,
  bridgeOf,
  callData,
  callVoid,
  inShell,
  type CallResult,
  type UltracBridge,
} from "../src/port/bridge.ts";
import type { RpcEvent, RpcSessionState } from "../src/state/wire.ts";

function stubBridge(overrides: Partial<UltracBridge> = {}): { bridge: UltracBridge; win: { ultrac?: UltracBridge } } {
  const bridge: UltracBridge = {
    shell: "electron",
    platform: "win32",
    call: async (command) => ({ ok: true, command: command.type, success: true, data: null }),
    onEvent: () => () => {},
    onSessionLoaded: () => () => {},
    openExternal: async () => {},
    ...overrides,
  };
  return { bridge, win: { ultrac: bridge } };
}

// -- presence ------------------------------------------------------------------

test("inShell / bridgeOf: present inside the shell, absent in a plain browser", () => {
  const { win } = stubBridge();
  assert.equal(inShell(win), true);
  assert.equal(bridgeOf(win).shell, "electron");
  assert.equal(inShell({}), false);
  assert.equal(inShell(undefined), false);
  assert.throws(() => bridgeOf({}), /桌面壳/);
});

// -- call ----------------------------------------------------------------------

test("callData returns the response data on success", async () => {
  const { bridge } = stubBridge({
    call: async () => ({ ok: true, command: "get_state", success: true, data: { sessionId: "s1", isStreaming: false } }),
  });
  const data = await callData<{ sessionId: string }>(bridge, { type: "get_state" });
  assert.equal(data.sessionId, "s1");
});

test("a kernel error response throws with the kernel's message", async () => {
  const { bridge } = stubBridge({
    call: async () => ({ ok: true, command: "prompt", success: false, error: "Agent is already processing." }),
  });
  await assert.rejects(
    () => callData(bridge, { type: "prompt", message: "hi" }),
    (err: unknown) => err instanceof BridgeError && err.message === "Agent is already processing.",
  );
});

test("a bridge failure throws with its code", async () => {
  const failure: CallResult = { ok: false, code: "exited", message: "内核已退出" };
  const { bridge } = stubBridge({ call: async () => failure });
  await assert.rejects(
    () => callData(bridge, { type: "get_state" }),
    (err: unknown) => err instanceof BridgeError && err.code === "exited" && err.message === "内核已退出",
  );
});

test("callVoid ignores the (absent) data", async () => {
  const { bridge } = stubBridge({ call: async () => ({ ok: true, command: "abort", success: true }) });
  await callVoid(bridge, { type: "abort" });
});

// -- events ----------------------------------------------------------------------

test("onEvent delivers in order and unsubscribes", () => {
  const listeners: Array<(event: RpcEvent) => void> = [];
  const { bridge } = stubBridge({ onEvent: (listener) => { listeners.push(listener); return () => { listeners.splice(listeners.indexOf(listener), 1); }; } });
  const seen: string[] = [];
  const off = bridge.onEvent((event) => seen.push(event.type));
  listeners.forEach((listener) => listener({ type: "message_start" }));
  listeners.forEach((listener) => listener({ type: "message_end" }));
  off();
  listeners.forEach((listener) => listener({ type: "agent_end" }));
  assert.deepEqual(seen, ["message_start", "message_end"]);
});

test("onSessionLoaded hands over the state once", () => {
  let fire: ((state: RpcSessionState) => void) | null = null;
  const { bridge } = stubBridge({ onSessionLoaded: (listener) => { fire = listener; return () => {}; } });
  const seen: RpcSessionState[] = [];
  bridge.onSessionLoaded((state) => seen.push(state));
  const state = { sessionId: "s1", thinkingLevel: "off", isStreaming: false, isCompacting: false, steeringMode: "all", followUpMode: "all", autoCompactionEnabled: false, messageCount: 0, pendingMessageCount: 0 };
  fire!(state);
  assert.deepEqual(seen, [state]);
});

// -- openExternal -----------------------------------------------------------------

test("openExternal passes the url through untouched", async () => {
  const opened: string[] = [];
  const { bridge } = stubBridge({ openExternal: async (url) => { opened.push(url); } });
  await bridge.openExternal("https://example.com");
  assert.deepEqual(opened, ["https://example.com"]);
});
