// rpc-client.ts — the shell's client for the C4 protocol: `ultrac --mode
// rpc` over a spawned child's stdio pipes. This is what replaces Reasonix's
// hostclient.js (plan §3.2): where that one spoke HTTP plus a token to a
// loopback origin, this one speaks JSONL down the pipe — the pipe is the
// security boundary (decision D1), so there is no origin, no token and no
// gate to arm.
//
// # Correlation
//
// Every call() stamps an id on its command and resolves on the response
// that echoes it. Events carry no id — they are pushes, delivered to
// onEvent listeners in arrival order. Responses are told apart from events
// by `type: "response"`, never by the presence of an id (a fire-and-forget
// send() gets a response without one).
//
// # Failure propagation
//
// A call fails with RpcCallError when the kernel times out ("timeout"),
// exits before answering ("exited"), or could not be spawned ("spawn").
// The exit path is deferred by one turn of the event loop: a response
// already sitting in the pipe when the kernel died must still reach its
// waiter before the failure lands.

import { EventEmitter } from "node:events";
import type { ChildProcess } from "node:child_process";
import { JsonlSplitter } from "./jsonl.ts";
import type { RpcCommand, RpcEvent, RpcResponse } from "./types.ts";

/** Why a call could not be answered. */
export type RpcCallErrorCode = "timeout" | "exited" | "spawn";

/** The failure a call() promise rejects with. */
export class RpcCallError extends Error {
  readonly code: RpcCallErrorCode;

  constructor(code: RpcCallErrorCode, message: string) {
    super(message);
    this.name = "RpcCallError";
    this.code = code;
  }
}

interface PendingCall {
  resolve: (response: RpcResponse) => void;
  reject: (error: RpcCallError) => void;
  timer: ReturnType<typeof setTimeout> | null;
}

/** One RPC session over one child process. One client per child. */
export class RpcClient extends EventEmitter {
  readonly #child: ChildProcess;
  readonly #splitter = new JsonlSplitter();
  readonly #pending = new Map<string, PendingCall>();
  #nextId = 1;
  #failed = false;
  #spawnError: RpcCallError | null = null;
  #exit: { code: number | null; signal: string | null } | null = null;

  constructor(child: ChildProcess) {
    super();
    this.#child = child;

    const stdout = child.stdout;
    if (!stdout) throw new Error("RpcClient needs a child spawned with a stdout pipe");
    stdout.setEncoding("utf8");
    stdout.on("data", (chunk: string) => this.#onChunk(chunk));
    // The stream ending is the last word on responses: by the time this
    // fires, everything the kernel will ever say has been read. A spawn
    // failure also closes the streams — that case was already explained
    // by the 'error' branch below, and its code is the truer one.
    stdout.on("close", () => {
      if (this.#spawnError) return;
      this.#failPending(new RpcCallError("exited", "the kernel's stdout closed before the call was answered"));
    });

    // A process that could not be started emits this and never exits, so
    // without this branch the failure would surface as a timeout instead
    // of the truth (Reasonix's host.js documents the same trap).
    child.on("error", (err: Error) => {
      this.#spawnError = new RpcCallError("spawn", `the kernel could not be started: ${err.message}`);
      this.#failPending(this.#spawnError);
    });
    child.on("exit", (code, signal) => {
      this.#exit = { code, signal: signal ?? null };
      const how = code !== null ? `code ${code}` : `signal ${signal ?? "unknown"}`;
      setImmediate(() => {
        this.#failPending(new RpcCallError("exited", `the kernel exited with ${how} before the call was answered`));
        this.emit("exit", this.#exit);
      });
    });
    // A write to a pipe whose reader is gone surfaces as an error event
    // rather than a throw; without a listener that would crash the shell.
    // The exit path above is what actually fails the affected calls.
    child.stdin?.on("error", () => {});
  }

  /** Call a command and await its response, correlated by id. */
  call(command: Omit<RpcCommand, "id">, options: { timeoutMs?: number } = {}): Promise<RpcResponse> {
    const id = String(this.#nextId++);
    const timeoutMs = options.timeoutMs ?? 0;
    return new Promise<RpcResponse>((resolve, reject) => {
      if (this.#failed) {
        reject(new RpcCallError("exited", "the kernel is gone; no call can be answered"));
        return;
      }
      const timer =
        timeoutMs > 0
          ? setTimeout(() => {
              this.#pending.delete(id);
              reject(new RpcCallError("timeout", `no response to "${command.type}" within ${timeoutMs} ms`));
            }, timeoutMs)
          : null;
      timer?.unref();
      this.#pending.set(id, { resolve, reject, timer });
      try {
        this.#write({ ...command, id });
      } catch (err) {
        this.#pending.delete(id);
        if (timer) clearTimeout(timer);
        reject(new RpcCallError("exited", `the kernel's stdin is gone: ${(err as Error).message}`));
      }
    });
  }

  /** Send a command without waiting for its response (no id on the wire). */
  send(command: Omit<RpcCommand, "id">): void {
    this.#write(command);
  }

  /** Subscribe to pushed events (they carry no id). Returns an unsubscribe fn. */
  onEvent(listener: (event: RpcEvent) => void): () => void {
    const handler = (event: RpcEvent) => listener(event);
    this.on("event", handler);
    return () => {
      this.off("event", handler);
    };
  }

  /**
   * End the kernel's stdin — the lease (plan §0/D1). The kernel treats
   * stdin EOF as its shutdown signal (cmd/ultrac/rpc.go), so a shell that
   * exits without asking cannot leave an orphan behind. The shell calls
   * this when its last window closes.
   */
  close(): void {
    this.#child.stdin?.end();
  }

  /** Force-kill the kernel: a wedged launch, or a retry's cleanup. */
  kill(): void {
    this.#child.kill();
  }

  /** How the kernel ended, once it has (null while it lives). */
  get exitInfo(): { code: number | null; signal: string | null } | null {
    return this.#exit;
  }

  // -- internals ----------------------------------------------------------

  #onChunk(chunk: string): void {
    for (const line of this.#splitter.push(chunk)) {
      // The kernel never emits blank lines; skipping them costs nothing
      // and keeps a stray "\r\n" from reaching the JSON parser.
      if (line === "") continue;
      this.#dispatch(line);
    }
  }

  #dispatch(line: string): void {
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch (err) {
      // A line we cannot read is one we must not act on — but it is not a
      // reason to drop the session either (the kernel's own rule for
      // malformed input, mirrored here for its output).
      this.emit("malformed", { line, error: err as Error });
      return;
    }
    if (typeof parsed !== "object" || parsed === null) {
      this.emit("malformed", { line, error: new Error("the line was not a JSON object") });
      return;
    }
    const record = parsed as { id?: unknown; type?: unknown };
    if (record.type === "response") {
      const id = typeof record.id === "string" ? record.id : null;
      const pending = id === null ? undefined : this.#pending.get(id);
      if (id === null || !pending) {
        // An answer to a fire-and-forget send(), or a stale id: nobody is
        // waiting. Surfaced for diagnostics, never fatal.
        this.emit("unmatched", record);
        return;
      }
      this.#settle(id, pending, record as RpcResponse);
      return;
    }
    this.emit("event", record as RpcEvent);
  }

  #settle(id: string, pending: PendingCall, response: RpcResponse): void {
    this.#pending.delete(id);
    if (pending.timer) clearTimeout(pending.timer);
    pending.resolve(response);
  }

  #failPending(error: RpcCallError): void {
    if (this.#failed) return;
    this.#failed = true;
    for (const pending of this.#pending.values()) {
      if (pending.timer) clearTimeout(pending.timer);
      pending.reject(error);
    }
    this.#pending.clear();
  }

  #write(command: Omit<RpcCommand, "id">): void {
    const stdin = this.#child.stdin;
    if (!stdin) throw new Error("the kernel was spawned without a stdin pipe");
    stdin.write(JSON.stringify(command) + "\n");
  }
}
