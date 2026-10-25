// app.test.tsx — the rendered page against a stub kernel. This is the
// machine half of the P-S2 gate (plan §6): the manual half is a real
// endpoint in a real window, which needs a display and a key; this half
// needs neither, and it drives the same path — session_loaded, history
// replay, the typewriter, a tool card, stop, steer/follow-up, model and
// thinking switches, the session rail, rename, the error banner, and the
// P-C4 dock entries (compact, the compacting flag, the fork picker).
//
// The stub is the shell's preload bridge (port/bridge.ts's UltracBridge),
// not the wire: everything above the bridge is the real page — the real
// store, the real reducer, the real components.

import { afterEach, beforeEach, expect, test } from "vitest";
import { cleanup, render, screen, waitFor, act, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "../../src/ui/App.tsx";
import { SessionStore } from "../../src/state/session.ts";
import type { CallResult, UltracBridge } from "../../src/port/bridge.ts";
import type { CompactResult, CustomModelResult, ForkMessage, ForkResult, RpcEvent, RpcSessionState, WireMessage } from "../../src/state/wire.ts";

// The page under test drives the P-C4 dock entries too: the compaction
// button, the fork picker, and the compacting flag on the water mark —
// and the custom-model dialog (custom-model-plan.md P4).

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

  // P-C4 answers. compact swaps the conversation for its summary (the
  // kernel rewrote the context; get_messages is re-read afterwards), fork
  // for the abandoned suffix — the same re-read, so both live here.
  compactAnswer: CompactResult | null = null;
  /** When set, compact answers with this refusal (the kernel's message). */
  compactError: string | null = null;
  forkCandidates: ForkMessage[] = [];
  forkAnswer: ForkResult = { leafId: null };
  /** What get_messages answers after a fork (the surviving projection). */
  forkKeeps: WireMessage[] | null = null;

  /** What get_available_thinking_levels answers: the levels of the model IN
   * USE, so a test can change it the way a re-save of that model does. */
  thinkingAnswer: string[] = ["off", "low", "high"];

  // add_custom_model's answers: the fresh catalog on success (the store
  // patches the picker from it), or the kernel's refusal.
  addAnswer: CustomModelResult | null = null;
  addError: string | null = null;

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
        return ok({ levels: this.thinkingAnswer });
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
      case "compact":
        if (this.compactError) return { ok: true, command: command.type, success: false, error: this.compactError };
        // The compaction rewrote the context to the summary; the store's
        // re-read (get_messages) is what the page ends up showing.
        this.messages = [msg("assistant", [{ type: "text", text: this.compactAnswer?.summary ?? "【摘要】" }])];
        return ok(this.compactAnswer ?? { summary: "【摘要】", firstKeptEntryId: "e2", tokensBefore: 0, estimatedTokensAfter: 0, usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, details: { readFiles: [], modifiedFiles: [] } });
      case "get_fork_messages":
        return ok({ messages: this.forkCandidates });
      case "fork": {
        // A fork abandons the suffix after the picked entry; the store's
        // re-read (get_messages) is what the page ends up showing, so the
        // test decides what survived via forkKeeps. The answer's
        // editorText (a user-message target) rides to the composer.
        if (this.forkKeeps) this.messages = this.forkKeeps;
        return ok(this.forkAnswer);
      }
      case "set_model":
        return ok({ ...(this.state.model ?? {}), id: command.modelId, name: command.modelId });
      case "set_thinking_level": {
        // The kernel clamps instead of rejecting (server.go's
        // cmdSetThinkingLevel); the fake clamps the same way, so the store's
        // follow-up get_state shows the settled level.
        const requested = String(command.level);
        this.state = {
          ...this.state,
          thinkingLevel: this.thinkingAnswer.includes(requested) ? requested : (this.thinkingAnswer[0] ?? "off"),
        };
        return ok(null);
      }
      case "add_custom_model": {
        if (this.addError) return { ok: true, command: command.type, success: false, error: this.addError };
        // The kernel declares the model and reloads; if the saved model is
        // the one in use, what it supports changed under the session — the
        // fake mirrors that so a stale 思考 list shows up as a failure.
        const levels = command.thinkingLevels;
        if (Array.isArray(levels)) this.thinkingAnswer = ["off", ...(levels as string[])];
        return ok(this.addAnswer ?? { models: [], providers: [] });
      }
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

  // The option value is the provider/id pair (ModelPicker): two providers
  // can expose the same model id once custom models exist.
  await user.selectOptions(screen.getByLabelText("模型"), "dsanthropic/deepseek-reasoner");
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

// -- P-C4: compaction + forking (the dock's two new entries) -----------------

test("compact: the dock button compacts, and the transcript re-reads the summary", async () => {
  const user = userEvent.setup();
  kernel.compactAnswer = {
    summary: "【压缩摘要】我们在讨论 main.go 的结构。",
    firstKeptEntryId: "e2",
    tokensBefore: 42000,
    estimatedTokensAfter: 900,
    usage: { input: 40000, output: 2000, cacheRead: 0, cacheWrite: 0, totalTokens: 42000, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
    details: { readFiles: ["main.go"], modifiedFiles: [] },
  };
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.click(screen.getByText("压缩上下文"));
  await waitFor(() => expect(kernel.called("compact")).toBe(true));

  // The kernel rewrote the context to the summary; the page re-read it —
  // the old exchange is gone, the summary row is what survives.
  expect(await screen.findByText("【压缩摘要】我们在讨论 main.go 的结构。")).toBeTruthy();
  expect(screen.queryByText("你好！有什么可以帮你？")).toBeNull();
});

test("compact refusal: the kernel's idempotence message lands in the banner", async () => {
  const user = userEvent.setup();
  kernel.compactError = "Nothing to compact (session too small)";
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.click(screen.getByText("压缩上下文"));
  expect(await screen.findByText("Nothing to compact (session too small)")).toBeTruthy();
  // A refusal leaves the conversation alone.
  expect(screen.getByText("你好！有什么可以帮你？")).toBeTruthy();
});

test("while the kernel compacts, both the button and the water mark say so", async () => {
  kernel.state.isCompacting = true;
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  // The dock button disables itself and renames; the status line flags.
  const button = screen.getByRole("button", { name: "压缩中…" });
  expect((button as HTMLButtonElement).disabled).toBe(true);
  expect(screen.queryByText("压缩上下文")).toBeNull();
  expect(within(screen.getByRole("group", { name: "会话工具" })).getByText("压缩中…")).toBeTruthy();
});

test("fork: the picker lists candidates, picking one prefills the composer and re-reads", async () => {
  const user = userEvent.setup();
  kernel.messages = [
    msg("user", "第一个问题"),
    msg("assistant", [{ type: "text", text: "第一个回答" }]),
    msg("user", "第二个问题"),
    msg("assistant", [{ type: "text", text: "第二个回答" }]),
  ];
  kernel.forkCandidates = [
    { entryId: "e1", text: "第一个问题" },
    { entryId: "e3", text: "第二个问题" },
  ];
  kernel.forkAnswer = { leafId: null, editorText: "第一个问题" };
  kernel.forkKeeps = [];
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();
  await waitFor(() => expect(screen.getByText("第二个问题")).toBeTruthy());

  await user.click(screen.getByText("从消息分叉"));
  const dialog = await screen.findByRole("dialog", { name: "选择分叉点" });
  // The list is the kernel's candidates, in its order.
  expect(within(dialog).getByText("第一个问题")).toBeTruthy();
  expect(within(dialog).getByText("第二个问题")).toBeTruthy();

  await user.click(within(dialog).getByText("第一个问题"));
  await waitFor(() => expect(kernel.callOf("fork").some((command) => command.entryId === "e1")).toBe(true));

  // The panel closes and the picked text is back in the editor, editable.
  expect(screen.queryByRole("dialog")).toBeNull();
  await waitFor(() => {
    const editor = screen.getByPlaceholderText(/说点什么/) as HTMLTextAreaElement;
    expect(editor.value).toBe("第一个问题");
  });

  // The abandoned suffix is gone: the fork moved the leaf to the root and
  // the re-read shows an empty conversation.
  await waitFor(() => expect(screen.queryByText("第一个回答")).toBeNull());
  expect(screen.queryByText("第二个问题")).toBeNull();
});

test("fork: a non-user target moves the leaf without touching the composer", async () => {
  const user = userEvent.setup();
  kernel.messages = [
    msg("user", "第一个问题"),
    msg("assistant", [{ type: "text", text: "第一个回答" }]),
    msg("user", "第二个问题"),
  ];
  kernel.forkCandidates = [{ entryId: "e1", text: "第一个问题" }];
  // Forking the assistant's entry keeps the first exchange as the context.
  kernel.forkAnswer = { leafId: "e2" };
  kernel.forkKeeps = kernel.messages.slice(0, 2);
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();
  await waitFor(() => expect(screen.getByText("第二个问题")).toBeTruthy());

  await user.click(screen.getByText("从消息分叉"));
  const dialog = await screen.findByRole("dialog", { name: "选择分叉点" });
  await user.click(within(dialog).getByText("第一个问题"));
  await waitFor(() => expect(kernel.callOf("fork").some((command) => command.entryId === "e1")).toBe(true));

  // No editorText: the composer keeps whatever it had (empty here).
  const editor = screen.getByPlaceholderText(/说点什么/) as HTMLTextAreaElement;
  expect(editor.value).toBe("");
  // The surviving prefix stays on screen; the forked-away tail does not.
  expect(screen.getByText("第一个回答")).toBeTruthy();
  await waitFor(() => expect(screen.queryByText("第二个问题")).toBeNull());
});

test("fork: a session with nothing to fork says so, and cancel closes the picker", async () => {
  const user = userEvent.setup();
  kernel.forkCandidates = [];
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.click(screen.getByText("从消息分叉"));
  const dialog = await screen.findByRole("dialog", { name: "选择分叉点" });
  expect(within(dialog).getByText("这个会话还没有可以分叉的消息。")).toBeTruthy();

  await user.click(within(dialog).getByText("取消"));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(kernel.called("fork")).toBe(false);
});

// -- custom models (custom-model-plan.md P4) ------------------------------------

test("custom model: the dialog derives, submits, and the picker gains the model", async () => {
  const user = userEvent.setup();
  // The kernel's fresh catalog: the two built-ins plus the added one.
  kernel.addAnswer = {
    models: [
      kernel.state.model!,
      { id: "deepseek-reasoner", name: "DeepSeek Reasoner", api: "anthropic-messages", provider: "dsanthropic", reasoning: true, contextWindow: 128000 },
      { id: "example-chat", name: "Example Chat", api: "openai-completions", provider: "api-example-com", baseUrl: "https://api.example.com/v1", contextWindow: 128000, maxTokens: 8192 },
    ],
    providers: [
      { id: "api-example-com", baseUrl: "https://api.example.com/v1", api: "openai-completions", modelIds: ["example-chat"] },
    ],
  };
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.click(screen.getByRole("button", { name: "自定义" }));
  const dialog = await screen.findByRole("dialog", { name: "自定义模型" });

  await user.type(within(dialog).getByLabelText("接口地址"), "https://api.example.com/v1");
  await user.type(within(dialog).getByLabelText("密钥"), "sk-example");
  await user.type(within(dialog).getByLabelText("模型 id"), "example-chat");
  // The protocol select opens on the inferred dialect; the limits are
  // prefilled with the dropdown defaults; no thinking level is ticked, so
  // the model is declared non-reasoning unless the user says otherwise.
  expect((within(dialog).getByLabelText("协议") as HTMLSelectElement).value).toBe("openai-completions");
  expect((within(dialog).getByLabelText("上下文窗口") as HTMLSelectElement).value).toBe("131072");
  expect((within(dialog).getByLabelText("最大输出") as HTMLSelectElement).value).toBe("32768");
  for (const level of ["低", "中", "高"]) {
    expect((within(dialog).getByLabelText(level) as HTMLInputElement).checked).toBe(false);
  }
  // Tick two levels and widen the window: 高 alone would NOT imply 中.
  await user.click(within(dialog).getByLabelText("低"));
  await user.click(within(dialog).getByLabelText("高"));
  await user.selectOptions(within(dialog).getByLabelText("上下文窗口"), "262144");
  // The derivation is shown, not hidden: the provider field's placeholder
  // is the id the command will carry.
  expect((within(dialog).getByLabelText("provider id（选填）") as HTMLInputElement).placeholder).toBe("api-example-com");

  await user.click(within(dialog).getByText("保存并添加"));

  await waitFor(() => expect(kernel.callOf("add_custom_model").length).toBe(1));
  const call = kernel.callOf("add_custom_model")[0];
  expect(call.provider).toBe("api-example-com"); // derived and explicit, never empty
  expect(call.baseUrl).toBe("https://api.example.com/v1");
  expect(call.apiKey).toBe("sk-example");
  expect(call.api).toBe("openai-completions");
  expect(call.modelId).toBe("example-chat");
  expect(call.modelName).toBe("example-chat"); // the display name falls back to the id
  expect(call.thinkingLevels).toEqual(["low", "high"]); // a set: 高 did not drag 中 along
  expect(call.contextWindow).toBe(262144);
  expect(call.maxTokens).toBe(32768);

  // The dialog closes and the picker offers the new model — patched from
  // the kernel's fresh catalog, no restart and no second round trip (D4).
  expect(screen.queryByRole("dialog")).toBeNull();
  await waitFor(() => {
    const select = screen.getByLabelText("模型") as HTMLSelectElement;
    expect([...select.options].some((option) => option.value === "api-example-com/example-chat")).toBe(true);
  });
});

test("custom model: re-saving the model in use refreshes the 思考 levels", async () => {
  const user = userEvent.setup();
  // The model in use is non-reasoning, so the top bar offers only 关. The
  // dialog then re-saves THAT model with 中+高 ticked: no set_model runs, so
  // the level list has to be re-read on its own. This is the regression —
  // 思考 kept showing 关 after a save that clearly worked.
  kernel.thinkingAnswer = ["off"];
  kernel.addAnswer = { models: [kernel.state.model!], providers: [] };
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();
  expect([...(screen.getByLabelText("思考") as HTMLSelectElement).options].map((o) => o.value)).toEqual(["off"]);

  await user.click(screen.getByRole("button", { name: "自定义" }));
  const dialog = await screen.findByRole("dialog", { name: "自定义模型" });
  await user.type(within(dialog).getByLabelText("接口地址"), "https://api.example.com/v1");
  await user.type(within(dialog).getByLabelText("密钥"), "sk-example");
  await user.type(within(dialog).getByLabelText("模型 id"), "deepseek-chat");
  await user.click(within(dialog).getByLabelText("中"));
  await user.click(within(dialog).getByLabelText("高"));
  await user.click(within(dialog).getByText("保存并添加"));

  await waitFor(() => expect(kernel.callOf("add_custom_model").length).toBe(1));
  expect(kernel.called("set_model")).toBe(false); // no switch to hang a refetch on
  await waitFor(() => {
    const select = screen.getByLabelText("思考") as HTMLSelectElement;
    expect([...select.options].map((option) => option.value)).toEqual(["off", "medium", "high"]);
  });
});

test("custom model: un-ticking the level the session runs asks for a clamp", async () => {
  const user = userEvent.setup();
  // The session runs 高 and the user re-saves the model with nothing ticked,
  // so the model now supports only 关. A <select> cannot display a value that
  // is not in its options, so the store asks the kernel to clamp
  // (set_thinking_level clamps rather than rejects) and re-reads the state.
  kernel.state = { ...kernel.state, thinkingLevel: "high" };
  kernel.thinkingAnswer = ["off", "low", "high"];
  kernel.addAnswer = { models: [kernel.state.model!], providers: [] };
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.click(screen.getByRole("button", { name: "自定义" }));
  const dialog = await screen.findByRole("dialog", { name: "自定义模型" });
  await user.type(within(dialog).getByLabelText("接口地址"), "https://api.example.com/v1");
  await user.type(within(dialog).getByLabelText("密钥"), "sk-example");
  await user.type(within(dialog).getByLabelText("模型 id"), "deepseek-chat");
  await user.click(within(dialog).getByText("保存并添加"));

  await waitFor(() =>
    expect(kernel.callOf("set_thinking_level").some((command) => command.level === "high")).toBe(true),
  );
  await waitFor(() => {
    expect((screen.getByLabelText("思考") as HTMLSelectElement).value).toBe("off");
  });
});

test("custom model: local validation refuses before any round trip", async () => {
  const user = userEvent.setup();
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.click(screen.getByRole("button", { name: "自定义" }));
  const dialog = await screen.findByRole("dialog", { name: "自定义模型" });

  // An empty form: the required fields complain, the kernel is not called.
  await user.click(within(dialog).getByText("保存并添加"));
  expect(within(dialog).getByText("接口地址必填")).toBeTruthy();
  expect(within(dialog).getByText("密钥必填（本地无鉴权端点可填任意占位符）")).toBeTruthy();
  expect(within(dialog).getByText("模型 id 必填")).toBeTruthy();
  expect(kernel.called("add_custom_model")).toBe(false);

  // A non-http endpoint is refused; so is the one illegal pair the two
  // dropdowns can still express — an answer cap wider than the window the
  // request has to fit in (plan D7). A zero window is now unreachable.
  await user.type(within(dialog).getByLabelText("接口地址"), "ftp://api.example.com");
  await user.selectOptions(within(dialog).getByLabelText("上下文窗口"), "32768");
  await user.selectOptions(within(dialog).getByLabelText("最大输出"), "1048576");
  await user.click(within(dialog).getByText("保存并添加"));
  expect(within(dialog).getByText("接口地址要是一个 http(s) 地址，且带主机名")).toBeTruthy();
  expect(within(dialog).getByText("最大输出不能大于上下文窗口")).toBeTruthy();
  expect(kernel.called("add_custom_model")).toBe(false);
});

test("custom model: a kernel refusal lands in the dialog's error bar and keeps it open", async () => {
  const user = userEvent.setup();
  kernel.addError = "write auth.json: Access is denied";
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.click(screen.getByRole("button", { name: "自定义" }));
  const dialog = await screen.findByRole("dialog", { name: "自定义模型" });
  await user.type(within(dialog).getByLabelText("接口地址"), "https://api.example.com/v1");
  await user.type(within(dialog).getByLabelText("密钥"), "sk-example");
  await user.type(within(dialog).getByLabelText("模型 id"), "example-chat");
  await user.click(within(dialog).getByText("保存并添加"));

  // The kernel's message, verbatim, in the bar; the form stays so the
  // user can fix and retry.
  expect(await within(dialog).findByText("write auth.json: Access is denied")).toBeTruthy();
  expect(screen.queryByRole("dialog")).toBeTruthy();
});

test("custom model: an existing provider id gets the overwrite warning", async () => {
  const user = userEvent.setup();
  render(<App store={store} bridge={kernel.bridge} />);
  await kernel.load();

  await user.click(screen.getByRole("button", { name: "自定义" }));
  const dialog = await screen.findByRole("dialog", { name: "自定义模型" });
  // The stub catalog's provider is dsanthropic; typing that id explicitly
  // trips the warning (R2 — the merge rule would overwrite silently).
  await user.type(within(dialog).getByLabelText("接口地址"), "https://api.example.com/v1");
  await user.type(within(dialog).getByLabelText("provider id（选填）"), "dsanthropic");
  expect(
    await within(dialog).findByText("已存在同名 provider，保存将覆盖它的声明（密钥另行管理，不受影响）。"),
  ).toBeTruthy();
});
