// shelllog.test.ts — what may reach disk. The redaction rules are the part
// worth pinning: a secret that survives into shell.log or host.log is this
// process's fault, and every rule below is a shape a real log line takes
// (JSON from the kernel's stderr, a Go %v dump, an Authorization header, a
// URL with userinfo, an argv echo).

import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { MAX_BYTES, redact, redactArgv, rotatingLog, startupFailure } from "../src/shelllog.ts";

test("a value this process was handed is removed wherever it appears", () => {
  assert.equal(redact("the token is sk-live-12345 ok", ["sk-live-12345"]), "the token is [redacted] ok");
  assert.equal(redact("nothing secret here", ["sk-live-12345"]), "nothing secret here");
  // An empty secret would split on nothing and corrupt every line.
  assert.equal(redact("keep me", ["", "keep me"]), "[redacted]");
});

test("a field a structure names as secret gives up its value, not its name", () => {
  assert.equal(redact('{"api_key":"sk-123","model":"x"}'), '{"api_key":"[redacted]","model":"x"}');
  assert.equal(redact("token: abc123"), "token: [redacted]");
  assert.equal(redact("PASSWORD=hunter2"), "PASSWORD=[redacted]");
  assert.equal(redact('"authorization": "Bearer x"'), '"authorization": "[redacted]"');
  // The secret word may be the whole identifier or its last segment.
  assert.equal(redact("my-secret: 1"), "my-secret: [redacted]");
  // ...and a name that merely contains one is not a secret.
  assert.equal(redact("keyboard: hello"), "keyboard: hello");
  assert.equal(redact("sort_key: 3"), "sort_key: 3");
  assert.equal(redact("token.json: ok"), "token.json: ok");
});

test("a scheme word followed by its credential is redacted wherever it is quoted", () => {
  // The credential goes first (the scheme rule), then the field rule sees
  // "Authorization: Bearer [redacted]" and takes the scheme word too — it is
  // the value of a secret-named field. Over-redaction, never under: the
  // scheme is recoverable from the field name, a leaked credential is not.
  assert.equal(redact("Authorization: Bearer abc.def-123"), "Authorization: [redacted] [redacted]");
  // Without a secret-named field around it, the scheme word stays: only the
  // credential after it goes.
  assert.equal(redact("proxy used Basic aG9sYTpteXA="), "proxy used Basic [redacted]");
  assert.equal(redact("negotiating Digest d3adbeef"), "negotiating Digest [redacted]");
  // The rule needs the scheme word; a bare credential next to a secret-named
  // field is still taken by the field rule.
  assert.equal(redact("x-auth-token: abc"), "x-auth-token: [redacted]");
});

test("the password half of URL userinfo never survives", () => {
  assert.equal(redact("dialing https://user:s3cret@example.com/x"), "dialing https://user:[redacted]@example.com/x");
});

test("redactArgv keeps the shape of a command line and drops the secret values", () => {
  assert.deepEqual(redactArgv(["ultrac", "--mode", "rpc", "--model", "x"]), ["ultrac", "--mode", "rpc", "--model", "x"]);
  assert.deepEqual(redactArgv(["ultrac", "--api-key", "sk-123", "--model", "x"]), ["ultrac", "--api-key", "[redacted]", "--model", "x"]);
  assert.deepEqual(redactArgv(["ultrac", "--token=sk-123"]), ["ultrac", "--token=[redacted]"]);
  // A secret flag at the end of the line keeps its name.
  assert.deepEqual(redactArgv(["ultrac", "--api-key"]), ["ultrac", "--api-key"]);
  // A value that looks like a flag is not swallowed by the previous flag.
  assert.deepEqual(redactArgv(["ultrac", "--api-key", "--model", "x"]), ["ultrac", "--api-key", "--model", "x"]);
});

test("a rotating log appends, keeps a tail, and drops no line twice", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ultrac-shelllog-"));
  const file = path.join(dir, "test.log");
  const log = rotatingLog(file, [], { now: () => new Date("2026-10-19T10:00:00.000Z") });
  log.line("first");
  log.line("second");
  const text = fs.readFileSync(file, "utf8");
  assert.match(text, /2026-10-19T10:00:00\.000Z first\n/);
  assert.match(text, /second\n$/);
  assert.equal(log.tail(1), "2026-10-19T10:00:00.000Z second");
  fs.rmSync(dir, { recursive: true, force: true });
});

test("a log over its cap is rotated, and a rename that fails empties instead", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ultrac-shelllog-"));
  const file = path.join(dir, "test.log");
  // Pre-fill past the cap: the next append rotates before writing.
  fs.writeFileSync(file, "x".repeat(64), { mode: 0o600 });
  const log = rotatingLog(file, [], { maxBytes: 32, keep: 2 });
  log.line("after rotation");
  assert.match(fs.readFileSync(file, "utf8"), /after rotation/);
  assert.equal(fs.readFileSync(`${file}.1`, "utf8"), "x".repeat(64));
  fs.rmSync(dir, { recursive: true, force: true });
});

test("raw() redacts whole lines only, and a run without a break is dropped", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ultrac-shelllog-"));
  const file = path.join(dir, "host.log");
  const log = rotatingLog(file, ["sk-999"]);
  log.raw("token sk-999\nsecond line\n");
  log.raw("half a line with token sk-999");
  const text = fs.readFileSync(file, "utf8");
  assert.match(text, /token \[redacted\]\nsecond line\n/);
  // The half line is still pending, not written in halves.
  assert.ok(!text.includes("half a line"));
  log.raw(" done\n");
  assert.match(fs.readFileSync(file, "utf8"), /half a line with token \[redacted\] done\n/);
  fs.rmSync(dir, { recursive: true, force: true });
});

test("a startup failure names the log directory and follows the locale", () => {
  const en = startupFailure("en-US", "the kernel exited", "/logs", "exited");
  assert.equal(en.title, "Ultrac could not start");
  assert.match(en.detail, /the kernel exited/);
  assert.match(en.detail, /host\.log has the rest/);
  assert.match(en.detail, /\/logs/);
  const zh = startupFailure("zh-CN", "内核退出", "/logs", "spawn");
  assert.equal(zh.title, "Ultrac 无法启动");
  assert.match(zh.detail, /无法运行内核程序/);
  // An unknown code still tells the reason and where the logs are.
  const plain = startupFailure("en", "boom", "/logs", "mystery");
  assert.match(plain.detail, /boom/);
  assert.ok(!plain.detail.includes("mystery"));
});
