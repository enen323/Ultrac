// hostclient.test.ts — the main process's door to the kernel, against a
// stub: no child process, no Electron. What is pinned here is the shaping:
// a refusal and an unreachable kernel both answer null where a surface has
// to keep working; get_state is the authoritative streaming answer; and the
// event stream keeps the cached one honest between polls.

import { test } from "node:test";
import assert from "node:assert/strict";
import { EvAgentEnd, EvAgentSettled, EvError, EvMessageStart, createHostClient } from "../src/hostclient.ts";
import type { KernelRpc } from "../src/hostclient.ts";
import type { RpcEvent, RpcResponse, RpcSessionState } from "../src/types.ts";

function stub(overrides: Partial<KernelRpc> = {}) {
  const calls: Array<{ command: { type: string }; options?: { timeoutMs?: number } }> = [];
  const listeners: Array<(event: RpcEvent) => void> = [];
  let unsubscribed = 0;
  const rpc: KernelRpc = {
    call: async (command, options) => {
      calls.push({ command, options });
      return { id: "1", type: "response", command: command.type, success: true, data: { ok: true } };
    },
    send: () => {},
    onEvent: (listener) => {
      listeners.push(listener);
      return () => {
        unsubscribed++;
      };
    },
    close: () => {},
    kill: () => {},
    exitInfo: null,
    ...overrides,
  };
  return { rpc, calls, listeners, emit: (event: RpcEvent) => listeners.forEach((l) => l(event)), get unsubscribed() { return unsubscribed; } };
}

const state = (over: Partial<RpcSessionState> = {}): RpcSessionState => ({
  thinkingLevel: "off",
  isStreaming: false,
  isCompacting: false,
  steeringMode: "all",
  followUpMode: "all",
  sessionId: "s1",
  autoCompactionEnabled: false,
  messageCount: 1,
  pendingMessageCount: 0,
  ...over,
});

test("a command is passed through untouched, with its options", async () => {
  const s = stub();
  const host = createHostClient(s.rpc);
  const response = await host.call({ type: "get_commands" }, { timeoutMs: 1234 });
  assert.equal(response.success, true);
  assert.deepEqual(s.calls, [{ command: { type: "get_commands" }, options: { timeoutMs: 1234 } }]);
});

test("a refusal and an unreachable kernel both answer null", async () => {
  const refused = stub({
    call: async () => ({ id: "1", type: "response", command: "get_state", success: false, error: "no session" }),
  });
  assert.equal(await createHostClient(refused.rpc).getState(), null);

  const unreachable = stub({
    call: async () => {
      throw new Error("the kernel is gone");
    },
  });
  const host = createHostClient(unreachable.rpc);
  assert.equal(await host.getState(), null);
  assert.equal(await host.callOrNull({ type: "get_state" }), null);
  // The throwing call still rejects for a caller that wants the failure.
  await assert.rejects(host.call({ type: "get_state" }), /the kernel is gone/);
});

test("get_state is the authoritative streaming answer", async () => {
  const s = stub({
    call: async (command) => ({ id: "1", type: "response", command: command.type, success: true, data: state({ isStreaming: true }) }),
  });
  const host = createHostClient(s.rpc);
  assert.equal(host.isStreaming(), false);
  const got = await host.getState({ timeoutMs: 5000 });
  assert.equal(got?.isStreaming, true);
  assert.equal(host.isStreaming(), true);
});

test("the event stream keeps the cached streaming answer honest between polls", () => {
  const s = stub();
  const host = createHostClient(s.rpc);
  assert.equal(host.isStreaming(), false);
  s.emit({ type: EvMessageStart });
  assert.equal(host.isStreaming(), true, "a turn starting is in flight");
  s.emit({ type: "message_update", usage: "{}" });
  assert.equal(host.isStreaming(), true, "an unrelated event changes nothing");
  s.emit({ type: EvAgentEnd });
  assert.equal(host.isStreaming(), false);
  s.emit({ type: EvAgentSettled, aborted: false });
  assert.equal(host.isStreaming(), false);
  s.emit({ type: EvError });
  assert.equal(host.isStreaming(), false);
  // The host subscribes once at construction; the renderer's own
  // subscription is separate and unsubscribes independently.
  const off = host.onEvent(() => {});
  off();
  assert.equal(s.unsubscribed, 1);
});

test("close and kill are the lease and the hammer, and exitInfo is the kernel's last word", () => {
  let closed = 0;
  let killed = 0;
  const s = stub({
    close: () => closed++,
    kill: () => killed++,
    exitInfo: { code: 0, signal: null },
  });
  const host = createHostClient(s.rpc);
  host.close();
  host.kill();
  assert.equal(closed, 1);
  assert.equal(killed, 1);
  assert.deepEqual(host.exitInfo, { code: 0, signal: null });
});
