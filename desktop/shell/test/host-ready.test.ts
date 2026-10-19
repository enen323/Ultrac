// host-ready.test.ts — the ready signal (plan §2, option c) against the
// fake kernel: the probe resolves with the state, a slow kernel is waited
// for rather than restarted, a dead or unspawnable kernel is retried once,
// and a rejected probe is a launch failure. The retry matrix is Reasonix's
// shouldRetry, ported — asserted here so it cannot drift.

import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import {
  READY_TIMEOUT_MS,
  RETRY_WITHIN_MS,
  SESSION_LOADED,
  SLOW_AFTER_MS,
  HostStartError,
  launchKernel,
  shouldRetry,
} from "../src/host.ts";

const fakeKernel = fileURLToPath(new URL("./fake-kernel.mjs", import.meta.url));
const bogusBinary = process.platform === "win32" ? "C:\\definitely\\not\\ultrac.exe" : "/definitely/not/ultrac";

function fakeArgs(scenario: string): string[] {
  return [fakeKernel, scenario];
}

test("the probe resolves with the kernel's state, and the client stays usable", async () => {
  const kernel = await launchKernel({ binary: process.execPath, args: fakeArgs("respond") });
  try {
    assert.equal(kernel.state.sessionId, "fake-session-0001");
    assert.equal(kernel.state.thinkingLevel, "off");
    assert.equal(kernel.state.isStreaming, false);
    assert.equal(kernel.state.messageCount, 1);
    assert.equal(kernel.state.model?.id, "fake-model");

    // Ready is not the end of the session: the shell keeps talking.
    const response = await kernel.client.call({ type: "get_commands" });
    assert.equal(response.success, true);
  } finally {
    kernel.client.close();
  }
});

test("a slow kernel is reported but waited for, never restarted", async () => {
  let slowReported = false;
  const kernel = await launchKernel({
    binary: process.execPath,
    args: fakeArgs("slow-respond"),
    slowAfterMs: 30,
    timeoutMs: 5000,
    onSlow: () => {
      slowReported = true;
    },
  });
  try {
    assert.equal(slowReported, true, "onSlow fired for the slow kernel");
    assert.equal(kernel.state.sessionId, "fake-session-0001");
  } finally {
    kernel.client.close();
  }
});

test("a silent kernel fails the launch with a timeout, after one attempt", async () => {
  await assert.rejects(
    launchKernel({ binary: process.execPath, args: fakeArgs("silent"), timeoutMs: 200 }),
    (err: unknown) => {
      assert.ok(err instanceof HostStartError, `expected HostStartError, got ${String(err)}`);
      assert.equal(err.code, "timeout");
      assert.equal(err.attempts, 1, "a slow kernel is waited for, never restarted");
      return true;
    },
  );
});

test("a kernel that dies before answering is retried exactly once", async () => {
  await assert.rejects(
    launchKernel({ binary: process.execPath, args: fakeArgs("exit-early") }),
    (err: unknown) => {
      assert.ok(err instanceof HostStartError, `expected HostStartError, got ${String(err)}`);
      assert.equal(err.code, "exited");
      assert.equal(err.attempts, 2);
      return true;
    },
  );
});

test("a kernel that cannot be spawned is retried exactly once", async () => {
  await assert.rejects(
    launchKernel({ binary: bogusBinary }),
    (err: unknown) => {
      assert.ok(err instanceof HostStartError, `expected HostStartError, got ${String(err)}`);
      assert.equal(err.code, "spawn");
      assert.equal(err.attempts, 2);
      return true;
    },
  );
});

test("a rejected probe fails the launch without a retry", async () => {
  await assert.rejects(
    launchKernel({ binary: process.execPath, args: fakeArgs("reject-probe") }),
    (err: unknown) => {
      assert.ok(err instanceof HostStartError, `expected HostStartError, got ${String(err)}`);
      assert.equal(err.code, "rejected");
      assert.equal(err.attempts, 1);
      return true;
    },
  );
});

test("shouldStop ends the launch before a kernel is started", async () => {
  await assert.rejects(
    launchKernel({ binary: process.execPath, args: fakeArgs("respond"), shouldStop: () => true }),
    (err: unknown) => {
      assert.ok(err instanceof HostStartError, `expected HostStartError, got ${String(err)}`);
      assert.equal(err.code, "stopped");
      assert.equal(err.attempts, 1);
      return true;
    },
  );
});

test("shouldRetry: one more try only for a fresh exited/spawn failure", () => {
  assert.equal(shouldRetry({ code: "exited" }, 1, 100), true);
  assert.equal(shouldRetry({ code: "spawn" }, 1, 100), true);
  assert.equal(shouldRetry({ code: "exited" }, 2, 100), false, "attempt two is the last");
  assert.equal(shouldRetry({ code: "timeout" }, 1, 100), false, "slow kernels are waited for");
  assert.equal(shouldRetry({ code: "rejected" }, 1, 100), false, "an answered probe is not a crash");
  assert.equal(shouldRetry({ code: "exited" }, 1, RETRY_WITHIN_MS + 1), false, "too late to be a scanner");
});

test("the readiness constants are what the plan fixed", () => {
  assert.equal(READY_TIMEOUT_MS, 60_000);
  assert.equal(SLOW_AFTER_MS, 3_000);
  assert.equal(SESSION_LOADED, "session_loaded");
});
