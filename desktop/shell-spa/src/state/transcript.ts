// transcript.ts — the event-to-view-model reducer: the heart of the page.
// Pure functions over plain data, no React, no DOM — that is what makes the
// streaming behaviour testable under node:test (test/transcript.test.ts)
// without a renderer.
//
// # The model
//
// The kernel speaks in events (internal/transport/pi_json.go); the page
// speaks in rows. This file is the whole translation:
//
//   message_start / message_end      one row per message (system rows are
//                                    dropped: they are prompt sections, not
//                                    conversation)
//   message_update                   the typewriter: text/thinking deltas
//                                    build the streaming assistant row,
//                                    *_end events replace blocks with the
//                                    authoritative content
//   tool_execution_start/update/end  the live status of a tool card, keyed
//                                    by toolCallId
//   toolResult messages              the card's output, routed into the same
//                                    toolCallId instead of a row of its own
//
// # Why replay mode exists
//
// The shell buffers kernel events until the page has loaded, then flushes
// them and sends session_loaded (main.ts did-finish-load). So a page that
// loads mid-run receives the run's early events BEFORE the get_messages
// snapshot that already contains the completed messages among them.
// Replaying those events naively would duplicate rows. applyEvent's replay
// flag is the dedup: an event is skipped when its message is already in the
// snapshot, kept when it describes the still-streaming message (which the
// snapshot never contains — state.messages only gains a message at
// message_end, internal/agent/agent.go processEvents).
//
// Identity is role+timestamp: ai.Message has no id, and TimestampMS is the
// closest thing to one.

import type {
  AssistantMessageEvent,
  ContentBlock,
  RpcEvent,
  ToolResultWire,
  Usage,
  WireMessage,
} from "./wire.ts";

// ---------------------------------------------------------------------------
// View model
// ---------------------------------------------------------------------------

export type BlockView =
  | { kind: "text"; text: string }
  | { kind: "thinking"; text: string }
  | { kind: "image"; data: string; mimeType: string }
  | { kind: "toolCall"; id: string; name: string; args: Record<string, unknown>; rawArgs: string };

export interface TranscriptMessage {
  /** Stable React key. Messages have no wire id; this is synthesised. */
  key: string;
  role: "user" | "assistant" | "toolResult";
  blocks: BlockView[];
  /** True while deltas are still arriving for this row. */
  streaming: boolean;
  errorMessage?: string;
  stopReason?: string;
  usage?: Usage;
  timestamp: number;
  /** toolResult-only fields. */
  toolCallId?: string;
  toolName?: string;
  isError?: boolean;
}

export interface ToolRun {
  id: string;
  name: string;
  status: "running" | "done";
  /** Text seen so far: partialResult while running, result once done. */
  text: string;
  isError: boolean;
  durationMs?: number;
  /** True once the toolResult message has arrived (the authoritative output). */
  hasResult: boolean;
}

export interface TranscriptState {
  messages: TranscriptMessage[];
  /** Live tool status keyed by toolCallId; drives the cards' spinners. */
  toolRuns: Record<string, ToolRun>;
  /** The key of the assistant row currently receiving deltas. */
  streamingKey: string | null;
  /**
   * Rows a live message_start opened, waiting for their message_end —
   * `role:timestamp` to a FIFO of row keys. The user message is the case:
   * it never streams (so streamingKey cannot close it), and the kernel
   * always follows its start with its end. A queue rather than a single
   * key because two prompts can share a millisecond.
   */
  awaitingEnd: Record<string, string[]>;
  /** The latest run-level error message, if the last run failed. */
  runError: string | null;
  /** Monotonic counter behind synthesised keys. */
  seq: number;
}

export function emptyTranscript(): TranscriptState {
  return { messages: [], toolRuns: {}, streamingKey: null, awaitingEnd: {}, runError: null, seq: 1 };
}

// ---------------------------------------------------------------------------
// Event application
// ---------------------------------------------------------------------------

/**
 * Apply one wire event. `replay` marks the post-snapshot drain of buffered
 * events, where dedup against already-present messages is required.
 */
export function applyEvent(state: TranscriptState, event: RpcEvent, replay = false): TranscriptState {
  switch (event.type) {
    case "message_start":
      return onMessageStart(state, asMessage(event.message), replay);
    case "message_update":
      return onMessageUpdate(state, event);
    case "message_end":
      return onMessageEnd(state, asMessage(event.message), replay);
    case "agent_end":
      // The run is over, so no further deltas can arrive for the open row.
      // The kernel always emits message_end first; this is the defensive
      // close for a page that somehow missed it (and for agent_end's own
      // message list, which carries the authoritative final message).
      return closeStreaming(state, asMessages(event.messages));
    case "agent_settled":
      return closeStreaming(state);
    case "tool_execution_start":
      return onToolStart(state, event);
    case "tool_execution_update":
      return onToolUpdate(state, event);
    case "tool_execution_end":
      return onToolEnd(state, event);
    default:
      // agent_start / turn_start / turn_end / agent_end / agent_settled are
      // session-level signals the store watches; the transcript ignores them.
      return state;
  }
}

/** Build the initial state from a get_messages snapshot. */
export function fromMessages(messages: WireMessage[]): TranscriptState {
  let state = emptyTranscript();
  for (const message of messages) state = absorbMessage(state, message);
  return state;
}

/** True when the row is one the page shows (system rows are internal). */
function visible(role: string): role is "user" | "assistant" | "toolResult" {
  return role === "user" || role === "assistant" || role === "toolResult";
}

/** Absorb one complete message: snapshot path and message_end share it. */
function absorbMessage(state: TranscriptState, message: WireMessage): TranscriptState {
  if (message.role === "toolResult") return absorbToolResult(state, message);
  if (!visible(message.role)) return state;
  const blocks = normalizeContent(message.content);
  const errorMessage = message.errorMessage?.trim() ? message.errorMessage : undefined;
  const row: TranscriptMessage = {
    key: `m${state.seq}`,
    role: message.role,
    blocks,
    streaming: false,
    timestamp: message.timestamp ?? 0,
    ...(errorMessage ? { errorMessage } : {}),
    ...(message.stopReason ? { stopReason: message.stopReason } : {}),
    ...(message.usage ? { usage: message.usage } : {}),
  };
  return {
    ...state,
    messages: [...state.messages, row],
    seq: state.seq + 1,
    ...(errorMessage ? { runError: errorMessage } : {}),
  };
}

function absorbToolResult(state: TranscriptState, message: WireMessage): TranscriptState {
  const id = message.toolCallId ?? "";
  if (!id) return state;
  const text = textOfBlocks(message.content);
  const run: ToolRun = {
    id,
    name: message.toolName ?? "",
    status: "done",
    text,
    isError: Boolean(message.isError),
    hasResult: true,
  };
  return { ...state, toolRuns: { ...state.toolRuns, [id]: run } };
}

/**
 * The run is over: close the open streaming row. agent_end's message list
 * (when it matches the row by timestamp) supplies the authoritative content;
 * otherwise the accumulated deltas stand — the best truth the page has.
 */
function closeStreaming(state: TranscriptState, messages?: WireMessage[]): TranscriptState {
  if (!state.streamingKey) return state;
  const index = state.messages.findIndex((m) => m.key === state.streamingKey);
  if (index < 0) return { ...state, streamingKey: null };
  const row = state.messages[index]!;
  const final = messages
    ?.filter((m) => m.role === "assistant")
    .find((m) => m.timestamp === row.timestamp);
  const next = [...state.messages];
  next[index] = final
    ? {
        ...row,
        blocks: normalizeContent(final.content),
        streaming: false,
        ...(final.errorMessage?.trim() ? { errorMessage: final.errorMessage } : {}),
        ...(final.stopReason ? { stopReason: final.stopReason } : {}),
        ...(final.usage ? { usage: final.usage } : {}),
      }
    : { ...row, streaming: false };
  return { ...state, messages: next, streamingKey: null };
}

function onMessageStart(state: TranscriptState, message: WireMessage, replay: boolean): TranscriptState {
  if (message.role === "toolResult") return absorbToolResult(state, message);
  if (!visible(message.role)) return state;
  if (replay && findByStamp(state, message)) return state; // already in the snapshot
  const streaming = message.role === "assistant";
  const errorMessage = message.errorMessage?.trim() ? message.errorMessage : undefined;
  const row: TranscriptMessage = {
    key: `m${state.seq}`,
    role: message.role,
    blocks: normalizeContent(message.content),
    streaming,
    timestamp: message.timestamp ?? 0,
    ...(errorMessage ? { errorMessage } : {}),
    ...(message.stopReason ? { stopReason: message.stopReason } : {}),
    ...(message.usage ? { usage: message.usage } : {}),
  };
  return {
    ...state,
    messages: [...state.messages, row],
    streamingKey: streaming ? row.key : state.streamingKey,
    seq: state.seq + 1,
    ...(errorMessage ? { runError: errorMessage } : {}),
    ...(streaming
      ? {}
      : // A live, non-streaming row waits for its end here. Replay skips
        // this: snapshot rows are closed by the snapshot itself, and their
        // echoed ends fall through to the replay dedup below.
        { awaitingEnd: pushAwaiting(state.awaitingEnd, message.role, message.timestamp ?? 0, row.key) }),
  };
}

function onMessageEnd(state: TranscriptState, message: WireMessage, replay: boolean): TranscriptState {
  if (message.role === "toolResult") return absorbToolResult(state, message);
  if (!visible(message.role)) return state;
  const streamingIndex = state.streamingKey
    ? state.messages.findIndex((m) => m.key === state.streamingKey && m.role === message.role)
    : -1;
  if (streamingIndex >= 0) {
    // The live row this message closes: replace its blocks with the
    // authoritative content (deltas are a rendering hint, never the truth).
    return finalizeRow(state, streamingIndex, message);
  }
  // A row a live message_start opened — the user message is the case. It is
  // never the streaming row (only assistant messages stream), and the kernel
  // always follows its start with its end: close the row the start opened
  // instead of appending a second one. (The full-chain Electron check
  // caught this doubling: the user bubble rendered twice.)
  const stamp = message.timestamp ?? 0;
  const queue = state.awaitingEnd[`${message.role}:${stamp}`];
  if (queue && queue.length > 0) {
    const key = queue[0]!;
    const index = state.messages.findIndex((m) => m.key === key);
    const shifted = shiftAwaiting(state.awaitingEnd, message.role, stamp);
    const base: TranscriptState = { ...state, awaitingEnd: shifted };
    return index >= 0 ? finalizeRow(base, index, message) : base;
  }
  if (replay) return state; // completed before the snapshot
  return absorbMessage(state, message);
}

/** Replace one row's content with the message's authoritative form. */
function finalizeRow(state: TranscriptState, index: number, message: WireMessage): TranscriptState {
  const messages = [...state.messages];
  const previous = messages[index]!;
  const errorMessage = message.errorMessage?.trim() ? message.errorMessage : undefined;
  messages[index] = {
    ...previous,
    blocks: normalizeContent(message.content),
    streaming: false,
    ...(errorMessage ? { errorMessage } : {}),
    ...(message.stopReason ? { stopReason: message.stopReason } : {}),
    ...(message.usage ? { usage: message.usage } : {}),
  };
  return {
    ...state,
    messages,
    streamingKey: state.streamingKey === previous.key ? null : state.streamingKey,
    ...(errorMessage ? { runError: errorMessage } : {}),
  };
}

function onMessageUpdate(state: TranscriptState, event: RpcEvent): TranscriptState {
  const assistantEvent = event.assistantMessageEvent as AssistantMessageEvent | undefined;
  if (!assistantEvent) return state;
  const index = state.streamingKey ? state.messages.findIndex((m) => m.key === state.streamingKey) : -1;
  if (index < 0) {
    // An update for a message that already completed: its content is final
    // in the snapshot (or in the closed row). Nothing to build.
    return state;
  }
  const messages = [...state.messages];
  const row = messages[index]!;
  const blocks = applyAssistantEvent(row.blocks, assistantEvent);
  if (blocks === row.blocks && assistantEvent.type !== "error") return state;
  if (blocks !== row.blocks) messages[index] = { ...row, blocks };
  const next: TranscriptState = { ...state, messages };
  if (assistantEvent.type === "error") {
    // The stream-level failure: the error variant carries a Message whose
    // errorMessage is the story. (The run-level failure path also emits a
    // message_start/message_end pair for the same message — both land on
    // runError, and the row renders it.)
    const text = assistantEvent.error.errorMessage?.trim();
    if (text) next.runError = text;
  }
  return next;
}

// ---------------------------------------------------------------------------
// Tool execution events
// ---------------------------------------------------------------------------

function onToolStart(state: TranscriptState, event: RpcEvent): TranscriptState {
  const id = str(event.toolCallId);
  if (!id) return state;
  const existing = state.toolRuns[id];
  if (existing?.hasResult) return state; // completed before the snapshot
  const run: ToolRun = {
    id,
    name: str(event.toolName) || existing?.name || "",
    status: "running",
    text: existing?.text ?? "",
    isError: false,
    hasResult: false,
  };
  return { ...state, toolRuns: { ...state.toolRuns, [id]: run } };
}

function onToolUpdate(state: TranscriptState, event: RpcEvent): TranscriptState {
  const id = str(event.toolCallId);
  if (!id) return state;
  const existing = state.toolRuns[id];
  if (existing?.hasResult) return state;
  const partial = event.partialResult as ToolResultWire | undefined;
  const run: ToolRun = {
    id,
    name: str(event.toolName) || existing?.name || "",
    status: existing?.status ?? "running",
    text: partial ? textOfBlocks(partial.content ?? []) : (existing?.text ?? ""),
    isError: existing?.isError ?? false,
    hasResult: false,
    ...(existing?.durationMs !== undefined ? { durationMs: existing.durationMs } : {}),
  };
  return { ...state, toolRuns: { ...state.toolRuns, [id]: run } };
}

function onToolEnd(state: TranscriptState, event: RpcEvent): TranscriptState {
  const id = str(event.toolCallId);
  if (!id) return state;
  const existing = state.toolRuns[id];
  if (existing?.hasResult) return state; // the result row already landed
  const run: ToolRun = {
    id,
    name: str(event.toolName) || existing?.name || "",
    status: "done",
    text: existing?.text ?? "",
    isError: Boolean(event.isError),
    hasResult: false,
    ...(typeof event.durationMs === "number" ? { durationMs: event.durationMs } : {}),
  };
  return { ...state, toolRuns: { ...state.toolRuns, [id]: run } };
}

// ---------------------------------------------------------------------------
// Assistant-event application (the typewriter)
// ---------------------------------------------------------------------------

/**
 * Apply one assistantMessageEvent to a row's blocks. Returns the same array
 * reference when nothing changed, so the caller can skip a re-render.
 *
 * The `partial` snapshot pi sends is stripped on the wire (pi_json.go's
 * projection), so blocks are built from deltas alone. The *_end events carry
 * the authoritative content and simply replace the block — which is what
 * makes a provider that seeds text at *_start and sends no deltas (DeepSeek's
 * anthropic endpoint does exactly this with thinking) render correctly.
 */
function applyAssistantEvent(blocks: BlockView[], event: AssistantMessageEvent): BlockView[] {
  const at = (index: number): BlockView | undefined => blocks[index];
  switch (event.type) {
    case "start":
    case "done":
      return blocks;
    case "text_start":
      return setBlock(blocks, event.contentIndex, { kind: "text", text: "" });
    case "text_delta": {
      const block = at(event.contentIndex);
      const text = (block?.kind === "text" ? block.text : "") + event.delta;
      return setBlock(blocks, event.contentIndex, { kind: "text", text });
    }
    case "text_end":
      return setBlock(blocks, event.contentIndex, { kind: "text", text: event.content });
    case "thinking_start":
      return setBlock(blocks, event.contentIndex, { kind: "thinking", text: "" });
    case "thinking_delta": {
      const block = at(event.contentIndex);
      const text = (block?.kind === "thinking" ? block.text : "") + event.delta;
      return setBlock(blocks, event.contentIndex, { kind: "thinking", text });
    }
    case "thinking_end":
      return setBlock(blocks, event.contentIndex, { kind: "thinking", text: event.content });
    case "toolcall_start":
      return setBlock(blocks, event.contentIndex, { kind: "toolCall", id: "", name: "", args: {}, rawArgs: "" });
    case "toolcall_delta": {
      const block = at(event.contentIndex);
      if (block?.kind !== "toolCall") return blocks;
      return setBlock(blocks, event.contentIndex, { ...block, rawArgs: block.rawArgs + event.delta });
    }
    case "toolcall_end":
      return setBlock(blocks, event.contentIndex, {
        kind: "toolCall",
        id: event.toolCall.id,
        name: event.toolCall.name,
        args: event.toolCall.arguments ?? {},
        rawArgs: "",
      });
    default:
      return blocks;
  }
}

/** Write block[index], padding gaps with empty text blocks (defensive). */
function setBlock(blocks: BlockView[], index: number, block: BlockView): BlockView[] {
  if (index < 0) return blocks;
  const next = [...blocks];
  while (next.length < index) next.push({ kind: "text", text: "" });
  next[index] = block;
  return next;
}

// ---------------------------------------------------------------------------
// Selectors
// ---------------------------------------------------------------------------

/** The last assistant row's usage — the page's own context-water mark. */
export function lastUsage(state: TranscriptState): Usage | undefined {
  for (let i = state.messages.length - 1; i >= 0; i--) {
    const row = state.messages[i]!;
    if (row.role === "assistant" && row.usage) return row.usage;
  }
  return undefined;
}

/**
 * Tokens the next request will carry, as the page can count them: the last
 * assistant message's input plus its cache reads and writes. There is no
 * compaction in this port (plan §4.2), so this is the only context signal
 * the page has — it is an estimate, and it is labelled as one in the UI.
 */
export function contextTokens(state: TranscriptState): number | undefined {
  const usage = lastUsage(state);
  if (!usage) return undefined;
  return usage.input + usage.cacheRead + usage.cacheWrite;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function asMessage(value: unknown): WireMessage {
  return (value ?? {}) as WireMessage;
}

function asMessages(value: unknown): WireMessage[] | undefined {
  return Array.isArray(value) ? (value as WireMessage[]) : undefined;
}

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/** A completed message already present in the transcript (replay dedup). */
function findByStamp(state: TranscriptState, message: WireMessage): TranscriptMessage | undefined {
  const stamp = message.timestamp ?? 0;
  return state.messages.find((m) => m.role === message.role && m.timestamp === stamp);
}

/** Record a live non-streaming row as waiting for its message_end. */
function pushAwaiting(awaitingEnd: Record<string, string[]>, role: string, stamp: number, key: string): Record<string, string[]> {
  const id = `${role}:${stamp}`;
  return { ...awaitingEnd, [id]: [...(awaitingEnd[id] ?? []), key] };
}

/** Consume the oldest row waiting under an identity (FIFO: same-ms pairs keep their order). */
function shiftAwaiting(awaitingEnd: Record<string, string[]>, role: string, stamp: number): Record<string, string[]> {
  const id = `${role}:${stamp}`;
  const queue = awaitingEnd[id];
  if (!queue || queue.length === 0) return awaitingEnd;
  return { ...awaitingEnd, [id]: queue.slice(1) };
}

/** Wire content (string | blocks) to view blocks. */
export function normalizeContent(content: string | ContentBlock[] | undefined): BlockView[] {
  if (typeof content === "string") return content ? [{ kind: "text", text: content }] : [];
  if (!Array.isArray(content)) return [];
  const blocks: BlockView[] = [];
  for (const block of content) {
    switch (block.type) {
      case "text":
        blocks.push({ kind: "text", text: block.text ?? "" });
        break;
      case "thinking":
        blocks.push({ kind: "thinking", text: block.thinking ?? "" });
        break;
      case "image":
        blocks.push({ kind: "image", data: block.data ?? "", mimeType: block.mimeType ?? "image/png" });
        break;
      case "toolCall":
        blocks.push({
          kind: "toolCall",
          id: block.id ?? "",
          name: block.name ?? "",
          args: block.arguments ?? {},
          rawArgs: "",
        });
        break;
      default:
        break;
    }
  }
  return blocks;
}

/** Concatenated text of a content-block array (tool output is text blocks). */
function textOfBlocks(content: ContentBlock[] | string | undefined): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  let text = "";
  for (const block of content) {
    if (block.type === "text" && block.text) text += block.text;
  }
  return text;
}
