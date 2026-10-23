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
//   2. session_loaded                → get_entries + catalogs in parallel
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
  CmdClearQueue,
  CmdCompact,
  CmdFork,
  CmdGetAvailableModels,
  CmdGetAvailableThinking,
  CmdGetEntries,
  CmdGetForkMessages,
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
import { historyRows } from "./history.ts";
import {
  applyEvent,
  emptyTranscript,
  fromHistory,
  fromMessages,
  type TranscriptState,
} from "./transcript.ts";
import type {
  CompactResult,
  ForkMessage,
  ForkResult,
  GetEntriesData,
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
  }

  async #onSessionLoaded(state: RpcSessionState): Promise<void> {
    this.#patch({ state });
    this.#replaying = true;
    let transcript = this.#snapshot.transcript;
    let models = this.#snapshot.models;
    let levels = this.#snapshot.thinkingLevels;
    let bootError: string | null = null;
    try {
      const [history, fetchedModels, fetchedLevels] = await Promise.all([
        this.#readTranscript(),
        callData<{ models: RpcModel[] }>(this.#bridge, { type: CmdGetAvailableModels }).then(
          (data) => data.models ?? [],
          () => [] as RpcModel[],
        ),
        callData<{ levels: string[] }>(this.#bridge, { type: CmdGetAvailableThinking }).then(
          (data) => data.levels ?? [],
          () => [] as string[],
        ),
      ]);
      transcript = history;
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
   * transcript is re-read from the session file afterwards: the file gained
   * a compaction entry at the cut, which the page renders as a boundary
   * marker while keeping every turn before it (the model's context lost
   * those turns — the page must not).
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
   * it: the leaf moved, so the branch the file now describes is a shorter
   * one — its walk from the new leaf simply stops before the abandoned
   * suffix.
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
   * swap, a compaction or a fork the leaf the file points at is not where
   * the page last was, and the session file is the only authority for what
   * the conversation contains.
   */
  async #reloadTranscript(): Promise<void> {
    try {
      this.#patch({ transcript: await this.#readTranscript() });
    } catch (err) {
      this.#patch({ actionError: messageOf(err) });
    }
  }

  /**
   * The transcript the session file describes: every message on the current
   * branch, with compaction and branch summaries as notice rows.
   *
   * Deliberately not get_messages. That command answers with the model's
   * request, and a compaction rewrites it: the summary takes a user
   * message's place and the summarized turns are gone, so a page built on
   * it shows the summary in the user's bubble and cannot show the earlier
   * conversation at all (state/history.ts tells the whole story).
   * get_messages stays as the fallback for a kernel that cannot serve
   * get_entries — a degraded history beats no page.
   */
  async #readTranscript(): Promise<TranscriptState> {
    try {
      const data = await callData<GetEntriesData>(this.#bridge, { type: CmdGetEntries });
      return fromHistory(historyRows(data.entries ?? [], data.leafId ?? null));
    } catch {
      const data = await callData<{ messages: WireMessage[] }>(this.#bridge, { type: CmdGetMessages });
      return fromMessages(data.messages ?? []);
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
