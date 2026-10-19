// hostclient.ts — the main process's door to the kernel. This is the §3.2
// transformation of Reasonix's hostclient.js: where that one spoke HTTP plus
// a token to a loopback origin (and existed so the tray could ask the kernel
// things without routing through the renderer), this one speaks the C4 JSONL
// pipe. There is no origin, no token and no gate to arm — the pipe is the
// security boundary (decision D1), so this facade is about shaping, not
// access:
//
//   - call() stamps nothing: ids belong to RpcClient, which owns correlation.
//   - callOrNull() answers null for both a refusal and an unreachable
//     kernel, because every caller (the power guard, the tray-to-be) is a
//     surface that has to keep working while the kernel is going down.
//   - isStreaming() is cached from the event stream so a surface can ask
//     without a round trip; getState() is the authoritative version.
//
// The client is injected as an interface rather than a class so this file
// can be tested against a stub with no child process (and no Electron).

import type { RpcClient } from "./rpc-client.ts";
import { CmdGetState } from "./types.ts";
import type { RpcCommand, RpcEvent, RpcResponse, RpcSessionState } from "./types.ts";

// Event names this shell reacts to, mirrored from internal/transport/pi_json.go.
// The vocabulary is the agent's, not the shell's: the shell watches only the
// two that say whether a turn is in flight (plan §3.2).
export const EvMessageStart = "message_start";
export const EvAgentEnd = "agent_end";
export const EvAgentSettled = "agent_settled";
export const EvError = "error";

/** The slice of RpcClient this facade uses (satisfied by the real class). */
export interface KernelRpc {
  call(command: Omit<RpcCommand, "id">, options?: { timeoutMs?: number }): Promise<RpcResponse>;
  send(command: Omit<RpcCommand, "id">): void;
  onEvent(listener: (event: RpcEvent) => void): () => void;
  close(): void;
  kill(): void;
  readonly exitInfo: { code: number | null; signal: string | null } | null;
}

export interface HostClient {
  /** One command, one response. Rejects only when the kernel cannot answer at all. */
  call(command: Omit<RpcCommand, "id">, options?: { timeoutMs?: number }): Promise<RpcResponse>;
  /** The same, but null instead of a rejection: a surface that must keep working. */
  callOrNull(command: Omit<RpcCommand, "id">, options?: { timeoutMs?: number }): Promise<RpcResponse | null>;
  /** get_state's data, or null when the kernel could not be asked. */
  getState(options?: { timeoutMs?: number }): Promise<RpcSessionState | null>;
  /** Cached from the event stream; getState() is the authoritative version. */
  isStreaming(): boolean;
  onEvent(listener: (event: RpcEvent) => void): () => void;
  close(): void;
  kill(): void;
  readonly exitInfo: { code: number | null; signal: string | null } | null;
}

// A turn is in flight from message_start until the agent settles. agent_end
// alone is not enough: the loop can emit agent_end and then keep draining
// queued messages, and agent_settled{aborted:false} is the last word
// (internal/agent emits both; see the P6 notes).
function streamingAfter(event: RpcEvent): boolean | null {
  switch (event.type) {
    case EvMessageStart:
      return true;
    case EvAgentEnd:
      return false;
    case EvAgentSettled:
      return false;
    case EvError:
      return false;
    default:
      return null;
  }
}

export function createHostClient(rpc: KernelRpc): HostClient {
  let streaming = false;

  rpc.onEvent((event) => {
    const next = streamingAfter(event);
    if (next !== null) streaming = next;
  });

  async function callOrNull(command: Omit<RpcCommand, "id">, options?: { timeoutMs?: number }): Promise<RpcResponse | null> {
    try {
      return await rpc.call(command, options);
    } catch {
      return null;
    }
  }

  return {
    call: (command, options) => rpc.call(command, options),
    callOrNull,
    async getState(options) {
      const response = await callOrNull({ type: CmdGetState }, options);
      if (!response?.success) return null;
      const state = response.data as RpcSessionState | undefined;
      if (!state) return null;
      streaming = state.isStreaming;
      return state;
    },
    isStreaming: () => streaming,
    onEvent: (listener) => rpc.onEvent(listener),
    close: () => rpc.close(),
    kill: () => rpc.kill(),
    get exitInfo() {
      return rpc.exitInfo;
    },
  };
}

/** The real thing, for main.ts: a facade over the pipe client host.ts built. */
export function hostClientFor(client: RpcClient): HostClient {
  return createHostClient(client);
}
