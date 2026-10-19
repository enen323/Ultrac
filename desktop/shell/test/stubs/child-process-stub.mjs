// child-process-stub.mjs — the spawn the shell's host.ts gets. The real
// binary cannot be spawned in a test (on Windows a .mjs is not executable),
// so every spawn is redirected to the fake kernel under the real node. The
// options (stdio pipes, windowsHide) are preserved: they are what the pipe
// protocol runs on.

import { spawn as realSpawn } from "node:child_process";
import process from "node:process";

export function spawn(binary, args, options) {
  const kernel = process.env.ULTRAC_SHELL_FAKE_KERNEL;
  const scenario = process.env.ULTRAC_SHELL_FAKE_SCENARIO ?? "boot-events";
  const child = realSpawn(process.execPath, [kernel, scenario], options);
  globalThis.__kernelSpawned = true;
  // The lease's proof: the kernel exiting on stdin EOF is the whole point of
  // the smoke, so the exit is published where the driver can see it.
  child.on("exit", (code, signal) => {
    globalThis.__kernelExit = { code, signal };
  });
  return child;
}

// Everything else is the real module — nothing else in the shell's graph
// spawns, but a re-export keeps the shape honest.
export * from "node:child_process";
