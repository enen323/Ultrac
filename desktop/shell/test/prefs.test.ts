// prefs.test.ts — the page's own preferences on disk. The shell reads three
// of them before any page script runs, so what is kept is a whitelist: a
// hand-edited file or a page bug cannot widen it. registerPrefs is tested
// against a fake ipcMain so the window gate is pinned without Electron.

import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { PREFIX, loadPrefs, pick, prefsFile, registerPrefs, savePrefs } from "../src/prefs.ts";

test("only the page's own string keys are kept, in both directions", () => {
  assert.deepEqual(pick({ "ultrac-lang": "zh", "ultrac-keep-awake": "on" }), { "ultrac-lang": "zh", "ultrac-keep-awake": "on" });
  assert.deepEqual(pick({ "ultrac-lang": 7, "ultrac-n": null }), {});
  assert.deepEqual(pick({ "rx-lang": "zh", "other": "x" }), {});
  assert.deepEqual(pick(null), {});
  assert.deepEqual(pick([1, 2]), {});
});

test("a roundtrip survives a relaunch, and a broken file reads as no prefs", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ultrac-prefs-"));
  const file = prefsFile(dir);
  assert.deepEqual(loadPrefs(file), {});
  savePrefs(file, { "ultrac-lang": "zh", "rx-lang": "dropped" });
  assert.deepEqual(loadPrefs(file), { "ultrac-lang": "zh" });
  // Written beside and renamed over: no .tmp is left behind.
  assert.equal(fs.existsSync(`${file}.tmp`), false);
  fs.writeFileSync(file, "{not json");
  assert.deepEqual(loadPrefs(file), {});
  fs.rmSync(dir, { recursive: true, force: true });
});

test("only the shell window may read or write, and a save announces itself", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ultrac-prefs-"));
  const file = prefsFile(dir);
  const handlers = new Map<string, (event: unknown, ...args: unknown[]) => void>();
  const ipcMain = { on: (channel: string, listener: (event: unknown, ...args: unknown[]) => void) => handlers.set(channel, listener) };
  let saves = 0;
  let fromThisWindow = true;
  registerPrefs(ipcMain, () => file, () => fromThisWindow, () => saves++);

  const load = handlers.get("prefs:load");
  const save = handlers.get("prefs:save");
  assert.ok(load && save, "both channels are registered");

  // A synchronous reply: the handler sets returnValue on the event it was
  // handed, exactly as ipcMain.on does.
  const reply = (listener: (event: unknown, ...args: unknown[]) => void, ...args: unknown[]) => {
    const event: { returnValue?: unknown } = {};
    listener(event, ...args);
    return event.returnValue;
  };

  assert.deepEqual(reply(load), {});
  savePrefs(file, { "ultrac-lang": "en" });
  assert.deepEqual(reply(load), { "ultrac-lang": "en" });

  reply(save, { "ultrac-lang": "zh" });
  assert.deepEqual(loadPrefs(file), { "ultrac-lang": "zh" });
  assert.equal(saves, 1);

  // A window that is not the one this launch owns is refused, not served.
  fromThisWindow = false;
  reply(save, { "ultrac-lang": "en" });
  assert.deepEqual(loadPrefs(file), { "ultrac-lang": "zh" });
  assert.deepEqual(reply(load), {});
  fs.rmSync(dir, { recursive: true, force: true });
});
