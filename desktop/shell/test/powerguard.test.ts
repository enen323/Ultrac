// powerguard.test.ts — the machine stays awake while a turn is in flight,
// and only then. The semantics under test (ported from Reasonix): streaming
// takes the block at once; letting go needs IDLE_PASSES clean not-streaming
// answers so a turn that ends and another that starts do not flap it; a pass
// that could not be answered neither takes it nor counts toward letting go;
// and a kernel that never answers again cannot hold the block forever.

import { test } from "node:test";
import assert from "node:assert/strict";
import { IDLE_PASSES, PREF_KEY, SILENT_PASSES, createPowerGuard, keepsAwake } from "../src/powerguard.ts";

function fakeBlocker() {
  let next = 1;
  const started = new Set<number>();
  return {
    calls: [] as string[],
    start: (kind: string) => {
      started.add(next);
      return next++;
    },
    stop: (id: number) => {
      started.delete(id);
    },
    isStarted: (id: number) => started.has(id),
  };
}

/** A guard whose running() answers from a script, and whose timer is manual. */
function makeGuard(answers: Array<{ streaming?: boolean } | null>, opts: { enabled?: () => boolean } = {}) {
  const blocker = fakeBlocker();
  let armed: (() => void) | null = null;
  const guard = createPowerGuard({
    blocker,
    running: async () => {
      const next = answers.shift();
      return next === undefined ? null : next;
    },
    enabled: opts.enabled ?? (() => true),
    setInterval: (fn: () => void) => {
      armed = fn;
      return { unref: () => {} };
    },
    clearInterval: () => {
      armed = null;
    },
  });
  return { blocker, guard, tick: () => armed?.() };
}

test("a turn in flight takes the block at once, and its end lets it go after the idle passes", async () => {
  const { guard } = makeGuard([{ streaming: true }, { streaming: false }, { streaming: false }, { streaming: false }]);
  await guard.refresh();
  assert.equal(guard.held(), true, "streaming takes the block at once");
  await guard.refresh();
  assert.equal(guard.held(), true, "one clean zero is not enough");
  await guard.refresh();
  assert.equal(guard.held(), true, "two clean zeros are not enough");
  await guard.refresh();
  assert.equal(guard.held(), false, "the third clean zero lets go");
});

test("a kernel that cannot be asked neither takes nor counts, and gives up after a minute", async () => {
  // One streaming answer takes the block; then SILENT_PASSES answers that
  // could not be obtained. The last of them is the one that lets go.
  const { guard } = makeGuard([{ streaming: true }, ...Array(SILENT_PASSES).fill(null)]);
  await guard.refresh();
  assert.equal(guard.held(), true);
  for (let i = 0; i < SILENT_PASSES - 1; i++) await guard.refresh();
  assert.equal(guard.held(), true, "an unanswered pass does not count toward letting go");
  await guard.refresh();
  assert.equal(guard.held(), false, "but it cannot hold the block forever");
});

test("a silent kernel never takes the block in the first place", async () => {
  const { guard } = makeGuard([null]);
  await guard.refresh();
  assert.equal(guard.held(), false);
});

test("the preference off means never held, even mid-turn", async () => {
  const { guard } = makeGuard([{ streaming: true }], { enabled: () => false });
  await guard.refresh();
  assert.equal(guard.held(), false);
  assert.equal(keepsAwake({ [PREF_KEY]: "on" }), true);
  assert.equal(keepsAwake({ [PREF_KEY]: "off" }), false);
  assert.equal(keepsAwake({}), true, "anything but an explicit off is the default");
});

test("begin arms the poll and close disarms it and releases", async () => {
  const { blocker, guard, tick } = makeGuard([{ streaming: true }]);
  guard.begin();
  // begin refreshes once immediately — asynchronously, so let the pass land.
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(guard.held(), true, "begin refreshes once immediately");
  tick();
  await new Promise((resolve) => setImmediate(resolve));
  // The poll found no scripted answer (unknown), which neither takes nor
  // releases — the block taken by the immediate refresh stays.
  assert.equal(guard.held(), true);
  guard.close();
  assert.equal(guard.held(), false);
  tick();
  assert.equal(blocker.isStarted(1), false, "a closed guard holds nothing");
});

test("a stale pass cannot change the answer after a newer one landed", async () => {
  const blocker = fakeBlocker();
  let resolveFirst: ((v: { streaming: boolean }) => void) | null = null;
  const guard = createPowerGuard({
    blocker,
    running: () =>
      new Promise<{ streaming: boolean } | null>((resolve) => {
        if (!resolveFirst) resolveFirst = resolve;
        else resolve({ streaming: true });
      }),
    enabled: () => true,
    setInterval: () => ({ unref: () => {} }),
    clearInterval: () => {},
  });
  const first = guard.refresh();
  const second = guard.refresh();
  resolveFirst?.({ streaming: true });
  await first;
  await second;
  assert.equal(guard.held(), true, "the newer pass took the block");
});
