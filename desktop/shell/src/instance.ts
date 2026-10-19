// instance.ts — which launches are the same Ultrac. Reasonix asks its kernel
// (`-instance-id`), because there the data home is resolved from an
// environment and a platform default the shell does not own. Ultrac has no
// such flag, and adding one to the kernel for a browser-profile question
// would be the tail wagging the dog — so the shell answers from the one fact
// it does own: the binary it is about to drive. The data home follows the
// binary in both layouts (dev reads the workspace bin/, a packaged build
// reads resourcesPath), so the resolved path is a faithful install identity
// (plan §3.1).
//
// What this scopes: the Electron userData path, and through it the platform
// single-instance lock. Two homes are two installs and must not share either;
// a path that cannot be resolved leaves the default alone rather than
// guessing.

import { createHash } from "node:crypto";
import { realpathSync } from "node:fs";
import path from "node:path";

export function instanceID(binary: string): string {
  try {
    const real = realpathSync(binary);
    return createHash("sha256").update(real, "utf8").digest("hex").slice(0, 16);
  } catch {
    // A kernel that cannot be resolved is a launch that is going to fail with
    // a better message in a moment. Refusing to start here would report it as
    // "already running", which is the wrong thing to tell someone.
    return "";
  }
}

// profileFor puts this instance's browser profile under its own identity,
// which is also what scopes the platform lock. An unknown identity leaves
// the default alone.
export function profileFor(base: string, id: string): string {
  return id ? path.join(base, id) : base;
}
