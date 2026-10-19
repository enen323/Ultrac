// menu.ts — where the editmenu templates meet Electron. A structure copy of
// Reasonix's menu.js (plan §3.1). The logic lives in editmenu.ts (testable
// without Electron); this file is the wiring: it installs the application
// menu on macOS and pops the context menu on a right-click.
//
// Without an application menu a WebContents has no copy, paste or undo at
// all: macOS routes those shortcuts through it, and a window with none reads
// as a broken text editor. Elsewhere the bar would render inside the window,
// and those platforms bind the shortcuts themselves.

import { Menu } from "electron";
import type { BrowserWindow, WebContents } from "electron";
import { contextTemplate, menuInstaller } from "./editmenu.ts";

export const installApplicationMenu = menuInstaller(Menu as unknown as Parameters<typeof menuInstaller>[0]);

// A production build ships without one, so a right-click in a text field
// offered nothing at all. Roles rather than handlers: the clipboard work
// belongs to the platform, and editFlags is the page's own account of what
// is possible here.
export function installContextMenu(
  contents: WebContents,
  window: BrowserWindow,
  languageOf: () => string = () => "en",
): void {
  contents.on("context-menu", (_event, params: Parameters<typeof contextTemplate>[0]) => {
    const template = contextTemplate(params, languageOf());
    if (!template.length) return;
    Menu.buildFromTemplate(template as Parameters<typeof Menu.buildFromTemplate>[0]).popup({ window });
  });
}
