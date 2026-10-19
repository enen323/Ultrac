// host.ts — spawn the kernel and learn that it is ready. This is section 2
// of docs/desktop-shell-plan.md, the "ready signal: zero protocol change"
// decision.
//
// # Why the probe is the handshake
//
// Reasonix's kernel opened with a structured line on stdout (version,
// origin, token) and the shell validated it before touching anything else.
// That works because the kernel is an HTTP server there. Ultrac's kernel
// is a pipe protocol: it writes nothing until asked, and every answer is
// correlated by the id of the command that caused it. So the shell asks a
// question whose answer is the readiness statement — `get_state` — and the
// response doubles as the SPA's initialization payload (session_loaded).
// No new line type, no protocol change, nothing for the dual-run driver or
// the print/json modes to learn about.
//
// # The slow-start / retry framework
//
// Ported from Reasonix's host.js and main.js (its launchKernel): a kernel
// that dies, or cannot be spawned, within seconds is tried exactly once
// more (a scanner holding a freshly written binary lets go about that
// fast); a slow kernel is waited for, never restarted. The timeout is
// generous because a cold start reads settings, resolves a model and
// builds a session manager before the first command is answered.

import { spawn } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { RpcCallError, RpcClient } from "./rpc-client.ts";
import { CmdGetState } from "./types.ts";
import type { RpcSessionState } from "./types.ts";

/** Why a launch has no kernel. Callers branch on the code, never the sentence. */
export type HostStartErrorCode = "timeout" | "exited" | "spawn" | "rejected" | "stopped";

export class HostStartError extends Error {
  code: HostStartErrorCode;
  /** How many attempts were made (Reasonix attaches the same field). */
  attempts = 1;

  constructor(code: HostStartErrorCode, message: string, detail: { attempts?: number } = {}) {
    super(message);
    this.name = "HostStartError";
    this.code = code;
    if (detail.attempts !== undefined) this.attempts = detail.attempts;
  }
}

/** How long the ready probe may take before the launch fails. */
export const READY_TIMEOUT_MS = 60_000;
/** When to report a slow kernel (the starting window) before the timeout. */
export const SLOW_AFTER_MS = 3_000;
/** A kernel that failed this soon after starting is worth one more try. */
export const RETRY_WITHIN_MS = 10_000;
/** Pause between the two attempts. */
export const RETRY_DELAY_MS = 1_000;

/**
 * The IPC message the shell sends the SPA once the probe has answered,
 * carrying the get_state payload (plan §2). Defined here so the main
 * process and the future preload bridge cannot drift on the name.
 */
export const SESSION_LOADED = "session_loaded";

/**
 * One more attempt, only for a kernel that stopped or could not be run
 * soon after it was started (a direct port of Reasonix's shouldRetry).
 */
export function shouldRetry(err: unknown, attempt: number, elapsedMs: number): boolean {
  const code = (err as { code?: unknown } | null | undefined)?.code;
  return attempt < 2 && (code === "exited" || code === "spawn") && elapsedMs <= RETRY_WITHIN_MS;
}

/** A live kernel: the child, its client, and the state the probe reported. */
export interface KernelHandle {
  child: ChildProcess;
  client: RpcClient;
  /** get_state's data — the session_loaded payload for the SPA. */
  state: RpcSessionState;
}

export interface LaunchOptions {
  /** Path to the ultrac binary (resolveKernelBinary finds the dev default). */
  binary: string;
  /** Arguments for the binary; kernelArgs() builds the usual set. */
  args?: string[];
  /** Working directory for the child (the shell also passes --cwd). */
  cwd?: string;
  /** Environment for the child; defaults to the shell's own. */
  env?: typeof process.env;
  /** How long the probe may take (default READY_TIMEOUT_MS). */
  timeoutMs?: number;
  /** When to report a slow kernel (default SLOW_AFTER_MS). */
  slowAfterMs?: number;
  /** Called once when the kernel is taking longer than slowAfterMs. */
  onSlow?: () => void;
  /** Kernel stderr, for the shell log (diagnostics never touch stdout). */
  onStderr?: (text: string) => void;
  /** Called when the kernel exits, ready or not. */
  onExit?: (code: number | null, signal: string | null) => void;
  /** Checked before each attempt; true ends the launch (the app is quitting). */
  shouldStop?: () => boolean;
}

/** The arguments that put a kernel into RPC mode (plan §3.2's spawn line). */
export function kernelArgs(options: { cwd?: string; continue?: boolean } = {}): string[] {
  const args = ["--mode", "rpc"];
  if (options.continue) args.push("-c");
  if (options.cwd) args.push("--cwd", options.cwd);
  return args;
}

/**
 * Where the kernel binary is. Dev reads the workspace's bin/ (plan §5: a
 * packaged build reads resourcesPath instead — that arrives with the
 * packaging phase). ULTRAC_BIN overrides for tests and odd checkouts.
 */
export function resolveKernelBinary(explicit?: string): string {
  if (explicit) return explicit;
  const fromEnv = process.env.ULTRAC_BIN;
  if (fromEnv) return fromEnv;
  const here = dirname(fileURLToPath(import.meta.url));
  const exe = process.platform === "win32" ? "ultrac.exe" : "ultrac";
  return resolve(here, "..", "..", "..", "bin", exe);
}

/** Spawn the kernel and attach a client — no probe yet. */
export function connectKernel(
  binary: string,
  args: string[],
  options: LaunchOptions = { binary, args },
): { child: ChildProcess; client: RpcClient } {
  const child = spawn(binary, args, {
    cwd: options.cwd,
    env: options.env ?? { ...process.env },
    stdio: ["pipe", "pipe", "pipe"],
    // ultrac is a console binary; without this a GUI shell flashes a
    // console window on every spawn on Windows.
    windowsHide: true,
  });
  child.stderr?.setEncoding("utf8");
  if (options.onStderr) child.stderr?.on("data", options.onStderr);
  if (options.onExit) child.on("exit", options.onExit);
  return { child, client: new RpcClient(child) };
}

/**
 * Ask the kernel whether it is ready, and take its answer as the state to
 * initialize the UI with. A rejected probe is a launch failure, not a
 * transport one: the kernel was alive enough to answer.
 */
export async function probeReady(client: RpcClient, options: { timeoutMs?: number } = {}): Promise<RpcSessionState> {
  const timeoutMs = options.timeoutMs ?? READY_TIMEOUT_MS;
  try {
    const response = await client.call({ type: CmdGetState }, { timeoutMs });
    if (!response.success) {
      throw new HostStartError(
        "rejected",
        `the kernel answered the ready probe with an error: ${response.error ?? "no message"}`,
      );
    }
    return response.data as RpcSessionState;
  } catch (err) {
    throw toHostStartError(err);
  }
}

/**
 * Spawn the kernel, probe it, and retry once if it died or could not be
 * spawned (see shouldRetry). Resolves with the live kernel and the state
 * the probe reported — the caller sends that to the SPA as session_loaded.
 */
export async function launchKernel(options: LaunchOptions): Promise<KernelHandle> {
  const timeoutMs = options.timeoutMs ?? READY_TIMEOUT_MS;
  const slowAfterMs = Math.min(options.slowAfterMs ?? SLOW_AFTER_MS, timeoutMs);
  let lastError: HostStartError | null = null;

  for (let attempt = 1; attempt <= 2; attempt++) {
    if (options.shouldStop?.()) {
      throw new HostStartError("stopped", "the launch was ended before a kernel was started", { attempts: attempt });
    }
    const began = Date.now();
    const { child, client } = connectKernel(options.binary, options.args ?? [], options);
    const slowTimer = options.onSlow ? setTimeout(options.onSlow, slowAfterMs) : null;
    slowTimer?.unref();
    try {
      const state = await probeReady(client, { timeoutMs });
      if (slowTimer) clearTimeout(slowTimer);
      return { child, client, state };
    } catch (err) {
      if (slowTimer) clearTimeout(slowTimer);
      const hostError = toHostStartError(err);
      hostError.attempts = attempt;
      lastError = hostError;
      const retry = !options.shouldStop?.() && shouldRetry(hostError, attempt, Date.now() - began);
      // A failed launch owns no live kernel: kill it whether we are about
      // to retry or to give up. A timed-out probe in particular leaves a
      // running process behind otherwise, and nobody is left to own it.
      // (For an exited or unspawnable kernel this is a harmless no-op.)
      child.kill();
      if (!retry) throw hostError;
      await new Promise((resolveDelay) => setTimeout(resolveDelay, RETRY_DELAY_MS));
    }
  }
  throw lastError ?? new HostStartError("stopped", "the launch ended without an attempt", { attempts: 2 });
}

/** Normalize any probe failure into a HostStartError (idempotent). */
function toHostStartError(err: unknown): HostStartError {
  if (err instanceof HostStartError) return err;
  if (err instanceof RpcCallError) {
    const code = err.code === "timeout" ? "timeout" : err.code === "spawn" ? "spawn" : "exited";
    return new HostStartError(code, err.message);
  }
  return new HostStartError("exited", `the ready probe failed: ${(err as Error)?.message ?? String(err)}`);
}
