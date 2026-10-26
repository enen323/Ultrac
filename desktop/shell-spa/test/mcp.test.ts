// mcp.test.ts — the MCP inventory in the store (mcp-plan.md P5).
//
// No DOM and no kernel: the store's bridge is injected, so this file drives
// it with a stub and asserts the two hooks the plan names — a pull when the
// panel opens, and a pull on a tool_execution_end whose tool carries the
// mcp__ prefix. What it does NOT assert is a timer: there is none, and that
// is the point (D6).

import assert from "node:assert/strict";
import { after, test } from "node:test";
import { SessionStore } from "../src/state/session.ts";
import type { CallResult, UltracBridge } from "../src/port/bridge.ts";
import type { McpServerStatus, RpcEvent, RpcSessionState } from "../src/state/wire.ts";

const STATE: RpcSessionState = {
  thinkingLevel: "off",
  isStreaming: false,
  isCompacting: false,
  steeringMode: "all",
  followUpMode: "all",
  sessionId: "s1",
  autoCompactionEnabled: false,
  messageCount: 0,
  pendingMessageCount: 0,
};

class Stub {
  calls: string[] = [];
  /** What get_mcp_status answers, or null to make the call fail. */
  mcpAnswer: McpServerStatus[] | null = [];
  failMcp = false;
  #events: Array<(event: RpcEvent) => void> = [];
  #loaded: Array<(state: RpcSessionState) => void> = [];

  readonly bridge: UltracBridge = {
    shell: "electron",
    platform: "win32",
    call: (command: { type: string }) => this.#call(command.type),
    onEvent: (listener) => {
      this.#events.push(listener);
      return () => {};
    },
    onSessionLoaded: (listener) => {
      this.#loaded.push(listener);
      return () => {};
    },
    openExternal: async () => {},
  };

  async #call(type: string): Promise<CallResult> {
    this.calls.push(type);
    const ok = (data: unknown): CallResult => ({ ok: true, command: type, success: true, data });
    switch (type) {
      case "get_messages":
        return ok({ messages: [] });
      case "get_available_models":
        return ok({ models: [] });
      case "get_available_thinking_levels":
        return ok({ levels: ["off"] });
      case "get_state":
        return ok(STATE);
      case "list_sessions":
        return ok({ sessions: [] });
      case "get_mcp_status":
        if (this.failMcp) return { ok: true, command: type, success: false, error: "内核不认识这个命令" };
        return ok({ servers: this.mcpAnswer ?? [] });
      default:
        return ok(null);
    }
  }

  /** The shell's handover; the store becomes ready after it. */
  async load(): Promise<void> {
    for (const listener of this.#loaded) await listener(STATE);
  }

  emit(event: RpcEvent): void {
    for (const listener of this.#events) listener(event);
  }

  called(type: string): boolean {
    return this.calls.includes(type);
  }

  countOf(type: string): number {
    return this.calls.filter((name) => name === type).length;
  }

  reset(): void {
    this.calls = [];
  }
}

const SERVERS: McpServerStatus[] = [
  { name: "codegraph", transport: "stdio", state: "ready", tools: 12 },
  { name: "filesystem", transport: "stdio", state: "cached", tools: 14, instructions: "a filesystem server" },
  { name: "broken", transport: "stdio", state: "failed", tools: 0, error: "command not found on PATH" },
];

function setup(): { stub: Stub; store: SessionStore } {
  const stub = new Stub();
  const store = new SessionStore(stub.bridge);
  stores.push(store);
  return { stub, store };
}

// Every store a test builds, so the file can let go of them at the end.
//
// This is not tidiness: the store starts its 2s get_state poll on
// message_start and stops it on agent_end (session.ts #startPoll), and one of
// the tests below feeds a message_start precisely because it must NOT trigger
// an MCP pull. Without the disposals that interval outlives the last test,
// the event loop never drains, and `node --test` reports the file as a
// failure 20 seconds later with nine passing assertions above it.
const stores: SessionStore[] = [];

after(() => {
  for (const store of stores) store.dispose();
});

// -- the open hook -------------------------------------------------------------

test("the panel's open pulls the inventory, and the snapshot carries it", async () => {
  const { stub, store } = setup();
  stub.mcpAnswer = SERVERS;
  await stub.load();
  stub.reset();

  await store.refreshMCPStatus();

  assert.equal(stub.called("get_mcp_status"), true);
  assert.deepEqual(
    store.getSnapshot().mcpServers.map((server) => server.name),
    ["codegraph", "filesystem", "broken"],
  );
  // The whole status travels, not just the names: the state and the tool
  // count are what a row shows, and the failure reason is why it is there.
  const broken = store.getSnapshot().mcpServers[2];
  assert.equal(broken.state, "failed");
  assert.equal(broken.tools, 0);
  assert.equal(broken.error, "command not found on PATH");
});

test("a server never contacted keeps its instructions", async () => {
  const { stub, store } = setup();
  stub.mcpAnswer = SERVERS;
  await stub.load();

  await store.refreshMCPStatus();

  // instructions is the only thing that can say what an uncontacted server
  // is for, so it survives a read that never touched the server.
  assert.equal(store.getSnapshot().mcpServers[1].instructions, "a filesystem server");
});

test("no servers is an empty list, not an error", async () => {
  const { stub, store } = setup();
  stub.mcpAnswer = [];
  await stub.load();

  await store.refreshMCPStatus();

  assert.deepEqual(store.getSnapshot().mcpServers, []);
});

test("a failed read leaves the last list alone instead of blanking it", async () => {
  const { stub, store } = setup();
  stub.mcpAnswer = SERVERS;
  await stub.load();
  await store.refreshMCPStatus();

  stub.failMcp = true;
  await store.refreshMCPStatus();

  // A panel that empties itself because one read failed would look like
  // every server vanished.
  assert.equal(store.getSnapshot().mcpServers.length, 3);
});

// -- the event hook ------------------------------------------------------------

test("an MCP tool finishing pulls", async () => {
  const { stub } = setup();
  await stub.load();
  stub.reset();

  stub.emit({ type: "tool_execution_end", toolCallId: "c1", toolName: "mcp__filesystem__read_file" });
  // Let the fire-and-forget refresh settle.
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(stub.called("get_mcp_status"), true);
});

test("a built-in tool finishing does not pull", async () => {
  const { stub } = setup();
  await stub.load();
  stub.reset();

  stub.emit({ type: "tool_execution_end", toolCallId: "c1", toolName: "read" });
  stub.emit({ type: "tool_execution_end", toolCallId: "c2", toolName: "bash" });
  stub.emit({ type: "message_end", message: { role: "assistant", content: [], timestamp: 1 } });
  await new Promise((resolve) => setTimeout(resolve, 0));

  // The union of the other MCP traffic is nil; a pull per tool call would be
  // a request per tool result.
  assert.equal(stub.countOf("get_mcp_status"), 0);
});

test("every other event type leaves it alone", async () => {
  const { stub } = setup();
  await stub.load();
  stub.reset();

  for (const event of [
    { type: "tool_execution_start", toolCallId: "c1", toolName: "mcp__filesystem__read_file" },
    { type: "tool_execution_update", toolCallId: "c1", toolName: "mcp__filesystem__read_file" },
    { type: "agent_end", messages: [] },
    { type: "message_start", message: { role: "assistant", content: [], timestamp: 1 } },
  ] as RpcEvent[]) {
    stub.emit(event);
  }
  await new Promise((resolve) => setTimeout(resolve, 0));

  // Only the END of an MCP tool is a state change: the start says it is
  // running, and the pool answers that with `spawning` only after a spawn.
  assert.equal(stub.countOf("get_mcp_status"), 0);
});

test("an event with no tool name is not treated as an MCP one", async () => {
  const { stub } = setup();
  await stub.load();
  stub.reset();

  stub.emit({ type: "tool_execution_end", toolCallId: "c1" });
  stub.emit({ type: "tool_execution_end", toolCallId: "c2", toolName: 42 });
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(stub.countOf("get_mcp_status"), 0);
});

test("the state a pull lands with replaces the previous one", async () => {
  const { stub, store } = setup();
  stub.mcpAnswer = [{ name: "fs", transport: "stdio", state: "cached", tools: 14 }];
  await stub.load();
  await store.refreshMCPStatus();
  assert.equal(store.getSnapshot().mcpServers[0].state, "cached");

  // The first call to one of its tools happened.
  stub.mcpAnswer = [{ name: "fs", transport: "stdio", state: "ready", tools: 14 }];
  await store.refreshMCPStatus();

  assert.equal(store.getSnapshot().mcpServers[0].state, "ready");
  assert.equal(store.getSnapshot().mcpServers.length, 1);
});
