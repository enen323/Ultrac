// app.test.tsx — the rendered page against a stub kernel. This is the
// machine half of the P-S2 gate (plan §6): the manual half is a real
// endpoint in a real window, which needs a display and a key; this half
// needs neither, and it drives the same path — session_loaded, history
// replay, the typewriter, a tool card, stop, steer/follow-up, model and
// thinking switches, the session rail, rename, and the error banner.
//
// The stub is the shell's preload bridge (port/bridge.ts's UltracBridge),
// not the wire: everything above the bridge is the real page — the real
// store, the real reducer, the real components.

import { afterEach, beforeEach, expect, test } from "vitest";
import { cleanup, render, screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "../../src/ui/App.tsx";
import { SessionStore } from "../../src/state/session.ts";
import type { CallResult, UltracBridge } from "../../src/port/bridge.ts";
import type { RpcEvent, RpcSessionState, WireMessage } from "../../src/state/wire.ts";

// -- the stub kernel ------------------------------------------------------------

let stamp = 1_700_000_000_000;

function msg(role: WireMessage["role"], content: WireMessage["content"], extra: Partial<WireMessage> = {}): WireMessage {
  stamp += 1000;
  return { role, content, timestamp: stamp, ...extra };
}

class StubKernel {
  calls: Array<{ type: string } & Record<string, unknown>> = [];
  messages: WireMessage[] = [];
  state: RpcSessionState = {
    model: { id: "deepseek-chat", name: "DeepSeek Chat", api: "anthropic-messages", provider: "dsanthropic", reasoning: false, contextWindow: 128000 },
    thinkingLevel: "off",
    isStreaming: false,
    isCompacting: false,
    steeringMode: "all",
    followUpMode: "all",
    sessionFile: "/sessions/current.jsonl",
    sessionId: "s-current",
    autoCompactionEnabled: false,
    messageCount: 2,
    pendingMessageCount: 0,
  };

  readonly bridge: UltracBridge = {
    shell: "electron",
    platform: "win32",
    call: (command) => this.#call(command),
    onEvent: (listener) => this.#on("event", listener),
    onSessionLoaded: (listener) => this.#on("loaded", listener),
    openExternal: async () => {},
  };

  #eventListeners: Array<(event: RpcEvent) => void> = [];
  #loadedListeners: Array<(state: RpcSessionState) => void> = [];

  #on(kind: "event", listener: (event: RpcEvent) => void): () => void;
  #on(kind: "loaded", listener: (state: RpcSessionState) => void): () => void;
  #on(kind: "event" | "loaded", listener: (arg: never) => void): () => void {
    const list = kind === "event" ? this.#eventListeners : this.#loadedListeners;
    list.push(listener as never);
    return () => {
      const index = list.indexOf(listener as never);
      if (index >= 0) list.splice(index, 1);
    };
  }

  async #call(command: Record<string, unknown> & { type: string }): Promise<CallResult> {
    this.calls.push(command);
    const ok = (data: unknown): CallResult => ({ ok: true, command: command.type, success: true, data });
    switch (command.type) {
      case "get_state":
        return ok(this.state);
      case "get_messages":
        return ok({ messages: this.messages });
      case "get_available_models":
        return ok({
          models: [
            this.state.model!,
            { id: "deepseek-reasoner", name: "DeepSeek Reasoner", api: "anthropic-messages", provider: "dsanthropic", reasoning: true, contextWindow: 128000 },
          ],
        });
      case "get_available_thinking_levels":
        return ok({ levels: ["off", "low", "high"] });
      case "list_sessions":
        return ok({
          sessions: [
            { sessionId: "s-old", path: "/sessions/old.jsonl", title: "旧会话甲", timestamp: "2026-10-19T10:00:00Z", messageCount: 4 },
            { sessionId: "s-current", path: "/sessions/current.jsonl", title: "当前会话", timestamp: "2026-10-20T08:00:00Z", messageCount: 2 },
          ],
        });
      case "prompt":
        return ok({ disposition: "started" });
      case "resume_session":
        return ok({ cancelled: false });
      case "set_model":
        return ok({ ...(this.state.model ?? {}), id: command.modelId, name: command.modelId });
      default:
        return ok(null);
    }
  }

  /** Push one kernel event, inside act so React processes it. */
  async emit(event: RpcEvent): Promise<void> {
    await act(async () => {
      for (const listener of this.#eventListeners) listener(event);
    });
  }

  /** The shell's session_loaded handover. */
  async load(state: RpcSessionState = this.state): Promise<void> {
    this.state = state;
    await act(async () => {
      for (const listener of this.#loadedListeners) listener(state);
    });
  }

  called(type: string): boolean {
    return this.calls.some((command) => command.type === type);
  }

  callOf(type: string): Array<{ type: string } & Record<string, unknown>> {
    return this.calls.filter((command) => command.type === type);
  }
}

// -- the tests -------------------------------------------------------------------

let kernel: StubKernel;
let store: SessionStore;

beforeEach(() => {
  stamp = 1_700_000_000_000;
  kernel = new StubKernel();
  kernel.messages = [msg("user", "你好"), msg("assistant", [{ type: "text", text: "你好！有什么可以帮你？" }])];
  store = new SessionStore(kernel.bridge);
});

afterEach(() => {
  store.dispose();
  cleanup();
});

test("boot: session_loaded replays history and the rail lists sessions", async () => {
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  // The snapshot becomes rows (system rows are dropped by the reducer).
  expect(await screen.findByText("你好")).toBeTruthy();
  expect(screen.getByText("你好！有什么可以帮你？")).toBeTruthy();

  // The rail: list_sessions ran, the current session is marked.
  await waitFor(() => expect(kernel.called("list_sessions")).toBe(true));
  expect(screen.getByText("旧会话甲")).toBeTruthy();
  // getByText lands on the inner span; the state is on the row button.
  expect(screen.getByText("当前会话").closest("button")?.getAttribute("aria-current")).toBe("true");
});

test("send: an idle prompt starts a run and its text streams in", async () => {
  const user = userEvent.setup();
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.type(screen.getByPlaceholderText(/说点什么/), "读一下 main.go");
  await user.click(screen.getByText("发送"));

  await waitFor(() =>
    expect(kernel.callOf("prompt").some((command) => command.message === "读一下 main.go")).toBe(true),
  );

  // The run's events build the rows: the user echo, then the typewriter.
  await kernel.emit({ type: "agent_start" });
  await kernel.emit({ type: "message_start", message: msg("user", "读一下 main.go") });
  await kernel.emit({ type: "message_start", message: msg("assistant", []) });
  await kernel.emit({ type: "message_update", usage: null, assistantMessageEvent: { type: "text_delta", contentIndex: 0, delta: "我来" } });
  await kernel.emit({ type: "message_update", usage: null, assistantMessageEvent: { type: "text_delta", contentIndex: 0, delta: "看看" } });

  expect(await screen.findByText("我来看看")).toBeTruthy();
  // message_end swaps in the authoritative text.
  await kernel.emit({ type: "message_end", message: msg("assistant", [{ type: "text", text: "我来看看这个文件。" }]) });
  expect(await screen.findByText("我来看看这个文件。")).toBeTruthy();
});

test("a tool call becomes a card: running → done → output", async () => {
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await kernel.emit({ type: "message_start", message: msg("assistant", []) });
  await kernel.emit({
    type: "message_end",
    message: msg("assistant", [{ type: "toolCall", id: "c1", name: "read", arguments: { path: "main.go" } }]),
  });
  await kernel.emit({ type: "tool_execution_start", toolCallId: "c1", toolName: "read", args: { path: "main.go" } });

  expect(screen.getByText("read")).toBeTruthy();
  expect(screen.getByText("运行中…")).toBeTruthy();

  await kernel.emit({
    type: "tool_execution_end",
    toolCallId: "c1",
    toolName: "read",
    result: { content: [{ type: "text", text: "package main" }] },
    isError: false,
    durationMs: 12,
  });
  expect(screen.getByText("12ms")).toBeTruthy();
  expect(screen.queryByText("运行中…")).toBeNull();

  // The result arrives as its own message and lands inside the card.
  await kernel.emit({ type: "message_start", message: msg("toolResult", [{ type: "text", text: "package main" }], { toolCallId: "c1", toolName: "read" }) });

  // Collapsed by default; expanding shows arguments and output.
  await userEvent.click(screen.getByText("展开"));
  expect(screen.getByText("package main")).toBeTruthy();
  expect(screen.getByText(/"path": "main.go"/)).toBeTruthy();
});

test("stop: the button aborts the run; steer/follow-up name the queue", async () => {
  const user = userEvent.setup();
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  // Open a run.
  await kernel.emit({ type: "message_start", message: msg("user", "在吗") });
  await kernel.emit({ type: "message_start", message: msg("assistant", []) });
  expect(await screen.findByText("停止")).toBeTruthy();

  // A message sent mid-run must name its queue. Pick the queue first, then
  // type (typing focuses the textarea; clicking the segmented button would
  // otherwise leave focus on the button and Enter would go nowhere).
  await user.click(screen.getByText("排后"));
  await user.type(screen.getByPlaceholderText(/运行中/), "稍等");
  await user.keyboard("{Enter}");
  await waitFor(() =>
    expect(kernel.callOf("prompt").some((command) => command.message === "稍等" && command.streamingBehavior === "followUp")).toBe(true),
  );

  // Stop aborts.
  await user.click(screen.getByText("停止"));
  await waitFor(() => expect(kernel.called("abort")).toBe(true));

  // Settled: the stop button goes away.
  await kernel.emit({ type: "agent_end", messages: [] });
  await kernel.emit({ type: "agent_settled", aborted: true });
  expect(await waitFor(() => expect(screen.queryByText("停止")).toBeNull())).toBeTruthy();
});

test("model and thinking switches call the kernel", async () => {
  const user = userEvent.setup();
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.selectOptions(screen.getByLabelText("模型"), "deepseek-reasoner");
  await waitFor(() =>
    expect(kernel.callOf("set_model").some((command) => command.provider === "dsanthropic" && command.modelId === "deepseek-reasoner")).toBe(true),
  );

  await user.selectOptions(screen.getByLabelText("思考"), "high");
  await waitFor(() => expect(kernel.callOf("set_thinking_level").some((command) => command.level === "high")).toBe(true));
});

test("the rail resumes a session and the header renames the current one", async () => {
  const user = userEvent.setup();
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();
  await waitFor(() => expect(screen.getByText("旧会话甲")).toBeTruthy());

  await user.click(screen.getByText("旧会话甲"));
  await waitFor(() => expect(kernel.callOf("resume_session").some((command) => command.sessionPath === "/sessions/old.jsonl")).toBe(true));

  await user.click(screen.getByTitle("点击重命名会话"));
  const input = screen.getByPlaceholderText("会话名字");
  await user.clear(input);
  await user.type(input, "改名后的会话{Enter}");
  await waitFor(() => expect(kernel.callOf("set_session_name").some((command) => command.name === "改名后的会话")).toBe(true));
});

test("a failed run shows the error banner", async () => {
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await kernel.emit({
    type: "message_start",
    message: msg("assistant", [{ type: "text", text: "" }], { stopReason: "error", errorMessage: "连接远端失败" }),
  });
  await kernel.emit({
    type: "message_end",
    message: msg("assistant", [{ type: "text", text: "" }], { stopReason: "error", errorMessage: "连接远端失败" }),
  });

  expect(await screen.findByText("连接远端失败")).toBeTruthy();
  expect(screen.getByText("出错了")).toBeTruthy();
});
