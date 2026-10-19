// hooks.mjs — the module customization hook that lets main.ts boot under
// plain node (test/main-boot.test.ts's headless smoke). Two redirections:
//
//   "electron"           → ./electron-stub.mjs, the fake Electron that also
//                          drives the boot and writes the report.
//   "node:child_process" → ./child-process-stub.mjs, which forces every spawn
//                          to the fake kernel. The importer check is what
//                          keeps the stub's own import of the real module
//                          from looping back into itself.
//
// Everything else resolves normally. Node 22.22 verified: resolve hooks can
// redirect node: builtins (experimented before this file existed).

export async function resolve(specifier, context, next) {
  if (specifier === "electron") {
    return { url: new URL("./electron-stub.mjs", import.meta.url).href, shortCircuit: true };
  }
  if (specifier === "node:child_process" && context.parentURL !== new URL("./child-process-stub.mjs", import.meta.url).href) {
    return { url: new URL("./child-process-stub.mjs", import.meta.url).href, shortCircuit: true };
  }
  return next(specifier, context);
}
