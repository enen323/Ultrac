// layout.ts — where the two things this shell launches live. A structure copy
// of Reasonix's layout.js (plan §5, lesson 2), cut to what Ultrac has: no
// computer helper, no separate page server. Packaged, both sit in resources/
// beside app.asar rather than inside it — a child process cannot be spawned
// from an archive, and the SPA is read off the filesystem. Dev reads them
// from the working tree (plan §7: desktop/shell-spa/dist).
//
// The kernel binary resolution delegates to host.ts's resolveKernelBinary
// for the dev case so there is exactly one dev default; this file adds the
// packaged case and the page.

import { existsSync } from "node:fs";
import path from "node:path";
import { resolveKernelBinary } from "./host.ts";

/** Everything about this launch the two resolvers need. */
export interface Where {
  packaged: boolean;
  resourcesPath: string;
  dirname: string;
  platform: string;
  env: NodeJS.ProcessEnv;
}

function exeName(platform: string): string {
  return platform === "win32" ? "ultrac.exe" : "ultrac";
}

export function kernelBinary(where: Where): string {
  if (where.env.ULTRAC_BIN) return where.env.ULTRAC_BIN;
  if (where.packaged) return path.join(where.resourcesPath, "bin", exeName(where.platform));
  return resolveKernelBinary();
}

export function pageDir(where: Where): string {
  if (where.env.ULTRAC_SHELL_PAGE) return where.env.ULTRAC_SHELL_PAGE;
  if (where.packaged) return path.join(where.resourcesPath, "spa");
  // src/ → desktop/shell/ → desktop/ → shell-spa/dist
  return path.join(where.dirname, "..", "..", "shell-spa", "dist");
}

// Whether the SPA has been built. When it has not, the main window shows
// starting.ts's missingPage instead of failing to load (P-S1 runs before the
// SPA exists; plan §6).
export function hasPage(dir: string): boolean {
  return existsSync(path.join(dir, "index.html"));
}
