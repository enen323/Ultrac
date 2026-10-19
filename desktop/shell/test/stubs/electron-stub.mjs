// electron-stub.mjs — just enough Electron for main.ts to boot, and a
// driver that walks the boot to its end. This is the headless half of the
// P-S1 gate (plan §6): the four things `pnpm start` verifies by hand —
// spawn, ready, window, lease — minus the pixels. What it cannot verify
// (a real window on a real screen) stays a human's job; what it can (the
// whole main-process wiring: identity, lock, launch, bridge, event
// forwarding, IPC handlers, stdin lease, failure dialog) it verifies here,
// against the real host.ts / rpc-client.ts / main.ts code with a fake
// kernel on the other end of the pipe.
//
// The driver's stages:
//   boot    wait for session_loaded (the get_state probe answered)
//   bridge  invoke the kernel:call handler as the page would
//   lease   emit before-quit, wait for the kernel to honour stdin EOF
// A failed boot ends in app.quit() (failStartup), which finishes at once.
// The report is JSON on disk (ULTRAC_SHELL_REPORT); the test asserts on it.

import { EventEmitter } from "node:events";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const report = {
  appName: null,
  userData: null,
  singleInstance: true,
  sessionLoaded: null,
  events: [],
  ipc: { handles: [], ons: [] },
  bridgeCall: null,
  bridgeRefused: null,
  openExternal: [],
  window: { created: false, shown: false, loadedFile: null, loadedUrl: null, windowOpenHandler: null, preload: null },
  starting: { created: false, loadedUrl: null },
  kernel: { spawned: false, exit: null },
  dialogs: [],
  quit: 0,
  menu: { cleared: 0, built: 0 },
  prefs: { loads: 0, saves: 0 },
  errors: [],
  exit: null,
};

const reportPath = process.env.ULTRAC_SHELL_REPORT;
let finished = false;
function finish(code, why) {
  if (finished) return;
  finished = true;
  report.kernel.spawned = globalThis.__kernelSpawned === true;
  report.exit = { code, why: why ?? null };
  try {
    if (reportPath) fs.writeFileSync(reportPath, JSON.stringify(report, null, 2));
  } catch (err) {
    process.stderr.write(`stub: report write failed: ${err.message}\n`);
  }
  process.exit(code);
}

// -- app -------------------------------------------------------------------

const paths = {};
const app = new EventEmitter();
app.commandLine = { appendSwitch: () => {} };
app.setName = (name) => {
  report.appName = name;
};
app.setPath = (key, value) => {
  paths[key] = value;
};
app.getPath = (key) => paths[key] ?? path.join(os.tmpdir(), "ultrac-shell-stub", key);
app.getVersion = () => "0.1.0-stub";
app.isPackaged = false;
app.getLocale = () => "zh-CN";
app.getPreferredSystemLanguages = () => ["zh-CN"];
app.requestSingleInstanceLock = () => report.singleInstance;
app.relaunch = () => {};
app.whenReady = () => Promise.resolve();
app.quit = () => {
  report.quit++;
  finish(0, "app.quit");
};

// -- window -----------------------------------------------------------------

class FakeWebContents extends EventEmitter {
  constructor(kind, notifyWindow) {
    super();
    this.kind = kind;
    // The window's own events (ready-to-show) are emitted by the window, as
    // in real Electron; the load methods are what trigger them.
    this.notifyWindow = notifyWindow ?? (() => {});
    this.sent = [];
    this.crashed = false;
  }
  send(channel, ...args) {
    this.sent.push({ channel, args });
    if (channel === "session_loaded") report.sessionLoaded = args[0];
    if (channel === "kernel:event") report.events.push(args[0]);
  }
  setWindowOpenHandler(handler) {
    report.window.windowOpenHandler = typeof handler === "function" ? handler : null;
  }
  openDevTools() {}
  async loadFile(file) {
    report.window.loadedFile = file;
    this.emit("did-finish-load");
    this.notifyWindow("ready-to-show");
  }
  async loadURL(url) {
    // Only the main window's page is the shell's; the starting window's data
    // URL is recorded apart, so a slow launch cannot pass for a loaded page.
    if (this.kind === "main") report.window.loadedUrl = url;
    else report.starting.loadedUrl = url;
    this.emit("did-finish-load");
    this.notifyWindow("ready-to-show");
  }
  isCrashed() {
    return this.crashed;
  }
}

class FakeBrowserWindow extends EventEmitter {
  constructor(options) {
    super();
    this.options = options;
    // The main window is the one with a preload; the starting window has none.
    this.kind = options?.webPreferences?.preload ? "main" : "starting";
    this.webContents = new FakeWebContents(this.kind, (channel) => this.emit(channel));
    if (this.kind === "main") {
      report.window.created = true;
      report.window.preload = options.webPreferences.preload;
      globalThis.__lastWindow = this;
    } else {
      report.starting.created = true;
    }
  }
  show() {
    report.window.shown = true;
  }
  // Real Electron's window carries these too, delegating to its contents.
  loadFile(file) {
    return this.webContents.loadFile(file);
  }
  loadURL(url) {
    return this.webContents.loadURL(url);
  }
  close() {}
  focus() {}
  restore() {}
  minimize() {}
  maximize() {}
  unmaximize() {}
  isMinimized() {
    return false;
  }
  isMaximized() {
    return false;
  }
  isVisible() {
    return true;
  }
  isDestroyed() {
    return false;
  }
  isFullScreen() {
    return false;
  }
  setFullScreen() {}
  removeMenu() {}
}

// -- the rest of the surface main.ts touches ---------------------------------

const handlers = {};
const listeners = {};
const ipcMain = {
  handle(channel, fn) {
    report.ipc.handles.push(channel);
    handlers[channel] = fn;
  },
  on(channel, fn) {
    report.ipc.ons.push(channel);
    listeners[channel] = fn;
    if (channel === "prefs:load") report.prefs.loads++;
    if (channel === "prefs:save") report.prefs.saves++;
  },
};

const dialog = {
  showErrorBox(title, detail) {
    report.dialogs.push({ title, detail });
  },
  showMessageBox: async () => ({ response: 1 }),
  showSaveDialog: async () => ({ canceled: true }),
  showOpenDialog: async () => ({ canceled: true, filePaths: [] }),
};

const powerSaveBlocker = {
  start: () => 1,
  stop: () => {},
  isStarted: () => true,
};

const screen = {
  getPrimaryDisplay: () => ({ workAreaSize: { width: 1600, height: 900 }, scaleFactor: 1 }),
  on: () => {},
  removeListener: () => {},
};

const shell = {
  openExternal: async (url) => {
    report.openExternal.push(url);
  },
  showItemInFolder: () => {},
  trashItem: async () => {},
};

const Menu = {
  buildFromTemplate: (template) => {
    report.menu.built++;
    return { template, popup: () => {} };
  },
  setApplicationMenu: (menu) => {
    if (menu === null) report.menu.cleared++;
  },
  getApplicationMenu: () => null,
};

export { app, FakeBrowserWindow as BrowserWindow, dialog, ipcMain, powerSaveBlocker, screen, shell, Menu };

// -- the driver ---------------------------------------------------------------

const watchdog = setTimeout(() => finish(4, "watchdog: the boot never settled"), 20_000);

const stage = { name: "boot" };
setInterval(() => {
  if (finished) return;
  if (report.quit > 0) return; // app.quit() already finished
  if (stage.name === "boot" && report.sessionLoaded) {
    stage.name = "bridge";
    void driveBridge();
  }
  // A turn's events are asynchronous pushes; the shell is expected to still
  // be running when they land, so the driver waits for the turn to end
  // before asking for quit. (Ending the lease first would close stdin and
  // take the kernel's pending timers with it — which is correct lease
  // behaviour and wrong test timing.)
  if (stage.name === "events" && report.events.some((e) => e.type === "agent_end")) {
    stage.name = "lease";
    app.emit("before-quit");
  }
  if (stage.name === "events" && Date.now() - stage.since > 5_000) {
    finish(5, "the turn's events never finished arriving");
  }
  if (stage.name === "lease" && globalThis.__kernelExit) {
    report.kernel.exit = globalThis.__kernelExit;
    finish(0);
  }
}, 25);

async function driveBridge() {
  const handler = handlers["kernel:call"];
  const win = globalThis.__lastWindow;
  if (!handler || !win) {
    finish(3, "the boot produced no window or no kernel:call handler");
    return;
  }
  // A sender that is not the window's own contents is refused rather than
  // served — check the gate, not just the happy path.
  report.bridgeRefused = await handler({ sender: { not: "the window" } }, { type: "get_commands" });
  report.bridgeCall = await handler({ sender: win.webContents }, { type: "get_commands" });
  // The lease comes after the turn: closing this end of the pipe is what
  // tells the kernel to drain, and the driver asks for it exactly as
  // window-all-closed would — once the page has nothing left to wait for.
  stage.name = "events";
  stage.since = Date.now();
}
