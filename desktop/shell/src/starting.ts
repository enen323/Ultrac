// starting.ts — the only sign of life while the kernel has not answered. A
// structure copy of Reasonix's starting.js (plan §3.1): until the ready
// probe has answered there is no window of the application's own to draw, so
// a small one says the launch is alive and, when it is slow, why that is
// normal.
//
// missingPage is the phase-1 stand-in for the SPA (plan §6, P-S2): when
// desktop/shell-spa/dist has not been built yet, the main window shows this
// instead of failing to load. The kernel is up and talking; there is simply
// nothing to draw the conversation with yet.

import { BrowserWindow } from "electron";
import type { BrowserWindow as BrowserWindowType } from "electron";

const TEXT = {
  en: ["Ultrac is starting", "The first start after installing or rebooting can take a minute."],
  zh: ["Ultrac 正在启动", "安装或重启电脑后的首次启动可能需要一分钟。"],
};

function pageFor(locale: unknown, title: string, note: string): string {
  return `<!doctype html><meta charset="utf-8"><title>${title}</title><style>
:root{color-scheme:light dark;--bg:#fafafa;--fg:#1c1c1e;--mute:#6b6b70}
@media(prefers-color-scheme:dark){:root{--bg:#1b1b1d;--fg:#ececee;--mute:#9a9aa0}}
body{margin:0;height:100vh;display:flex;flex-direction:column;justify-content:center;gap:8px;padding:0 28px;box-sizing:border-box;background:var(--bg);color:var(--fg);font:14px system-ui,sans-serif}
h1{margin:0;font-size:16px;font-weight:600}p{margin:0;color:var(--mute);line-height:1.5}
.bar{height:3px;margin-top:10px;border-radius:2px;background:var(--mute);opacity:.35;overflow:hidden;position:relative}
.bar::after{content:"";position:absolute;inset:0 70% 0 0;background:var(--fg);animation:run 1.4s ease-in-out infinite}
@keyframes run{from{transform:translateX(-100%)}to{transform:translateX(340%)}}
</style><h1>${title}</h1><p>${note}</p><div class="bar"></div>`;
}

export function startingPage(locale: unknown): string {
  const [title, note] = String(locale).toLowerCase().startsWith("zh") ? TEXT.zh : TEXT.en;
  return pageFor(locale, title, note);
}

export function showStarting(locale: unknown): BrowserWindowType {
  const win = new BrowserWindow({
    width: 380,
    height: 150,
    resizable: false,
    minimizable: false,
    maximizable: false,
    frame: true,
    title: "Ultrac",
    autoHideMenuBar: true,
    webPreferences: { sandbox: true, contextIsolation: true, nodeIntegration: false },
  });
  win.removeMenu();
  void win.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(startingPage(locale))}`);
  return win;
}

// Shown in the main window when the SPA has not been built (plan §7:
// desktop/shell-spa/dist arrives with P-S2). It says the kernel is alive and
// what to run, because a blank window reads as a crash.
export function missingPage(locale: unknown): string {
  const zh = String(locale).toLowerCase().startsWith("zh");
  const [title, note] = zh
    ? ["Ultrac 已就绪", "内核已启动并就绪，但页面还没有构建。在 desktop/shell-spa 里运行构建（pnpm build）后重启本壳。"]
    : ["Ultrac is ready", "The kernel is up and answering, but the page has not been built yet. Build it in desktop/shell-spa (pnpm build) and restart this shell."];
  return pageFor(locale, title, note);
}
