// layout.test.ts — where the kernel binary and the page live, in both
// layouts. Packaged, both sit in resources/ beside app.asar rather than
// inside it: a child process cannot be spawned from an archive, and the SPA
// is read off the filesystem. Dev reads them from the working tree.

import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { hasPage, kernelBinary, pageDir } from "../src/layout.ts";

const dev = { packaged: false, resourcesPath: "", dirname: "C:\\repo\\desktop\\shell\\src", platform: "win32", env: {} };
const packed = { packaged: true, resourcesPath: "C:\\resources", dirname: "C:\\resources\\app.asar.unpacked", platform: "win32", env: {} };

test("the env override wins over every layout", () => {
  assert.equal(kernelBinary({ ...dev, env: { ULTRAC_BIN: "D:\\elsewhere\\ultrac.exe" } }), "D:\\elsewhere\\ultrac.exe");
  assert.equal(pageDir({ ...dev, env: { ULTRAC_SHELL_PAGE: "D:\\elsewhere\\dist" } }), "D:\\elsewhere\\dist");
});

test("the binary is named for the platform and read from resources when packaged", () => {
  assert.equal(kernelBinary(packed), path.join("C:\\resources", "bin", "ultrac.exe"));
  assert.equal(kernelBinary({ ...packed, platform: "linux" }), path.join("C:\\resources", "bin", "ultrac"));
});

test("dev reads the workspace's bin/ — one default, owned by host.ts", () => {
  // resolveKernelBinary ignores `where` and walks up from this module's own
  // location (src/ → repo root), so the dev default is the repo's bin/, not
  // anything inside the shell's own directory.
  const resolved = kernelBinary(dev);
  assert.match(resolved, /bin[\\/]ultrac\.exe$/);
  assert.ok(!resolved.includes("desktop"), "the dev default is the repo's bin/, above desktop/");
  assert.ok(!resolved.includes("shell"), "and not the shell's own directory");
});

test("the page lives in shell-spa/dist in dev and resources/spa when packaged", () => {
  assert.equal(pageDir(dev), path.join("C:\\repo\\desktop", "shell-spa", "dist"));
  assert.equal(pageDir(packed), path.join("C:\\resources", "spa"));
});

test("a dist without index.html has no page, and one with it does", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ultrac-layout-"));
  assert.equal(hasPage(dir), false);
  fs.writeFileSync(path.join(dir, "index.html"), "<html></html>");
  assert.equal(hasPage(dir), true);
  assert.equal(hasPage(path.join(dir, "nope")), false);
  fs.rmSync(dir, { recursive: true, force: true });
});
