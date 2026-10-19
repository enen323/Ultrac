// prefs.ts — the page's own preferences, kept on disk by the shell. The
// structure is Reasonix's prefs.js; the reason it survives the transport
// change is different and smaller. There, the kernel listened on a fresh
// loopback port each launch and localStorage was keyed by origin, so without
// this every launch opened on a page that remembered nothing. Here the page
// loads from file:// and persists on its own — but the shell still has to
// read three of the page's keys (language, keep-awake, and the window state
// the menus act on) before any page script runs, and only this process can
// do that. So: the page writes, the shell keeps the file, both speak the
// ultrac- prefix (plan §3.1).

import fs from "node:fs";
import path from "node:path";

export const PREFIX = "ultrac-";

export function prefsFile(userData: string): string {
  return path.join(userData, "window-prefs.json");
}

// Anything that is not one of the page's own string preferences is dropped, in
// both directions: a hand-edited file or a page bug cannot widen what is kept.
export function pick(value: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (!value || typeof value !== "object" || Array.isArray(value)) return out;
  for (const [key, v] of Object.entries(value as Record<string, unknown>)) {
    if (key.startsWith(PREFIX) && typeof v === "string") out[key] = v;
  }
  return out;
}

export function loadPrefs(file: string): Record<string, string> {
  try {
    return pick(JSON.parse(fs.readFileSync(file, "utf8")));
  } catch {
    return {};
  }
}

// Written beside and renamed over, so a crash mid-write leaves the last good
// copy rather than half of one.
export function savePrefs(file: string, prefs: unknown): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const tmp = `${file}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(pick(prefs)));
  fs.renameSync(tmp, file);
}

/** The subset of ipcMain this module uses (injected so tests need no Electron). */
export interface PrefsIpc {
  on: (channel: string, listener: (event: unknown, ...args: unknown[]) => void) => void;
}

/** The subset of an IpcMainEvent this module reads. */
export interface PrefsEvent {
  returnValue?: unknown;
}

// Only the shell window may read or write them. fileOf is asked on each use:
// the profile directory is settled during boot, after these handlers exist.
export function registerPrefs(
  ipcMain: PrefsIpc,
  fileOf: () => string,
  fromWindow: (event: unknown) => boolean,
  onSave: () => void = () => {},
): void {
  ipcMain.on("prefs:load", (event: PrefsEvent) => {
    event.returnValue = fromWindow(event) ? loadPrefs(fileOf()) : {};
  });
  ipcMain.on("prefs:save", (event: PrefsEvent, prefs: unknown) => {
    if (fromWindow(event)) {
      try {
        savePrefs(fileOf(), prefs);
        onSave();
      } catch {
        // A preference that did not save is one relaunch of lost state, not a
        // reason to fail the window.
      }
    }
    event.returnValue = true;
  });
}
