// main.tsx — the entry. Three things happen here and nothing else:
//
//   1. decide whether the page is inside the shell (window.ultrac exists —
//      preload.cjs put it there) and say so plainly when it is not, instead
//      of failing mysteriously one command at a time;
//   2. build the one SessionStore the app subscribes to. It is built here,
//      outside the render, so React's StrictMode double-render cannot build
//      two of them (each would subscribe to the same bridge and double every
//      event);
//   3. arm the boot screen's patience cap and hand the window over.
//
// The dev-server case (vite dev in a plain browser) lands in branch 1: there
// is no kernel behind a browser tab, and pretending otherwise would only
// produce a page of failed calls.

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { bridgeOf, inShell } from "./port/bridge.ts";
import { SessionStore } from "./state/session.ts";
import { App } from "./ui/App.tsx";
import { armBootCap, dismissBoot } from "./ui/boot.ts";
import "./styles/tokens.css";
import "./styles/app.css";

const root = document.getElementById("root")!;

if (!inShell(window)) {
  // No bridge: the page is a preview, not the app. The shell is what spawns
  // the kernel (docs/desktop-shell-plan.md §3), and a browser cannot.
  dismissBoot();
  createRoot(root).render(
    <div className="no-shell">
      <span className="big">Ultrac</span>
      <span>这个页面要在 Ultrac 桌面壳里运行——壳负责拉起内核（<code>ultrac --mode rpc</code>）并桥接对话。</span>
      <span>
        开发预览：<code>npm run dev</code> 在这里只能看界面；完整对话请运行壳（<code>desktop/shell</code> 的 <code>pnpm start</code>）。
      </span>
    </div>,
  );
} else {
  const bridge = bridgeOf(window);
  const store = new SessionStore(bridge);
  armBootCap();
  createRoot(root).render(
    <StrictMode>
      <App store={store} bridge={bridge} />
    </StrictMode>,
  );
}
