// kernel-probe.test.ts — the ready signal against the real kernel
// (bin/ultrac.exe), which is the acceptance evidence for plan §2's
// "zero protocol change" claim:
//
//   1. the probe is answered (the request-response pair IS the handshake),
//   2. the kernel pushes nothing at all before answering — no handshake
//      line, no banner, nothing for a client to mistake for protocol,
//   3. the state it returns carries everything the SPA needs to initialize,
//   4. closing stdin ends the kernel with exit 0 — the parent lease.
//
// The kernel runs hermetically: a throwaway ULTRAC_CONFIG_DIR with a dummy
// credential (the probe never calls a model) and a throwaway project cwd,
// so no real session or setting is touched.

import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { once } from "node:events";
import { connectKernel, kernelArgs, probeReady, resolveKernelBinary } from "../src/host.ts";
import type { RpcSessionState } from "../src/types.ts";

const binary = resolveKernelBinary();

/** A throwaway config home: dummy credential + default model, nothing else. */
function makeConfigHome(): string {
  const dir = mkdtempSync(join(tmpdir(), "ultrac-shell-cfg-"));
  writeFileSync(
    join(dir, "settings.json"),
    JSON.stringify({
      env: {
        ANTHROPIC_AUTH_TOKEN: "shell-test-dummy-token",
        ANTHROPIC_MODEL: "deepseek-flash",
      },
    }),
  );
  return dir;
}

test("the real kernel answers the ready probe and pushes nothing before it", async (t) => {
  if (!existsSync(binary)) {
    t.skip(`no kernel binary at ${binary}; build it with: go build -o bin/ultrac.exe ./cmd/ultrac`);
    return;
  }

  const configHome = makeConfigHome();
  const cwd = mkdtempSync(join(tmpdir(), "ultrac-shell-cwd-"));

  const { child, client } = connectKernel(binary, kernelArgs({ cwd }), {
    cwd,
    env: { ...process.env, ULTRAC_CONFIG_DIR: configHome },
  });

  // Everything the kernel could push before the probe answers must be
  // nothing: that is the zero-protocol-change claim, observed.
  const beforeReady: unknown[] = [];
  client.onEvent((e) => beforeReady.push(e));
  client.on("malformed", (m) => beforeReady.push(m));
  client.on("unmatched", (r) => beforeReady.push(r));

  const state: RpcSessionState = await probeReady(client, { timeoutMs: 30_000 });

  assert.deepEqual(beforeReady, [], "the kernel wrote nothing before answering the probe");

  // The state doubles as the SPA's session_loaded payload (plan §2).
  assert.equal(typeof state.sessionId, "string");
  assert.ok(state.sessionId.length > 0, "sessionId initializes the sidebar");
  assert.ok(state.sessionFile && state.sessionFile.length > 0, "sessionFile points at the transcript");
  assert.ok(state.sessionFile.includes("sessions"), "sessions live under the config home");
  assert.equal(state.isStreaming, false);
  assert.equal(typeof state.thinkingLevel, "string");
  assert.ok(state.messageCount >= 0);
  assert.ok(state.model, "a session always has a model in this port");
  assert.ok(state.model.id.length > 0);
  assert.ok(state.model.provider.length > 0);

  // The lease: closing stdin is how the shell tells the kernel to leave.
  client.close();
  const [code] = await once(child, "exit");
  assert.equal(code, 0, "stdin EOF is the kernel's shutdown signal (exit 0)");
});

test("the client keeps working after ready — a second command is answered", async (t) => {
  if (!existsSync(binary)) {
    t.skip(`no kernel binary at ${binary}`);
    return;
  }

  const configHome = makeConfigHome();
  const cwd = mkdtempSync(join(tmpdir(), "ultrac-shell-cwd-"));

  const { child, client } = connectKernel(binary, kernelArgs({ cwd }), {
    cwd,
    env: { ...process.env, ULTRAC_CONFIG_DIR: configHome },
  });
  try {
    await probeReady(client, { timeoutMs: 30_000 });
    const commands = await client.call({ type: "get_commands" }, { timeoutMs: 10_000 });
    assert.equal(commands.success, true);
    assert.deepEqual(commands.data, { commands: [] }, "no extensions: the honest empty list");
  } finally {
    client.close();
    await once(child, "exit");
  }
});
