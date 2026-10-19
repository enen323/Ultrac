// appicon.ts — the window icon, per platform. A structure copy of Reasonix's
// appicon.js (plan §3.1): macOS takes the window icon from the bundle, never
// from the window, so naming one there is not a smaller version of the same
// thing — it is ignored. The two platforms that do read it want different
// formats: .ico carries the small sizes Windows draws in the taskbar, and
// GTK reads a single PNG.
//
// The assets themselves arrive with packaging (P-S3): until then the files
// are absent and the key is omitted, which leaves the platform default
// rather than a broken path. A shell with no icon must never background its
// window — hiding it would leave a running session with nothing to bring it
// back — which is also why the tray waits for the same assets (plan §3.3).

import { existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ASSETS = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "assets");

export function iconFile(platform: string): string | null {
  switch (platform) {
    case "win32":
      return "icon.ico";
    case "darwin":
      return null;
    default:
      return "icon.png";
  }
}

// appIcon is spread into the BrowserWindow options, so the key is absent
// rather than undefined where the platform owns the icon itself.
export function appIcon(platform = process.platform): { icon?: string } {
  const file = iconFile(platform);
  if (!file) return {};
  const resolved = path.join(ASSETS, file);
  return existsSync(resolved) ? { icon: resolved } : {};
}
