// preload.cjs — the renderer's door to the shell. The §3.2 transformation of
// Reasonix's preload.js. The big difference from Reasonix: there, the page
// talked to the kernel itself over HTTP with a cookie credential, and the
// bridge only carried OS-level verbs (window controls, dialogs, the agent's
// browser). Here the pipe is the security boundary and the kernel client
// lives in the main process, so the bridge carries the conversation itself:
// every command the page sends and every event the kernel pushes crosses
// here, correlated by RpcClient in main (decision D1).
//
// This file is CommonJS on purpose. Electron's ESM matrix is explicit: a
// sandboxed preload script cannot use ESM imports, and the window is created
// with sandbox: true. The rest of the shell is ESM TypeScript; the one file
// that must run inside the sandbox stays plain JS with a require of electron
// only — the same choice Reasonix's preload.js makes.
//
// Channel names are stated in both files rather than shared: a sandboxed
// preload cannot import the main process's modules, and a string that drifts
// fails loudly (an invoke with no handler rejects) rather than silently.
// main.ts is the other side of every one of them.

"use strict";
const { contextBridge, ipcRenderer } = require("electron");

// The page's own preference keys (prefs.ts is the main-process half).
const PREFIX = "ultrac-";

function subscribe(channel, listener) {
  const handler = (_event, payload) => listener(payload);
  ipcRenderer.on(channel, handler);
  return () => {
    ipcRenderer.removeListener(channel, handler);
  };
}

// Verbs, and nothing else. The page names no location, opens no file and
// reaches no OS surface that is not listed here.
//
//   call(command)        → one RPC command; resolves with the response or a
//                          failure code ({ ok: false, code, message }).
//   onEvent(listener)    → every pushed kernel event, in arrival order.
//   onSessionLoaded(l)   → the get_state payload, once, after the page loads.
//   openExternal(url)    → http/https only; links.ts decides in main.
contextBridge.exposeInMainWorld("ultrac", {
  shell: "electron",
  platform: process.platform,
  call: (command) => ipcRenderer.invoke("kernel:call", command),
  onEvent: (listener) => subscribe("kernel:event", listener),
  onSessionLoaded: (listener) => subscribe("session_loaded", listener),
  openExternal: (url) => ipcRenderer.invoke("shell:open-external", String(url)),
});

// The shell keeps the page's own preferences on disk (prefs.ts) because three
// of them — language, keep-awake, window state — are read before any page
// script runs. They are restored here, before the page reads them, and handed
// back as they change. A key the page already holds wins: it is at least as
// new as the copy on disk.
function ownPrefs() {
  const out = {};
  for (let i = 0; i < localStorage.length; i++) {
    const key = localStorage.key(i);
    if (key && key.startsWith(PREFIX)) out[key] = localStorage.getItem(key) ?? "";
  }
  return out;
}
try {
  const saved = ipcRenderer.sendSync("prefs:load") || {};
  for (const [key, value] of Object.entries(saved)) {
    if (key.startsWith(PREFIX) && localStorage.getItem(key) === null) localStorage.setItem(key, String(value));
  }
} catch {
  // Storage unavailable: the page runs on its defaults, as it always has.
}
let sent = "";
function flush(sync) {
  try {
    const prefs = ownPrefs();
    const next = JSON.stringify(prefs);
    if (next === sent) return;
    sent = next;
    if (sync) ipcRenderer.sendSync("prefs:save", prefs);
    else ipcRenderer.send("prefs:save", prefs);
  } catch {
    // Nothing to keep this time; the next change tries again.
  }
}
setInterval(() => flush(false), 3000);
window.addEventListener("pagehide", () => flush(true));
