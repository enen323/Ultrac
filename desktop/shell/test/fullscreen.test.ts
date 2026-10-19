// fullscreen.test.ts — F11 where there is no menu to carry it. Off macOS
// the shell installs no application menu, and F11 is otherwise bound only
// through a menu role; macOS keeps its own control on the title bar.

import { test } from "node:test";
import assert from "node:assert/strict";
import { installFullScreenKey } from "../src/fullscreen.ts";

function fake() {
  const listeners: Array<(event: { preventDefault: () => void }, input: unknown) => void> = [];
  const calls: string[] = [];
  let fullScreen = false;
  return {
    calls,
    fire: (input: Record<string, unknown>) => {
      let prevented = false;
      for (const listener of listeners) listener({ preventDefault: () => (prevented = true) }, input);
      return prevented;
    },
    contents: { on: (channel: string, listener: never) => listeners.push(listener) },
    window: {
      setFullScreen: (flag: boolean) => {
        calls.push(`setFullScreen:${flag}`);
        fullScreen = flag;
      },
      isFullScreen: () => fullScreen,
    },
  };
}

test("F11 toggles full screen, with modifiers or repeats left alone", () => {
  const f = fake();
  installFullScreenKey(f.contents as never, f.window as never, "win32");
  assert.equal(f.fire({ type: "keyDown", key: "F11" }), true);
  assert.equal(f.fire({ type: "keyDown", key: "F11" }), true);
  assert.deepEqual(f.calls, ["setFullScreen:true", "setFullScreen:false"]);
  assert.equal(f.fire({ type: "keyDown", key: "F11", control: true }), false);
  assert.equal(f.fire({ type: "keyDown", key: "F11", shift: true }), false);
  assert.equal(f.fire({ type: "keyDown", key: "F11", isAutoRepeat: true }), false);
  assert.equal(f.fire({ type: "keyUp", key: "F11" }), false);
  assert.equal(f.calls.length, 2);
});

test("macOS keeps its own control and installs nothing", () => {
  const f = fake();
  installFullScreenKey(f.contents as never, f.window as never, "darwin");
  assert.equal(f.fire({ type: "keyDown", key: "F11" }), false);
  assert.deepEqual(f.calls, []);
});
