// history.ts — the page's history source: the session file, not the live
// model context.
//
// # The bug this file exists for
//
// The page used to build its transcript from get_messages, i.e. from
// agent.State().Messages. That list is the *request* the model is about to
// be given, and compaction is defined as "rewrite the request": the
// prefix is replaced by a summary framed as a user message
// (internal/session/pi_context.go's compactionSummaryPrefix). So after a
// compaction — or after re-opening a compacted session — get_messages
// answers with the summary in the user's position and nothing before the
// cut. The page rendered exactly that: the user's own words replaced by the
// LLM's summary, and the earlier conversation unviewable.
//
// The session file keeps everything: compaction is an *appended* entry
// (internal/session/pi_manager.go AppendCompaction), it never deletes the
// messages it summarized. get_entries serves those entries; this module
// projects them into rows.
//
// # The walk, not the file order
//
// Entries chain through parentId, and a fork moves the leaf back without
// deleting the abandoned suffix. Reading the file top-to-bottom would
// replay a conversation that never happened, so the rows come from the
// parent walk out of leafId — the same rule the kernel's buildPath uses
// (internal/session/pi_context.go).
//
// # What a compaction entry becomes
//
// A notice row, not a message. It is a boundary in the conversation, and
// rendering it as a user turn is what made the summary look like something
// the user typed. The turns before it stay where they were.

import type { HistoryRow } from "./transcript.ts";
import type { WireEntry } from "./wire.ts";

/**
 * Project entry lines into ordered history rows: every message on the
 * current branch, plus one notice per compaction or branch summary.
 *
 * Entries that contribute nothing to a conversation (model_change,
 * thinking_level_change, session_info, usage, custom, label, context_edit,
 * queue operations) are skipped — the same set the kernel's
 * contextMessages ignores.
 */
export function historyRows(entries: WireEntry[], leafId: string | null): HistoryRow[] {
  const rows: HistoryRow[] = [];
  for (const entry of pathOf(entries, leafId)) {
    switch (entry.type) {
      case "message": {
        if (!entry.message) break;
        rows.push({ kind: "message", message: entry.message });
        break;
      }
      case "custom_message": {
        // A custom message enters context as a user message (pi's
        // convertToLlm); the page shows it the same way.
        const content = entry.content;
        if (content === undefined) break;
        rows.push({
          kind: "message",
          message: { role: "user", content, timestamp: stampOf(entry) },
        });
        break;
      }
      case "compaction": {
        const summary = (entry.summary ?? "").trim();
        if (!summary) break;
        rows.push({
          kind: "compaction",
          entryId: entry.id ?? "",
          timestamp: stampOf(entry),
          summary: entry.summary ?? "",
          ...(typeof entry.tokensBefore === "number" ? { tokensBefore: entry.tokensBefore } : {}),
        });
        break;
      }
      case "branch_summary": {
        const summary = (entry.summary ?? "").trim();
        if (!summary) break;
        rows.push({
          kind: "branch",
          entryId: entry.id ?? "",
          timestamp: stampOf(entry),
          summary: entry.summary ?? "",
        });
        break;
      }
      default:
        // State-only entry, or a type this build does not know: it shapes
        // the session, not the conversation.
        break;
    }
  }
  return rows;
}

/**
 * The branch from the leaf back to the root, root-first. A blank leaf (no
 * entries yet) yields nothing, and a corrupt parent chain stops instead of
 * looping — the kernel's buildPath makes the same two concessions.
 */
export function pathOf(entries: WireEntry[], leafId: string | null): WireEntry[] {
  if (!leafId) return [];
  const byId = new Map<string, WireEntry>();
  for (const entry of entries) if (entry.id) byId.set(entry.id, entry);

  const path: WireEntry[] = [];
  const seen = new Set<string>();
  let current = byId.get(leafId);
  while (current) {
    if (current.id) {
      if (seen.has(current.id)) break;
      seen.add(current.id);
    }
    path.push(current);
    const parent = current.parentId;
    current = parent ? byId.get(parent) : undefined;
  }
  return path.reverse();
}

/** An entry's timestamp as Unix milliseconds; 0 when absent or unparseable. */
function stampOf(entry: WireEntry): number {
  const parsed = Date.parse(entry.timestamp ?? "");
  return Number.isNaN(parsed) ? 0 : parsed;
}
