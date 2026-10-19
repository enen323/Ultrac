// links.test.ts — where a link is allowed to go. Links come out of model
// output, so this is a security boundary, not a convenience: the platform
// opener must never be handed a scheme of its choosing.

import { test } from "node:test";
import assert from "node:assert/strict";
import { externalTarget } from "../src/links.ts";

test("http and https are the whole allowed set", () => {
  assert.equal(externalTarget("https://example.com/a?b=c#d"), "https://example.com/a?b=c#d");
  assert.equal(externalTarget("http://127.0.0.1:8080/x"), "http://127.0.0.1:8080/x");
});

test("every other scheme is refused, including the dangerous familiar ones", () => {
  for (const raw of [
    "file:///etc/passwd",
    "javascript:alert(1)",
    "data:text/html,<script>alert(1)</script>",
    "mailto:someone@example.com",
    "ms-excel:ofe|u|",
    "vscode://extension/x",
    "chrome://settings",
    "not a url at all",
    "",
    null,
    undefined,
    42,
  ]) {
    assert.equal(externalTarget(raw), null, `${String(raw)} must be refused`);
  }
});
