// rpc-client.test.ts — the JSONL client against a scriptable fake kernel
// (test/fake-kernel.mjs). What is under test: id correlation (including
// out-of-order answers), event delivery, the failure taxonomy
// (timeout / exited / spawn), the stdin lease, and survival of hostile
// bytes on the wire (U+2028, CRLF, records split across chunks — including
// a chunk that cuts a character's UTF-8 bytes in half).

import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import { once } from "node:events";
import { fileURLToPath } from "node:url";
import { RpcCallError, RpcClient } from "../src/rpc-client.ts";
import type { RpcEvent, RpcSessionState } from "../src/types.ts";

const fakeKernel = fileURLToPath(new URL("./fake-kernel.mjs", import.meta.url));

function startFake(scenario: string): ChildProcess {
  return spawn(process.execPath, [fakeKernel, scenario], { stdio: ["pipe", "pipe", "pipe"] });
}

async function waitFor(predicate: () => boolean, timeoutMs = 2000): Promise<void> {
  const began = Date.now();
  for (;;) {
    if (predicate()) return;
    if (Date.now() - began > timeoutMs) throw new Error("waitFor timed out");
    await new Promise((r) => setTimeout(r, 10));
  }
}

test("resolves a call with the response that echoes its id, and stays usable", async () => {
  const child = startFake("respond");
  const client = new RpcClient(child);
  try {
    const first = await client.call({ type: "get_state" });
    assert.equal(first.id, "1");
    assert.equal(first.type, "response");
    assert.equal(first.command, "get_state");
    assert.equal(first.success, true);
    assert.equal((first.data as RpcSessionState).sessionId, "fake-session-0001");

    const second = await client.call({ type: "get_commands" });
    assert.equal(second.id, "2");
    assert.equal(second.command, "get_commands");
    assert.equal(second.success, true);
  } finally {
    client.close();
  }
});

test("correlates concurrent calls even when the answers arrive out of order", async () => {
  const child = startFake("respond-reverse");
  const client = new RpcClient(child);
  try {
    const first = client.call({ type: "get_state" });
    const second = client.call({ type: "get_available_models" });
    const [a, b] = await Promise.all([first, second]);
    assert.equal(a.command, "get_state");
    assert.equal(b.command, "get_available_models");
    assert.equal((a.data as RpcSessionState).sessionId, "fake-session-0001");
    assert.deepEqual(b.data, { echo: "get_available_models" });
  } finally {
    client.close();
  }
});

test("survives hostile bytes on the wire: U+2028, CRLF, split records, split characters", async () => {
  const child = startFake("framing");
  const client = new RpcClient(child);
  const events: RpcEvent[] = [];
  client.onEvent((e) => events.push(e));
  try {
    const response = await client.call({ type: "get_state" });
    assert.equal(events.length, 1, "exactly one push arrived");
    assert.equal(events[0].type, "probe_event");
    // The U+2028/U+2029 pair arrived intact inside one record.
    assert.equal(events[0].note, "x\u2028y\u2029z");
    assert.equal(response.command, "get_state");
    assert.equal(response.success, true);
    assert.equal((response.data as RpcSessionState).sessionId, "fake-session-0001");
  } finally {
    client.close();
  }
});

test("delivers events in arrival order, including one that precedes its call's response", async () => {
  const child = startFake("framing");
  const client = new RpcClient(child);
  const seen: string[] = [];
  client.onEvent((e) => seen.push(e.type));
  try {
    await client.call({ type: "get_state" });
    assert.deepEqual(seen, ["probe_event"]);
  } finally {
    client.close();
  }
});

test("times out a call the kernel never answers", async () => {
  const child = startFake("silent");
  const client = new RpcClient(child);
  try {
    await assert.rejects(
      client.call({ type: "get_state" }, { timeoutMs: 150 }),
      (err: unknown) => {
        assert.ok(err instanceof RpcCallError, `expected RpcCallError, got ${String(err)}`);
        assert.equal(err.code, "timeout");
        return true;
      },
    );
  } finally {
    child.kill();
  }
});

test("rejects a pending call when the kernel exits before answering, and fast-fails the next one", async () => {
  const child = startFake("exit-early");
  const client = new RpcClient(child);
  try {
    await assert.rejects(
      client.call({ type: "get_state" }, { timeoutMs: 5000 }),
      (err: unknown) => {
        assert.ok(err instanceof RpcCallError, `expected RpcCallError, got ${String(err)}`);
        assert.equal(err.code, "exited");
        return true;
      },
    );
    await assert.rejects(client.call({ type: "get_state" }), (err: unknown) => {
      assert.ok(err instanceof RpcCallError);
      assert.equal(err.code, "exited");
      return true;
    });
  } finally {
    child.kill();
  }
});

test("reports a kernel that could not be spawned as a spawn failure, not a timeout", async () => {
  const bogus = process.platform === "win32" ? "C:\\definitely\\not\\ultrac.exe" : "/definitely/not/ultrac";
  const child = spawn(bogus, ["--mode", "rpc"], { stdio: ["pipe", "pipe", "pipe"] });
  const client = new RpcClient(child);
  await assert.rejects(
    client.call({ type: "get_state" }, { timeoutMs: 5000 }),
    (err: unknown) => {
      assert.ok(err instanceof RpcCallError, `expected RpcCallError, got ${String(err)}`);
      assert.equal(err.code, "spawn");
      return true;
    },
  );
});

test("close() ends stdin and the kernel exits 0 — the lease", async () => {
  const child = startFake("hold-open");
  const client = new RpcClient(child);
  const state = await client.call({ type: "get_state" });
  assert.equal(state.success, true);

  client.close();
  const [code] = await once(child, "exit");
  assert.equal(code, 0);
});

test("a malformed line does not kill the stream", async () => {
  const child = startFake("malformed-then-respond");
  const client = new RpcClient(child);
  const malformed: unknown[] = [];
  client.on("malformed", (m) => malformed.push(m));
  try {
    const response = await client.call({ type: "get_state" });
    assert.equal(response.success, true);
    assert.equal(malformed.length, 1);
  } finally {
    client.close();
  }
});

test("send() writes without an id; the id-less answer is surfaced as unmatched", async () => {
  const child = startFake("respond");
  const client = new RpcClient(child);
  const unmatched: Array<{ command?: string }> = [];
  client.on("unmatched", (r) => unmatched.push(r));
  try {
    client.send({ type: "get_commands" });
    await waitFor(() => unmatched.length === 1);
    assert.equal(unmatched[0].command, "get_commands");
  } finally {
    client.close();
  }
});

test("a failed call leaves the client alive for the next one", async () => {
  const child = startFake("respond");
  const client = new RpcClient(child);
  try {
    // A rejected probe is a launch failure, but the client itself stays
    // usable: the stream is fine, only that answer was a no.
    const rejected = await client.call({ type: "no_such_command" });
    assert.equal(rejected.success, false);
    assert.match(rejected.error ?? "", /Unknown command/);
    const ok = await client.call({ type: "get_state" });
    assert.equal(ok.success, true);
  } finally {
    client.close();
  }
});
