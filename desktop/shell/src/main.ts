// main.ts — the Electron shell's entry point (plan §3.2's transformed
// main.js). What this file owns:
//
//   1. Identity and the single-instance lock, before anything is created.
//   2. The launch: spawn `ultrac --mode rpc` (host.ts), wait for the ready
//      probe, and treat its get_state answer as the SPA's initialization
//      payload (plan §2).
//   3. The window: load the SPA from disk (loadFile — there is no origin to
//      load from; the pipe is the boundary), or a placeholder page while the
//      SPA has not been built.
//   4. The bridge: the kernel client lives here, and the renderer reaches it
//      through preload.cjs's four verbs (kernel:call / kernel:event /
//      session_loaded / shell:open-external).
//   5. The lease: closing stdin is what tells the kernel to drain
//      (cmd/ultrac/rpc.go). before-quit ends it; a kernel that ignores the
//      lease is killed after a grace period.
//
// What was cut from Reasonix's main.js, and why: the cookie arm and the
// will-navigate origin guard (there is no origin and no credential — D1), the
// agent-browser trio (browserviews/browserprotocol/browserrelay — Ultrac has
// no browser pages), the layout's computerHelper branch, ozone, and the tray.
// The tray is cut for now only because it needs an icon asset that arrives
// with packaging (appicon.ts), and Reasonix's own rule applies: a shell with
// no icon must never background its window — so close quits, and the lease
// does the rest. Frameless windows and the custom title bar are deferred to
// P-S2 alongside the SPA that would have to draw the chrome; the system frame
// keeps phase one honest.
//
// Module order matters here in one place: app.setPath and
// app.requestSingleInstanceLock must run before the ready event, and with an
// ESM entry that means this file's side effects — which is exactly why the
// entry is src/main.ts itself (package.json "main"), not a dynamic import
// (Electron's ESM guide documents that footgun).

import { app, BrowserWindow, dialog, ipcMain, powerSaveBlocker, screen, shell } from "electron";
import type { BrowserWindow as BrowserWindowType, WebContents } from "electron";
import { homedir } from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import { READY_TIMEOUT_MS, SESSION_LOADED, kernelArgs, launchKernel } from "./host.ts";
import type { HostStartError, KernelHandle } from "./host.ts";
import { EvAgentEnd, EvMessageStart, hostClientFor } from "./hostclient.ts";
import type { HostClient } from "./hostclient.ts";
import { installApplicationMenu, installContextMenu } from "./menu.ts";
import { installFullScreenKey } from "./fullscreen.ts";
import { installReload } from "./reload.ts";
import { externalTarget } from "./links.ts";
import { appIcon } from "./appicon.ts";
import { hasPage, kernelBinary, pageDir } from "./layout.ts";
import { instanceID, profileFor } from "./instance.ts";
import { loadPrefs, prefsFile, registerPrefs } from "./prefs.ts";
import { createPowerGuard, keepsAwake } from "./powerguard.ts";
import { uiLanguage } from "./uilang.ts";
import { failStartup, openLogs, redactArgv } from "./shelllog.ts";
import type { ShellLogs } from "./shelllog.ts";
import { missingPage, showStarting } from "./starting.ts";
import type { RpcCommand, RpcEvent, RpcSessionState } from "./types.ts";

// A page in a minimized or fully covered window counts as hidden, and a
// hidden page drops the timers a streaming answer arrives on. The agent
// works while the person is in another app, so occlusion must not reach the
// window. The cost is that this application's renderer keeps running at full
// rate while nobody is looking — which is the point.
app.commandLine.appendSwitch("disable-backgrounding-occluded-windows");

const DEFAULT_SIZE = { width: 1440, height: 900 };
const MIN_SIZE = { width: 760, height: 480 };
// How long before-quit waits for the kernel to honour the lease (stdin EOF)
// before killing it. A healthy kernel exits in milliseconds; this only fires
// on a wedged one.
const LEASE_GRACE_MS = 2_000;
// How long a main-process call may take before it answers "the kernel did
// not answer" instead of hanging a surface (the power guard's get_state).
const CALL_TIMEOUT_MS = 5_000;
// Kernel events are buffered until the page has finished loading, so a
// message_start that lands mid-load is not lost. Bounded, because a page
// that never loads must not grow this process.
const EVENT_BUFFER_LIMIT = 200;

const here = path.dirname(fileURLToPath(import.meta.url));
const where = {
  packaged: app.isPackaged,
  // Electron injects this; it is absent under plain node (the stub smoke),
  // where nothing packaged is resolved anyway.
  resourcesPath: (process as { resourcesPath?: string }).resourcesPath ?? "",
  dirname: here,
  platform: process.platform,
  env: process.env,
};

let kernel: KernelHandle | null = null;
let host: HostClient | null = null;
let win: BrowserWindowType | null = null;
let starting: BrowserWindowType | null = null;
let logs: ShellLogs | null = null;
let powerGuard: ReturnType<typeof createPowerGuard> | null = null;
let quitting = false;
let handshaken = false;
// The page has finished loading; until then kernel events wait in the buffer.
let rendererReady = false;
let buffered: RpcEvent[] = [];

function closeStarting(): void {
  const current = starting;
  starting = null;
  if (current && !current.isDestroyed()) current.close();
}

function userData(): string {
  return app.getPath("userData");
}

function uiLang(): "zh" | "en" {
  const prefs = logs ? loadPrefs(prefsFile(userData())) : {};
  return uiLanguage(prefs, app.getPreferredSystemLanguages()[0] ?? app.getLocale());
}

function readyTimeoutMs(): number {
  const ms = Number(process.env.ULTRAC_SHELL_READY_TIMEOUT_MS);
  return Number.isFinite(ms) && ms > 0 ? ms : READY_TIMEOUT_MS;
}

// Where the kernel's sessions live: its cwd. A desktop shell's natural
// workspace is the home directory — the shell is started from wherever the
// launcher happens to be, which is no place at all. ULTRAC_SHELL_CWD
// overrides for tests and odd setups.
function kernelCwd(): string {
  return process.env.ULTRAC_SHELL_CWD || homedir();
}

// Every handler answers the one window this launch owns. A sender that is
// not its contents is refused rather than served.
function fromWindow(event: { sender: WebContents }): BrowserWindowType | null {
  return win && !win.isDestroyed() && event.sender === win.webContents ? win : null;
}

// A window larger than the display it opens on puts its own title bar off
// screen, and a frameless one takes the only way to move it along with it.
function fitted(): { width: number; height: number } {
  const area = screen.getPrimaryDisplay().workAreaSize;
  return {
    width: Math.max(MIN_SIZE.width, Math.min(DEFAULT_SIZE.width, area.width)),
    height: Math.max(MIN_SIZE.height, Math.min(DEFAULT_SIZE.height, area.height)),
  };
}

function createWindow(): BrowserWindowType {
  return new BrowserWindow({
    ...fitted(),
    ...appIcon(),
    minWidth: MIN_SIZE.width,
    minHeight: MIN_SIZE.height,
    // Shown once it has been measured against the screen it landed on; sizing
    // a visible window makes the correction a flicker.
    show: false,
    title: "Ultrac",
    webPreferences: {
      preload: path.join(here, "preload.cjs"),
      // Stated rather than left to the defaults: this is the list a reviewer
      // reads to know what the renderer may reach. Sandboxed, because the
      // renderer never needs node — every kernel question goes through the
      // bridge — and it is also why preload.cjs is CommonJS (Electron's ESM
      // matrix: sandboxed preloads cannot use ESM imports).
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
      webviewTag: false,
      webSecurity: true,
    },
  });
}

// The page may only ever be the file it was loaded from. A navigation
// anywhere else is refused rather than followed, and a second window is never
// opened at all — a link leaves through the platform opener or not at all
// (Reasonix's guard, minus the origin: there is nothing to compare against
// but the page's own file URL).
function guardPage(contents: WebContents, pageFile: string): void {
  const page = pathToFileURL(pageFile).toString();
  contents.on("will-navigate", (event, url) => {
    if (!url.startsWith(page)) event.preventDefault();
  });
  contents.setWindowOpenHandler(() => ({ action: "deny" }));
}

// The SPA when it exists; the placeholder when it does not (P-S1 runs before
// P-S2 builds desktop/shell-spa/dist). A dist that fails to load mid-flight
// (deleted between the check and the read) falls back the same way rather
// than leaving the window blank.
async function loadPage(target: BrowserWindowType, locale: string): Promise<void> {
  const dir = pageDir(where);
  const index = path.join(dir, "index.html");
  if (hasPage(dir)) {
    try {
      await target.loadFile(index);
      guardPage(target.webContents, index);
      return;
    } catch (err) {
      logs?.shell.line(`shell: the SPA failed to load (${(err as Error).message}); showing the placeholder`);
    }
  }
  await target.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(missingPage(locale))}`);
}

// Kernel events reach the page through the bridge. Until the page has
// finished loading they wait here, oldest first, capped.
function forwardEvent(event: RpcEvent): void {
  if (!rendererReady) {
    buffered.push(event);
    if (buffered.length > EVENT_BUFFER_LIMIT) buffered.shift();
    return;
  }
  win?.webContents.send("kernel:event", event);
}

function flushEvents(): void {
  const pending = buffered;
  buffered = [];
  for (const event of pending) win?.webContents.send("kernel:event", event);
}

// host.ts owns the retry matrix (a kernel that died or could not be spawned
// within seconds is tried once more; a slow kernel is waited for). This
// wrapper is the shell's half of it: the starting window, the logs, and the
// record of the child for the lease.
async function launchKernelWithFeedback(): Promise<KernelHandle> {
  const binary = kernelBinary(where);
  const args = kernelArgs({ cwd: kernelCwd() });
  logs?.shell.line(`host: starting ${binary} ${args.join(" ")}`);
  const began = Date.now();
  try {
    const handle = await launchKernel({
      binary,
      args,
      timeoutMs: readyTimeoutMs(),
      onSlow: () => {
        logs?.shell.line("host: no ready probe yet; showing the starting window");
        if (starting || quitting) return;
        const slow = showStarting(app.getLocale());
        starting = slow;
        slow.once("closed", () => {
          if (starting !== slow) return;
          starting = null;
          logs?.shell.line("host: starting window closed; ending the launch");
          app.quit();
        });
      },
      onStderr: (text) => {
        logs?.host.raw(text);
        process.stderr.write(text);
      },
      onExit: (code, signal) => {
        logs?.shell.line(`host: exited code=${code} signal=${signal}${handshaken ? "" : " before its ready probe"}`);
        // Before the probe the launch itself fails, and boot's catch owns
        // telling the person why; quitting here would race that dialog.
        powerGuard?.close();
        if (handshaken && code !== 0 && !quitting) app.quit();
      },
      shouldStop: () => quitting,
    });
    kernel = handle;
    handle.child.on("error", (err: Error) => logs?.shell.line(`host: spawn failed: ${err.message}`));
    handle.child.stderr?.on("close", () => logs?.host.flush());
    logs?.shell.line(`host: ready after ${Date.now() - began} ms (session ${handle.state.sessionId})`);
    return handle;
  } catch (err) {
    const hostError = err as HostStartError;
    logs?.shell.line(`host: launch failed (${hostError.code}, ${hostError.attempts} attempt(s)): ${hostError.message}`);
    throw hostError;
  }
}

async function boot(): Promise<void> {
  const ready = await launchKernelWithFeedback();
  handshaken = true;
  host = hostClientFor(ready.client);

  // Events: to the page (buffered until it has loaded) and to the power
  // guard, which asks get_state on its own poll but takes the hint when a
  // turn starts or ends rather than waiting up to five seconds for it.
  host.onEvent((event) => {
    forwardEvent(event);
    if (event.type === EvMessageStart || event.type === EvAgentEnd) void powerGuard?.refresh();
  });

  powerGuard = createPowerGuard({
    blocker: powerSaveBlocker,
    running: async () => {
      const state = await host?.getState({ timeoutMs: CALL_TIMEOUT_MS });
      return state ? { streaming: state.isStreaming } : null;
    },
    enabled: () => keepsAwake(loadPrefs(prefsFile(userData()))),
  });
  powerGuard.begin();

  win = createWindow();
  closeStarting();
  const target = win;
  installContextMenu(target.webContents, target, uiLang);
  installFullScreenKey(target.webContents, target);
  installReload(target.webContents, target);
  target.once("ready-to-show", () => target.show());
  target.webContents.once("did-finish-load", () => {
    // The page's subscriptions exist by now (module scripts run before the
    // load event), so everything buffered during the load is delivered and
    // the initialization payload follows.
    rendererReady = true;
    flushEvents();
    const state = kernel?.state as RpcSessionState | undefined;
    if (state) target.webContents.send(SESSION_LOADED, state);
    if (!app.isPackaged && process.env.ULTRAC_SHELL_DEVTOOLS === "1") {
      target.webContents.openDevTools({ mode: "bottom" });
    }
  });

  await loadPage(target, app.getLocale());
  logs?.shell.line(`shell: window loaded (page dir ${pageDir(where)})`);
}

// -- IPC: the four verbs preload.cjs exposes, plus the page's preferences ---

// The bridge's one command channel. The page names a command; this process
// owns the client, so it stamps nothing (RpcClient owns ids) and answers with
// the response or with why there was none. A window that is not the one this
// launch owns is refused.
ipcMain.handle("kernel:call", async (event, command: unknown) => {
  if (!fromWindow(event)) return { ok: false as const, code: "refused" as const, message: "no window" };
  const cmd = command as Partial<RpcCommand> | null;
  if (!cmd || typeof cmd.type !== "string") {
    return { ok: false as const, code: "refused" as const, message: "a command needs a type" };
  }
  const client = host;
  if (!client) {
    return { ok: false as const, code: "exited" as const, message: "the kernel is not up" };
  }
  try {
    const response = await client.call({ ...cmd, id: undefined }, { timeoutMs: CALL_TIMEOUT_MS });
    return {
      ok: true as const,
      command: response.command,
      success: response.success,
      data: response.data,
      error: response.error,
    };
  } catch (err) {
    const code = (err as { code?: string }).code;
    return {
      ok: false as const,
      code: (code === "timeout" || code === "spawn" ? code : "exited") as "timeout" | "spawn" | "exited",
      message: (err as Error).message,
    };
  }
});

// Links come out of model output, so the platform opener must never be handed
// a scheme of its choosing.
ipcMain.handle("shell:open-external", (event, raw: unknown) => {
  if (!fromWindow(event)) return;
  const target = externalTarget(raw);
  if (target) return shell.openExternal(target);
});

// -- identity, lock, logs ------------------------------------------------

// Named before any path is derived from it: userData hangs off the app name,
// and a name that depended on how this was launched would put two launches of
// the same install on two profiles — and so on two locks.
app.setName("Ultrac");

// Claimed before anything is created: the profile decides which launches
// share a lock, and Chromium reads it the moment the app is ready. A launch
// that does not get the lock has to leave without spawning a kernel of its
// own — the one already running owns those session files.
const identity = instanceID(kernelBinary(where));
app.setPath("userData", profileFor(app.getPath("userData"), identity));
// A launch from a shortcut has no console, so this file is the only place a
// failure before the window can be read back from.
logs = openLogs(userData());
logs.shell.line(
  `shell: start version=${app.getVersion()} packaged=${app.isPackaged} platform=${process.platform}/${process.arch} ` +
    `instance=${identity || "(unknown)"} argv=${JSON.stringify(redactArgv(process.argv))}`,
);
const primary = app.requestSingleInstanceLock();
if (!primary) {
  // The ordinary second launch: the running instance raises its own window.
  logs.shell.line(`shell: another instance holds the lock (instance=${identity || "(unknown)"}); leaving`);
  app.quit();
} else {
  app.on("second-instance", showWindow);
  // macOS reopens an existing app through activation, without a second process.
  app.on("activate", showWindow);
}

// Only the shell window may read or write the page's preferences; the profile
// directory is settled by the time any page asks.
registerPrefs(
  ipcMain,
  () => prefsFile(userData()),
  (event) => fromWindow(event as { sender: WebContents }) !== null,
  () => {
    installApplicationMenu(uiLang);
    void powerGuard?.refresh();
  },
);

// showWindow brings it back from wherever it went. Show alone is a no-op on a
// window that is merely buried, so the focus is what actually raises it.
function showWindow(): void {
  if (starting && !starting.isDestroyed()) starting.focus();
  if (quitting || !win || win.isDestroyed()) return;
  if (win.isMinimized()) win.restore();
  win.show();
  win.focus();
}

app.whenReady().then(() => {
  if (!primary) return;
  installApplicationMenu(uiLang);
  boot().catch((err: HostStartError | Error) => {
    if (quitting) {
      logs?.shell.line(`startup ended by quit: ${err.message}`);
      return;
    }
    void hostLastWords().then((words) => {
      if (quitting) {
        logs?.shell.line(`startup ended by quit: ${err.message}`);
        return;
      }
      if (!logs) return;
      failStartup({ app, dialog, logs, locale: app.getLocale() }, err, words);
    });
  });
});

// A child's exit can be reported before its stderr has drained, and what it
// said last is usually the reason it gave up. Bounded, for a host still
// running.
async function hostLastWords(): Promise<string> {
  if (handshaken || !kernel) return "";
  const stderr = kernel.child.stderr;
  if (!stderr || stderr.closed) return logs?.host.tail() ?? "";
  await new Promise((resolve) => {
    const timer = setTimeout(resolve, 1_000);
    stderr.once("close", () => {
      clearTimeout(timer);
      resolve(undefined);
    });
  });
  return logs?.host.tail() ?? "";
}

// Closing this end of the pipe is what tells the kernel to drain. Without it
// a session file is left being written by a process nobody is holding open.
// What this launch is holding, for a test that drives the real shell rather
// than a copy of it.
export function current(): { win: BrowserWindowType | null; host: HostClient | null } {
  return { win, host };
}

app.on("before-quit", () => {
  quitting = true;
  logs?.shell.line("shell: quitting");
  powerGuard?.close();
  // The lease, then the grace period: a kernel that honours stdin EOF exits
  // in milliseconds; one that does not must not outlive the shell.
  const child = kernel?.child;
  host?.close();
  if (child && !child.killed) {
    const timer = setTimeout(() => child.kill(), LEASE_GRACE_MS);
    timer.unref();
  }
});

// Before the probe the only window is the starting one, and the launch
// decides for itself whether to go on; after it, the last window closing is
// the person saying they are done.
app.on("window-all-closed", () => {
  if (handshaken) app.quit();
});
