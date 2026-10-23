// history.test.ts — the page's history source (state/history.ts).
//
// The case this file is built around is the compaction bug: the page used
// to read get_messages (the model's request) and a compaction rewrites it,
// so on re-opening a compacted session the summary appeared in the user's
// bubble and everything before the cut was gone. These tests pin the fix:
// rows come from the session file, the turns survive, the summary is a
// notice, and the walk follows the leaf rather than the file.

import assert from "node:assert/strict";
import { test } from "node:test";
import { historyRows, pathOf } from "../src/state/history.ts";
import { fromHistory } from "../src/state/transcript.ts";
import type { WireEntry } from "../src/state/wire.ts";

// -- builders ---------------------------------------------------------------

let stamp = 1_700_000_000_000;

function iso(offset = 0): string {
  return new Date(stamp + offset * 1000).toISOString();
}

/** A message entry; parent is the id given (null for a root). */
function messageEntry(id: string, parentId: string | null, role: "user" | "assistant" | "toolResult", text: string): WireEntry {
  stamp += 1000;
  return {
    type: "message",
    id,
    parentId,
    timestamp: iso(),
    message: { role, content: [{ type: "text", text }], timestamp: stamp },
  };
}

function compactionEntry(id: string, parentId: string, summary: string, tokensBefore = 42000): WireEntry {
  stamp += 1000;
  return {
    type: "compaction",
    id,
    parentId,
    timestamp: iso(),
    summary,
    firstKeptEntryId: parentId,
    tokensBefore,
  };
}

/** The conversation the bug report describes: two turns, then a cut. */
function compactedSession(): WireEntry[] {
  return [
    messageEntry("e1", null, "user", "帮我看看 main.go 的结构"),
    messageEntry("e2", "e1", "assistant", "它有三个包"),
    compactionEntry("e3", "e2", "用户在问 main.go 的结构，我答了三个包。"),
    messageEntry("e4", "e3", "user", "那第二个包呢"),
  ];
}

// -- the compaction case ----------------------------------------------------

test("a compaction cut keeps every summarized turn and marks the boundary", () => {
  const rows = historyRows(compactedSession(), "e4");

  assert.deepEqual(
    rows.map((row) => row.kind),
    ["message", "message", "compaction", "message"],
  );
  // The turns the summary replaced are still there, in their own words.
  assert.deepEqual(rows[0], {
    kind: "message",
    message: { role: "user", content: [{ type: "text", text: "帮我看看 main.go 的结构" }], timestamp: 1_700_000_001_000 },
  });
  // The cut carries the metadata the page renders.
  const notice = rows[2];
  assert.equal(notice.kind, "compaction");
  if (notice.kind === "compaction") {
    assert.equal(notice.entryId, "e3");
    assert.equal(notice.tokensBefore, 42000);
    assert.match(notice.summary, /三个包/);
  }
  assert.deepEqual(rows[3]?.kind, "message");
});

test("the summary never turns into a user turn", () => {
  const rows = historyRows(compactedSession(), "e4");
  const userTexts = rows
    .filter((row) => row.kind === "message" && row.message.role === "user")
    .map((row) => (row.kind === "message" ? row.message.content : ""));

  assert.deepEqual(userTexts, [
    [{ type: "text", text: "帮我看看 main.go 的结构" }],
    [{ type: "text", text: "那第二个包呢" }],
  ]);
});

test("fromHistory builds rows the page can render: notice role, no streaming", () => {
  const state = fromHistory(historyRows(compactedSession(), "e4"));

  assert.deepEqual(
    state.messages.map((row) => row.role),
    ["user", "assistant", "notice", "user"],
  );
  const notice = state.messages[2]!;
  assert.equal(notice.streaming, false);
  assert.equal(notice.notice, "compaction");
  assert.equal(notice.tokensBefore, 42000);
  assert.deepEqual(notice.blocks, [{ kind: "text", text: "用户在问 main.go 的结构，我答了三个包。" }]);
  assert.equal(state.streamingKey, null);
});

// -- the walk ---------------------------------------------------------------

test("the walk starts at the leaf: an abandoned fork suffix is not history", () => {
  const entries = [
    ...compactedSession(),
    messageEntry("e5", "e4", "assistant", "第二个包是 tool"),
    // A fork moved the leaf back to e4 and the branch changed:
    messageEntry("e6", "e4", "user", "换个问法"),
  ];

  const rows = historyRows(entries, "e6");
  const texts = rows.filter((row) => row.kind === "message").map((row) => (row.kind === "message" ? row.message.content : ""));
  assert.deepEqual(texts.at(-1), [{ type: "text", text: "换个问法" }]);
  assert.equal(
    rows.some((row) => row.kind === "message" && JSON.stringify(row.message.content).includes("第二个包是 tool")),
    false,
  );
});

test("a blank leaf is an empty session, and a cycle stops instead of looping", () => {
  assert.deepEqual(pathOf(compactedSession(), null), []);
  const cyclic: WireEntry[] = [
    { type: "message", id: "a", parentId: "b", timestamp: iso(), message: { role: "user", content: "a", timestamp: stamp } },
    { type: "message", id: "b", parentId: "a", timestamp: iso(), message: { role: "assistant", content: "b", timestamp: stamp } },
  ];
  // Two entries and then the guard fires: root-first is b → a, the walk
  // never revisits a. (A cycle has no root, so "root-first" is just where
  // it happened to stop — what matters is that it stopped.)
  const walked = pathOf(cyclic, "a");
  assert.deepEqual(
    walked.map((entry) => entry.id),
    ["b", "a"],
  );
});

// -- entries that are not messages ------------------------------------------

test("a branch summary is a boundary notice of its own kind", () => {
  const entries: WireEntry[] = [
    messageEntry("e1", null, "user", "旧分支上的问题"),
    { type: "branch_summary", id: "e2", parentId: "e1", timestamp: iso(), summary: "旧分支在讨论部署。" },
    messageEntry("e3", "e2", "user", "继续"),
  ];

  const rows = historyRows(entries, "e3");
  assert.deepEqual(
    rows.map((row) => row.kind),
    ["message", "branch", "message"],
  );
  assert.equal(fromHistory(rows).messages[1]?.notice, "branch");
});

test("state-only entries contribute nothing; a custom message reads as a user turn", () => {
  const entries: WireEntry[] = [
    messageEntry("e1", null, "user", "你好"),
    { type: "model_change", id: "e2", parentId: "e1", timestamp: iso() },
    { type: "thinking_level_change", id: "e3", parentId: "e2", timestamp: iso() },
    { type: "session_info", id: "e4", parentId: "e3", timestamp: iso() },
    { type: "custom_message", id: "e5", parentId: "e4", timestamp: iso(), content: "来自扩展的提醒" },
  ];

  const rows = historyRows(entries, "e5");
  assert.deepEqual(
    rows.map((row) => row.kind),
    ["message", "message"],
  );
  const custom = rows[1];
  assert.equal(custom.kind, "message");
  if (custom.kind === "message") {
    assert.equal(custom.message.role, "user");
    assert.equal(custom.message.content, "来自扩展的提醒");
  }
});

test("a compaction entry without a summary is not a cut the page can show", () => {
  const entries: WireEntry[] = [
    messageEntry("e1", null, "user", "你好"),
    { type: "compaction", id: "e2", parentId: "e1", timestamp: iso(), summary: "   " },
  ];
  assert.deepEqual(
    historyRows(entries, "e2").map((row) => row.kind),
    ["message"],
  );
});
