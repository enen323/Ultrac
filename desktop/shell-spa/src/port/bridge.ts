// bridge.ts — the page's door to the kernel. preload.cjs (desktop/shell/src)
// exposes exactly one object, `window.ultrac`, and this file is its typed
// client. Four verbs, nothing else:
//
//   call(command)        one RPC command; resolves with the response or a
//                        failure code ({ ok: false, code, message }).
//   onEvent(listener)    every pushed kernel event, in arrival order.
//   onSessionLoaded(l)   the get_state payload, once, after the page loads.
//   openExternal(url)    http/https only; links.ts decides in main.
//
// The window object is injected rather than read from the global so tests
// can drive this file with a stub and no DOM (node:test, same as the shell's
// suite). A page opened outside the shell — `vite dev` in a plain browser —
// has no bridge, and bridgeOf says so with one clear error rather than a
// stream of mysterious failures.
//
// The failure codes are the shell's (main.ts's kernel:call handler maps
// RpcClient's timeout/exited/spawn onto them, plus its own "refused").

import type { RpcCommand, RpcEvent, RpcSessionState } from "../state/wire.ts";

/** What window.ultrac.call resolves with on success (main.ts:340-345). */
export interface CallOk {
  ok: true;
  command: string;
  success: boolean;
  data?: unknown;
  error?: string;
}

/** Why a call could not be answered. */
export type CallFailureCode = "timeout" | "spawn" | "exited" | "refused";

/** What window.ultrac.call resolves with when the kernel could not answer. */
export interface CallFailure {
  ok: false;
  code: CallFailureCode;
  message: string;
}

export type CallResult = CallOk | CallFailure;

/** The shape preload.cjs puts on the window. */
export interface UltracBridge {
  shell: string;
  platform: string;
  call: (command: RpcCommand) => Promise<CallResult>;
  onEvent: (listener: (event: RpcEvent) => void) => () => void;
  onSessionLoaded: (listener: (state: RpcSessionState) => void) => () => void;
  openExternal: (url: string) => Promise<void>;
}

declare global {
  interface Window {
    ultrac?: UltracBridge;
  }
}

/** Read the bridge off a window-like object, or explain its absence. */
export function bridgeOf(win: { ultrac?: UltracBridge } | undefined | null): UltracBridge {
  const bridge = win?.ultrac;
  if (!bridge) {
    throw new Error(
      "没有找到桌面壳的桥接（window.ultrac）。这个页面要在 Ultrac 桌面壳里运行；" +
        "在普通浏览器里打开只能看到界面，连不上内核。",
    );
  }
  return bridge;
}

/** True when the page is running inside the shell (and can reach a kernel). */
export function inShell(win: { ultrac?: UltracBridge } | undefined | null): boolean {
  return Boolean(win?.ultrac);
}

/**
 * Call a command and unwrap its response data. Throws with the kernel's own
 * message when the response says failure, or with the failure code when the
 * kernel could not answer at all — callers that care distinguish with
 * `isCallFailure`.
 */
export async function callData<T>(bridge: UltracBridge, command: RpcCommand): Promise<T> {
  const result = await bridge.call(command);
  if (!result.ok) throw new BridgeError(result.code, result.message);
  if (!result.success) throw new BridgeError("refused", result.error || `command "${command.type}" failed`);
  return result.data as T;
}

/** A call that could not be completed, carrying the shell's failure code. */
export class BridgeError extends Error {
  readonly code: CallFailureCode;

  constructor(code: CallFailureCode, message: string) {
    super(message);
    this.name = "BridgeError";
    this.code = code;
  }
}

/** True when an error is a bridge failure (the kernel could not answer). */
export function isCallFailure(err: unknown): err is BridgeError {
  return err instanceof BridgeError;
}

/** The response data of a command that answers with nothing (success, no data). */
export async function callVoid(bridge: UltracBridge, command: RpcCommand): Promise<void> {
  await callData<unknown>(bridge, command);
}
