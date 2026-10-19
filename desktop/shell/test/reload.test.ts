// reload.test.ts — the recovery chords, and the crash loop that stops being
// recovered. A dead renderer's input never reaches before-input-event, so
// the crash itself has to trigger the reload — but a page that dies on every
// load stops being reloaded after three tries inside a minute.

import { test } from "node:test";
import assert from "node:assert/strict";
import { CRASH_RELOADS, installPaneReload, installReload, reloadAction } from "../src/reload.ts";

/** A WebContents that records what was asked of it and can fire events. */
function fakeContents() {
  const input: Array<(event: { preventDefault: () => void }, input: unknown) => void> = [];
  const gone: Array<(event: unknown, details: unknown) => void> = [];
  const calls: string[] = [];
  return {
    calls,
    fireInput: (inputEvent: Record<string, unknown>) => {
      let prevented = false;
      for (const listener of input) listener({ preventDefault: () => (prevented = true) }, { type: "keyDown", isAutoRepeat: false, ...inputEvent });
      return prevented;
    },
    fireGone: (details: Record<string, unknown>) => {
      for (const listener of gone) listener({}, details);
    },
    contents: {
      on: (channel: string, listener: never) => {
        if (channel === "before-input-event") input.push(listener);
        if (channel === "render-process-gone") gone.push(listener);
      },
      reload: () => calls.push("reload"),
      reloadIgnoringCache: () => calls.push("hard"),
      isCrashed: () => true,
    },
  };
}

const window = { isDestroyed: () => false, isVisible: () => true };

test("the reload chords are the page's own, and the recovery one is shifted", () => {
  const key = (over: Record<string, unknown>) => ({ type: "keyDown", isAutoRepeat: false, ...over });
  // Windows/Linux: control, macOS: meta. Never both.
  assert.equal(reloadAction(key({ key: "r", control: true }), "win32"), "reload");
  assert.equal(reloadAction(key({ key: "r", control: true, shift: true }), "win32"), "hard");
  assert.equal(reloadAction(key({ key: "r", meta: true }), "darwin"), "reload");
  assert.equal(reloadAction(key({ key: "r", meta: true, shift: true }), "darwin"), "hard");
  assert.equal(reloadAction(key({ key: "r", control: true }), "darwin"), null);
  assert.equal(reloadAction(key({ key: "r", meta: true }), "win32"), null);
  // F5 is the plain one.
  assert.equal(reloadAction(key({ key: "F5" }), "win32"), "reload");
  assert.equal(reloadAction(key({ key: "F5", control: true }), "win32"), null);
  // A non-latin layout reports a code instead of a key.
  assert.equal(reloadAction(key({ key: "і", code: "KeyR", control: true }), "win32"), "reload");
  // Alt is the compositor's, and a keyUp or autorepeat is not a chord.
  assert.equal(reloadAction(key({ key: "r", control: true, alt: true }), "win32"), null);
  assert.equal(reloadAction({ type: "keyUp", key: "r", control: true }, "win32"), null);
  assert.equal(reloadAction({ type: "keyDown", key: "r", control: true, isAutoRepeat: true }, "win32"), null);
  assert.equal(reloadAction(key({ key: "x", control: true }), "win32"), null);
});

test("a pane's plain chord reloads and its shifted one ignores the cache", () => {
  const fake = fakeContents();
  installPaneReload(fake.contents as never, "win32");
  assert.equal(fake.fireInput({ key: "r", control: true }), true);
  assert.equal(fake.fireInput({ key: "r", control: true, shift: true }), true);
  assert.deepEqual(fake.calls, ["reload", "hard"]);
  // A chord that is not a reload is left to the page.
  assert.equal(fake.fireInput({ key: "x", control: true }), false);
  assert.deepEqual(fake.calls, ["reload", "hard"]);
});

test("the shell's own recovery chord reloads plainly, and a plain chord is the page's", () => {
  const fake = fakeContents();
  installReload(fake.contents as never, window as never, { platform: "win32" });
  assert.equal(fake.fireInput({ key: "r", control: true, shift: true }), true);
  assert.deepEqual(fake.calls, ["reload"]);
  assert.equal(fake.fireInput({ key: "r", control: true }), false);
  assert.deepEqual(fake.calls, ["reload"]);
});

test("a crash reloads a visible window, and a clean exit is not a crash", () => {
  let now = 1_000_000;
  const fake = fakeContents();
  installReload(fake.contents as never, window as never, { platform: "win32", now: () => now });
  fake.fireGone({ reason: "clean-exit" });
  assert.deepEqual(fake.calls, [], "a clean exit reloads nothing");
  fake.fireGone({ reason: "crashed" });
  assert.deepEqual(fake.calls, ["reload"]);
});

test(`a page that dies on every load stops being reloaded after ${CRASH_RELOADS} tries`, () => {
  let now = 1_000_000;
  const fake = fakeContents();
  installReload(fake.contents as never, window as never, { platform: "win32", now: () => now });
  for (let i = 0; i < CRASH_RELOADS + 2; i++) {
    now += 1_000;
    fake.fireGone({ reason: "crashed" });
  }
  assert.deepEqual(fake.calls, ["reload", "reload", "reload"]);
  // The window is a minute old: the count forgets and recovery resumes.
  now += 61_000;
  fake.fireGone({ reason: "crashed" });
  assert.equal(fake.calls.length, CRASH_RELOADS + 1);
});

test("a hidden crashed window is recovered by the revive, not the crash", () => {
  const fake = fakeContents();
  const hidden = { isDestroyed: () => false, isVisible: () => false };
  const guard = installReload(fake.contents as never, hidden as never, { platform: "win32" });
  fake.fireGone({ reason: "crashed" });
  assert.deepEqual(fake.calls, [], "a hidden window's crash does not reload it");
  guard.revive();
  assert.deepEqual(fake.calls, ["reload"], "the next revive before it is shown does");
});
