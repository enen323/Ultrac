// main-boot.test.ts — the headless half of the P-S1 gate. A child process
// runs the real src/main.ts with a fake electron (test/stubs/) and a fake
// kernel on the other end of the pipe, then drives the boot to its end:
// spawn → ready probe → window → bridge call → before-quit → the kernel
// honours the lease. What is left for a human with a screen is the pixels
// (plan §6: "pnpm start 能开窗"); everything else is asserted here.
//
// The stubs are module-customization hooks, not import interception inside
// the test: the child is `node --import test/stubs/register.mjs
// src/main.ts`, and the hook redirects "electron" and "node:child_process"
// (Node 22.22 verified that builtins can be redirected this way).

import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const REGISTER = path.join(here, "stubs", "register.mjs");
const MAIN = path.join(here, "..", "src", "main.ts");
const FAKE_KERNEL = path.join(here, "fake-kernel.mjs");

interface BootReport {
  appName: string | null;
  sessionLoaded: { sessionId: string; model?: { id: string }; messageCount: number } | null;
  events: Array<{ type: string }>;
  ipc: { handles: string[]; ons: string[] };
  bridgeCall: unknown;
  bridgeRefused: unknown;
  window: { created: boolean; shown: boolean; loadedFile: string | null; loadedUrl: string | null; preload: string | null };
  kernel: { spawned: boolean; exit: { code: number | null; signal: string | null } | null };
  dialogs: Array<{ title: string; detail: string }>;
  quit: number;
  menu: { cleared: number; built: number };
  exit: { code: number; why: string | null } | null;
}

async function runShell(scenario: string, readyTimeoutMs = 8000): Promise<{ report: BootReport | null; stderr: string; code: number | null }> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ultrac-main-boot-"));
  const reportPath = path.join(dir, "report.json");
  // Windows quirk (measured, Node 22.22): --import must be a file:// URL —
  // a plain Windows path there is parsed as a URL and dies with protocol
  // 'd:'. The entry itself takes a plain path.
  const child = spawn(process.execPath, ["--import", pathToFileURL(REGISTER).href, MAIN], {
    env: {
      ...process.env,
      ULTRAC_SHELL_REPORT: reportPath,
      ULTRAC_SHELL_FAKE_KERNEL: FAKE_KERNEL,
      ULTRAC_SHELL_FAKE_SCENARIO: scenario,
      // The binary the shell resolves (instanceID hashes its real path); the
      // spawn stub redirects to the fake kernel regardless.
      ULTRAC_BIN: FAKE_KERNEL,
      ULTRAC_SHELL_CWD: dir,
      ULTRAC_SHELL_READY_TIMEOUT_MS: String(readyTimeoutMs),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stderr = "";
  child.stdout?.on("data", () => {});
  child.stderr?.on("data", (chunk: Buffer) => {
    stderr += chunk.toString();
  });
  const code = await new Promise<number | null>((resolve) => child.on("exit", (c) => resolve(c)));
  let report: BootReport | null = null;
  try {
    report = JSON.parse(fs.readFileSync(reportPath, "utf8")) as BootReport;
  } catch {
    // A boot that crashed before its report: the stderr tail is the evidence.
  }
  return { report, stderr, code };
}

test("the boot spawns, probes, opens the page, bridges, and honours the lease", async () => {
  const { report, stderr, code } = await runShell("boot-events");
  assert.ok(report, `the stub child wrote no report; stderr:\n${stderr}`);
  assert.equal(report.exit?.why, null, `the boot did not settle cleanly: ${report.exit?.why}`);

  // Identity and lock, before anything was created.
  assert.equal(report.appName, "Ultrac");

  // The ready probe's answer is the SPA's initialization payload.
  assert.equal(report.sessionLoaded?.sessionId, "fake-session-0001");
  assert.equal(report.sessionLoaded?.model?.id, "fake-model");
  assert.equal(report.sessionLoaded?.messageCount, 1);

  // The window: shown, with the real preload. Which page it holds depends on
  // the P-S2 transition: with desktop/shell-spa/dist built the shell loads
  // the real SPA (plan §4); without it, the placeholder holds the window
  // (starting.ts missingPage). Both halves of loadPage's contract.
  const spaIndex = path.join(here, "..", "..", "shell-spa", "dist", "index.html");
  assert.equal(report.window.shown, true);
  assert.match(report.window.preload ?? "", /preload\.cjs$/);
  if (fs.existsSync(spaIndex)) {
    assert.equal(report.window.loadedFile, spaIndex);
    assert.equal(report.window.loadedUrl, null);
  } else {
    assert.equal(report.window.loadedFile, null);
    assert.match(report.window.loadedUrl ?? "", /^data:text\/html/);
  }

  // Events flow to the page, in order, id-less pushes and all.
  assert.deepEqual(
    report.events.map((e) => e.type),
    ["message_start", "message_update", "agent_end"],
  );

  // The bridge: a foreign sender is refused, the window's own is served.
  assert.deepEqual(report.bridgeRefused, { ok: false, code: "refused", message: "no window" });
  assert.deepEqual(report.bridgeCall, { ok: true, command: "get_commands", success: true, data: { echo: "get_commands" } });

  // The channels the page and the preload speak.
  assert.ok(report.ipc.handles.includes("kernel:call"));
  assert.ok(report.ipc.handles.includes("shell:open-external"));
  assert.ok(report.ipc.ons.includes("prefs:load"));
  assert.ok(report.ipc.ons.includes("prefs:save"));

  // The lease: before-quit ended stdin, and the kernel exited 0 because of it.
  assert.equal(report.kernel.spawned, true);
  assert.deepEqual(report.kernel.exit, { code: 0, signal: null });
  assert.equal(report.quit, 0, "the shell quit through the lease, not through app.quit");
  assert.equal(report.dialogs.length, 0);
  // Windows/Linux install no application menu; the installer still ran.
  assert.equal(report.menu.cleared, 1);
});

test("a kernel that never answers fails the launch with a dialog, not a hang", async () => {
  const { report, stderr, code } = await runShell("silent", 300);
  assert.ok(report, `the stub child wrote no report; stderr:\n${stderr}`);
  assert.equal(code, 0);
  assert.equal(report.sessionLoaded, null, "no window, no payload");
  assert.equal(report.window.created, false, "a failed launch opens no window");
  assert.equal(report.quit, 1, "failStartup ends the launch with app.quit");
  assert.equal(report.dialogs.length, 1);
  assert.match(report.dialogs[0].title, /Ultrac/);
  assert.match(report.dialogs[0].detail, /ready probe|handshake|timeout|内核/);
});
