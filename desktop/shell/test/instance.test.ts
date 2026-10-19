// instance.test.ts — which launches are the same Ultrac. The identity
// scopes the Electron profile and, through it, the platform single-instance
// lock: two homes are two installs and must not share either.

import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { instanceID, profileFor } from "../src/instance.ts";

test("the identity follows the resolved binary, stably", () => {
  const id = instanceID(process.execPath);
  assert.match(id, /^[0-9a-f]{16}$/);
  assert.equal(instanceID(process.execPath), id, "the same binary is the same install");
  assert.notEqual(instanceID(path.join(path.dirname(process.execPath), "somewhere-else")), id);
});

test("a binary that cannot be resolved leaves the default profile alone", () => {
  assert.equal(instanceID(path.join(path.sep, "no", "such", "ultrac")), "");
  assert.equal(profileFor("C:\\profile", ""), "C:\\profile");
  assert.equal(profileFor("C:\\profile", "abc123"), path.join("C:\\profile", "abc123"));
});
