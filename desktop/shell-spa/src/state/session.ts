// session.ts — the page's single store: the bridge's events and responses
// reduced into one immutable snapshot React subscribes to. The shape of the
// layering is Reasonix's state/ + port/ split, cut to the 18-command face:
//
//   port/bridge.ts    the wire (injected, so this file tests without a DOM)
//   state/transcript  the pure event reducer (its own tests)
//   state/session     this file: orchestration, commands, lifecycle
//
// # Lifecycle
//
// The shell sends session_loaded once the page has loaded (main.ts's
// did-finish-load), and every kernel event that arrived before that waits in
// the shell's buffer and is flushed first. So:
//
//   1. events before session_loaded  → queued, not applied
//   2. session_loaded                → get_messages + catalogs in parallel
//   3. snapshot lands                → transcript = snapshot, then the queue
//                                     drains through the reducer in replay
//                                     mode (transcript.ts explains the dedup)
//   4. everything after              → applied live
//
// # Polling
//
// isStreaming is derived from events (message_start / agent_end), which is
// immediate. pendingMessageCount has no event of its own — the queue moves
// when the loop drains it — so while a run is in flight the store asks
// get_state every two seconds. Two seconds is the shell's own cadence for
// the same number (powerguard.ts), and it is the only poll there is.

import {
  CmdAbort,
  CmdAddCustomModel,
  CmdClearQueue,
  CmdCompact,
  CmdFork,
  CmdGetAvailableModels,
  CmdGetAvailableThinking,
  CmdGetForkMessages,
  CmdGetMCPStatus,
  CmdGetMessages,
  CmdGetState,
  CmdListSessions,
  CmdNewSession,
  CmdPrompt,
  CmdResumeSession,
  CmdSetModel,
  CmdSetSessionName,
  CmdSetThinkingLevel,
} from "../port/commands.ts";
import { callData, callVoid, type UltracBridge } from "../port/bridge.ts";
import {
  applyEvent,
  emptyTranscript,
  fromMessages,
  type TranscriptState,
} from "./transcript.ts";
import type {
  CompactResult,
  CustomModelResult,
  CustomModelSpec,
  ForkMessage,
  ForkResult,
  McpServerStatus,
  McpStatusResult,
  RpcEvent,
  RpcModel,
  RpcSessionState,
  SessionSummary,
  WireMessage,
} from "./wire.ts";

const POLL_MS = 2000;

/** Which queue a message joins while a run is active (server.go cmdPrompt). */
export type SendBehavior = "steer" | "followUp";

export interface SessionSnapshot {
  /** session_loaded received and the initial replay drained. */
  ready: boolean;
  /** Why the page could not start, when it could not. */
  bootError: string | null;
  /** The last action's error (a failed send, rename, model switch…). */
  actionError: string | null;
  /** get_state's data. */
  state: RpcSessionState | null;
  transcript: TranscriptState;
  models: RpcModel[];
  thinkingLevels: string[];
  sessions: SessionSummary[];
  /** get_mcp_status's inventory (mcp-plan.md D6). Empty means none. */
  mcpServers: McpServerStatus[];
}

export class SessionStore {
  readonly #bridge: UltracBridge;
  readonly #listeners = new Set<() => void>();
  #snapshot: SessionSnapshot = {
    ready: false,
    bootError: null,
    actionError: null,
    state: null,
    transcript: emptyTranscript(),
    models: [],
    thinkingLevels: [],
    sessions: [],
    mcpServers: [],
  };
  /** Events seen before the snapshot (and while it was in flight). */
  #queue: RpcEvent[] = [];
  #replaying = false;
  #streaming = false;
  #poll: ReturnType<typeof setInterval> | null = null;

  constructor(bridge: UltracBridge) {
    this.#bridge = bridge;
    bridge.onEvent((event) => this.#onEvent(event));
    // The shell sends session_loaded exactly once, right after the buffered
    // events. A shell that never sends it leaves the page on its boot screen
    // — the store's catch is for a store-level failure, not that case.
    bridge.onSessionLoaded((state) => {
      this.#onSessionLoaded(state).catch((err: unknown) => {
        this.#patch({ ready: true, bootError: messageOf(err) });
      });
    });
  }

  // -- React subscription --------------------------------------------------

  subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  };

  getSnapshot = (): SessionSnapshot => this.#snapshot;

  dispose(): void {
    this.#stopPoll();
    this.#listeners.clear();
  }

  // -- events ---------------------------------------------------------------

  #onEvent(event: RpcEvent): void {
    if (!this.#snapshot.ready || this.#replaying) {
      this.#queue.push(event);
      return;
    }
    this.#ingest(event, false);
  }

  /** Apply one event to the snapshot (live, or replay after the snapshot). */
  #ingest(event: RpcEvent, replay: boolean): void {
    const transcript = applyEvent(this.#snapshot.transcript, event, replay);
    const streaming = streamingAfter(event, this.#streaming);
    this.#patch({ transcript });
    if (streaming !== this.#streaming) {
      this.#streaming = streaming;
      if (streaming) this.#startPoll();
      else this.#stopPoll();
    }
    if (isMcpToolEnd(event)) void this.refreshMCPStatus();
  }

  async #onSessionLoaded(state: RpcSessionState): Promise<void> {
    this.#patch({ state });
    this.#replaying = true;
    let transcript = this.#snapshot.transcript;
    let models = this.#snapshot.models;
    let levels = this.#snapshot.thinkingLevels;
    let bootError: string | null = null;
    try {
      const [messages, fetchedModels, fetchedLevels] = await Promise.all([
        callData<{ messages: WireMessage[] }>(this.#bridge, { type: CmdGetMessages }).then(
          (data) => data.messages ?? [],
          () => [] as WireMessage[],
        ),
        callData<{ models: RpcModel[] }>(this.#bridge, { type: CmdGetAvailableModels }).then(
          (data) => data.models ?? [],
          () => [] as RpcModel[],
        ),
        callData<{ levels: string[] }>(this.#bridge, { type: CmdGetAvailableThinking }).then(
          (data) => data.levels ?? [],
          () => [] as string[],
        ),
      ]);
      transcript = fromMessages(messages);
      models = fetchedModels;
      levels = fetchedLevels;
    } catch (err) {
      // The catalogs each fall back to empty above; this is for the
      // unexpected. The page still opens — an empty transcript with an
      // explanation beats a boot screen that never leaves.
      bootError = messageOf(err);
    }
    // Open the live path first, then drain what queued meanwhile. The order
    // matters: an event arriving between the snapshot landing and the flip
    // would otherwise sit in the queue with nothing left to drain it.
    this.#replaying = false;
    this.#patch({ transcript, models, thinkingLevels: levels, ready: true, bootError });
    const pending = this.#queue;
    this.#queue = [];
    // Replay mode: dedup against the snapshot (transcript.ts carries the
    // reasoning for why buffered echoes must not double as rows).
    for (const event of pending) this.#ingest(event, true);
    void this.refreshSessions();
  }

  // -- commands -------------------------------------------------------------

  /** Send a prompt. While a run is active the message must name its queue. */
  async send(text: string, behavior?: SendBehavior): Promise<void> {
    const command = behavior
      ? { type: CmdPrompt, message: text, streamingBehavior: behavior }
      : { type: CmdPrompt, message: text };
    await this.#act(async () => {
      await callData<{ disposition: string }>(this.#bridge, command);
    });
    void this.#refreshState();
  }

  async abort(): Promise<void> {
    await this.#act(() => callVoid(this.#bridge, { type: CmdAbort }));
    void this.#refreshState();
  }

  async clearQueue(): Promise<void> {
    await this.#act(() => callVoid(this.#bridge, { type: CmdClearQueue }));
    void this.#refreshState();
  }

  /** New session: the kernel aborts any run and swaps before answering. */
  async newSession(): Promise<void> {
    await this.#act(() => callVoid(this.#bridge, { type: CmdNewSession }));
    this.#patch({ transcript: emptyTranscript() });
    await this.#refreshState();
    void this.refreshSessions();
  }

  async resumeSession(sessionPath: string): Promise<void> {
    await this.#act(() => callVoid(this.#bridge, { type: CmdResumeSession, sessionPath }));
    await this.#reloadTranscript();
    await this.#refreshState();
  }

  async renameSession(name: string): Promise<void> {
    await this.#act(() => callVoid(this.#bridge, { type: CmdSetSessionName, name }));
    await this.#refreshState();
    void this.refreshSessions();
  }

  async setModel(provider: string, modelId: string): Promise<void> {
    await this.#act(async () => {
      const model = await callData<RpcModel>(this.#bridge, { type: CmdSetModel, provider, modelId });
      this.#patch({ state: this.#snapshot.state ? { ...this.#snapshot.state, model } : this.#snapshot.state });
    });
    // The model decides which thinking levels exist (ai.ClampThinkingLevel),
    // so the level list is refetched with it.
    await this.#refreshCatalogs();
  }

  async setThinkingLevel(level: string): Promise<void> {
    await this.#act(() => callVoid(this.#bridge, { type: CmdSetThinkingLevel, level }));
    await this.#refreshState();
  }

  /**
   * Compact the conversation now (P-C2's manual path). The kernel aborts
   * any active run first, so this is safe to call mid-stream. The
   * transcript was rewritten by the compaction — a checkpoint summary
   * replaces the old prefix — so it is re-read exactly like a resume.
   * Returns the result, or null when the call failed (the error banner
   * carries the kernel's message, including the idempotence refusals
   * "Already compacted" / "Nothing to compact (session too small)").
   */
  async compact(): Promise<CompactResult | null> {
    return this.#act(async () => {
      const result = await callData<CompactResult>(this.#bridge, { type: CmdCompact });
      await this.#reloadTranscript();
      await this.#refreshState();
      return result;
    });
  }

  /**
   * The fork selector's candidates: every user message with text, in file
   * order (get_fork_messages). An empty list is a session with nothing to
   * fork from — the caller renders that, it is not an error.
   */
  async forkMessages(): Promise<ForkMessage[]> {
    const messages = await this.#act(async () => {
      const data = await callData<{ messages: ForkMessage[] }>(this.#bridge, { type: CmdGetForkMessages });
      return data.messages ?? [];
    });
    return messages ?? [];
  }

  /**
   * Fork from one entry (plan D7: the leaf moves, the agent is re-seeded
   * from the fork point — the kernel's ReplaceMessages). The transcript
   * the page shows must be re-read for the same reason a resume re-reads
   * it: the abandoned history is gone from the live context.
   *
   * Returns the kernel's answer, whose editorText (a user-message target)
   * is what the composer puts back for editing. null when the call
   * failed.
   */
  async fork(entryId: string): Promise<ForkResult | null> {
    return this.#act(async () => {
      const result = await callData<ForkResult>(this.#bridge, { type: CmdFork, entryId });
      await this.#reloadTranscript();
      await this.#refreshState();
      return result;
    });
  }

  /**
   * Add a custom model (custom-model-plan.md D3): the kernel writes the
   * credential and the declaration to settings' providers — at
   * the user layer, which is cwd-independent (D8) — reloads, and answers
   * with the fresh catalog.
   *
   * The response is the whole merged catalog, so the picker's source of
   * truth becomes the kernel's in one patch: the new model is selectable
   * immediately, without a restart and without a second round trip (D4).
   *
   * The LEVEL list is a second matter. It is the kernel's answer about the
   * model currently in use (get_available_thinking_levels →
   * ai.ClampThinkingLevel), and the dialog can save exactly that model: a
   * re-save with 低/中/高 ticked changes what it supports while no
   * set_model ever runs, so nothing else would refetch. Patching the
   * catalog alone left 思考 showing only 关 after the user ticked 中+高 on
   * the running model (custom-model-thinking-limits-plan.md R4 was wrong
   * about this — "the added model has not been selected yet" does not hold
   * for a re-save).
   *
   * null when the kernel refused (its message is in actionError, which
   * the dialog shows as its error bar while it is open).
   */
  async addCustomModel(spec: CustomModelSpec): Promise<CustomModelResult | null> {
    return this.#act(async () => {
      const result = await callData<CustomModelResult>(this.#bridge, {
        type: CmdAddCustomModel,
        ...spec,
      });
      this.#patch({ models: result.models ?? [] });
      await this.#refreshThinkingLevels();
      // …and the live session may still sit on a level the model no longer
      // supports (un-tick 高 while the session runs 高). A <select> cannot
      // display a value outside its options, so ask the kernel to clamp:
      // set_thinking_level is the command that clamps rather than rejects
      // (server.go's cmdSetThinkingLevel).
      const level = this.#snapshot.state?.thinkingLevel;
      if (level && !this.#snapshot.thinkingLevels.includes(level)) {
        await callVoid(this.#bridge, { type: CmdSetThinkingLevel, level });
        await this.#refreshState();
      }
      return result;
    });
  }

  /**
   * Read the MCP inventory (mcp-plan.md D6).
   *
   * This is the whole "keep the panel current" strategy, and it is a pull
   * rather than a push for a concrete reason: the boot strategy collapses
   * the window in which a server's state can change. A boot with a cache
   * leaves a server `cached` and nothing touches it until a tool is called;
   * a boot without one blocks until the server is `ready` or `failed`. So
   * the only moment the state can move spur is the first call to one of its
   * tools — and that is the moment #ingest pulls (see isMcpToolEnd).
   *
   * The panel pulls once when it opens, for the same reason there is no
   * timer: an open panel that never sees a tool call has nothing new to
   * show.
   *
   * A failed read is silent. The panel renders whatever it last had — an
   * empty list renders as "no MCP servers", which is true on most machines
   * and is not worth an error banner.
   */
  async refreshMCPStatus(): Promise<void> {
    try {
      const data = await callData<McpStatusResult>(this.#bridge, { type: CmdGetMCPStatus });
      this.#patch({ mcpServers: data.servers ?? [] });
    } catch {
      // The next open, or the next MCP tool call, tries again.
    }
  }

  async refreshSessions(): Promise<void> {
    try {
      const data = await callData<{ sessions: SessionSummary[] }>(this.#bridge, { type: CmdListSessions });
      this.#patch({ sessions: data.sessions ?? [] });
    } catch {
      // The sidebar is a convenience; a failed listing must not disturb the
      // conversation. The next refresh tries again.
    }
  }

  // -- internals ------------------------------------------------------------

  /**
   * Run one action, recording its failure for the error banner, and
   * handing back whatever it produced (null on failure). The commands
   * whose data the page acts on — compact, fork — return it; the rest
   * ignore it.
   */
  async #act<T>(run: () => Promise<T>): Promise<T | null> {
    this.#patch({ actionError: null });
    try {
      return await run();
    } catch (err) {
      this.#patch({ actionError: messageOf(err) });
      return null;
    }
  }

  /**
   * Re-read the live conversation into the transcript. After a session
   * swap, a compaction or a fork the kernel's context is not what the
   * page last saw, and get_messages is the only authority for it.
   */
  async #reloadTranscript(): Promise<void> {
    try {
      const messages = await callData<{ messages: WireMessage[] }>(this.#bridge, { type: CmdGetMessages });
      this.#patch({ transcript: fromMessages(messages.messages ?? []) });
    } catch (err) {
      this.#patch({ actionError: messageOf(err) });
    }
  }

  async #refreshState(): Promise<void> {
    try {
      const state = await callData<RpcSessionState>(this.#bridge, { type: CmdGetState });
      this.#patch({ state });
    } catch {
      // A refresh that fails is one the next poll (or event) corrects.
    }
  }

  async #refreshCatalogs(): Promise<void> {
    try {
      const [models, levels] = await Promise.all([
        callData<{ models: RpcModel[] }>(this.#bridge, { type: CmdGetAvailableModels }),
        callData<{ levels: string[] }>(this.#bridge, { type: CmdGetAvailableThinking }),
      ]);
      this.#patch({ models: models.models ?? [], thinkingLevels: levels.levels ?? [] });
    } catch {
      // ditto
    }
  }

  /**
   * Re-read the levels the model in use supports. Separate from
   * #refreshCatalogs because add_custom_model is the one flow that can
   * change this list while the model stays put — the catalog there comes
   * from the command's own response, so only the levels need asking.
   */
  async #refreshThinkingLevels(): Promise<void> {
    try {
      const data = await callData<{ levels: string[] }>(this.#bridge, { type: CmdGetAvailableThinking });
      this.#patch({ thinkingLevels: data.levels ?? [] });
    } catch {
      // A failed read is one the next model switch corrects.
    }
  }

  #startPoll(): void {
    if (this.#poll) return;
    this.#poll = setInterval(() => void this.#refreshState(), POLL_MS);
  }

  #stopPoll(): void {
    if (this.#poll) clearInterval(this.#poll);
    this.#poll = null;
  }

  #patch(partial: Partial<SessionSnapshot>): void {
    this.#snapshot = { ...this.#snapshot, ...partial };
    for (const listener of this.#listeners) listener();
  }
}

/**
 * Whether an event is the end of an MCP tool call — the one moment a
 * server's state can have moved (mcp-plan.md §3.7).
 *
 * The name test is the `mcp__` prefix, which is the pool's naming and the
 * ecosystem's: a tool whose name starts with it came from a server. The
 * server name itself is never parsed out — the panel does not need it, and
 * a name with a sanitised-hash suffix is not something to take apart in a
 * string comparison.
 */
function isMcpToolEnd(event: RpcEvent): boolean {
  if (event.type !== "tool_execution_end") return false;
  const name = event.toolName;
  return typeof name === "string" && name.startsWith("mcp__");
}

/** The streaming flag the event stream implies (hostclient.ts's rule). */
function streamingAfter(event: RpcEvent, current: boolean): boolean {
  switch (event.type) {
    case "message_start":
      return true;
    case "agent_end":
    case "agent_settled":
      return false;
    default:
      return current;
  }
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
