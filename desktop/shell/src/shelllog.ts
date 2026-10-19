// shelllog.ts — the shell's own log and the redaction that protects it. A
// structure copied from Reasonix's shelllog.js: the rules about what may be
// written to disk are the ones a production shell learns the hard way, and
// they have nothing to do with which kernel sits on the other end of the
// pipe (plan §3.1).
//
// # What lands on disk
//
// Two rotating files under <userData>/logs: shell.log for this process's own
// account of a launch, host.log for the kernel's stderr. Both pass through
// redact() first. A launch from a shortcut has no console, so these files are
// the only place a failure before the window can be read back from — and the
// only place a secret can leak by accident.
//
// # Redaction
//
// Three kinds of knowledge: values this process was handed at runtime
// (addSecret), values a structure names as secret (`token: ...`,
// `Authorization: Bearer ...`, URL userinfo), and argv flags whose name says
// they carry one. A secret printed bare, with nothing naming it, is beyond
// what text can say — everything else is removed before a line is appended.

import fs from "node:fs";
import path from "node:path";

const MAX_BYTES = 1024 * 1024;
const KEEP = 3;
const TAIL_BYTES = 4096;
const TAIL_LINES = 5;
const PENDING_LIMIT = 16 * 1024;
const REDACTED = "[redacted]";
// A name is secret when the secret word is the whole identifier or its last
// `-`/`_`/`.` segment: keyboard, oauth, token.json and sort_key are not.
const SECRET_NAME =
  /(?:^|[-_.])(?:token|secret|password|passwd|credentials?|auth|authorization)$|(?:^|[-_.])(?:api|access|secret|private)[-_]?key$|^key$/i;
// A scheme word followed by its credential, as in an Authorization header.
const AUTH_SCHEME = /\b(Bearer|Basic|Digest)(\s+)[A-Za-z0-9._~+/=-]+/gi;
// The password half of URL userinfo: scheme://user:password@host.
const USERINFO = /\b([a-z][a-z0-9+.-]*:\/\/[^\s/:@]+:)[^\s/@]+@/gi;
// A field in JSON, Go %v, slog text, headers, and query strings, quoted or
// not, with its value quoted or bare; SECRET_NAME decides whether it is one.
const NAMED = /(["']?)([\w.-]+)\1(\s*[:=]\s*)("(?:[^"\\]|\\.)*"|'[^']*'|[^\s,;&}\])"']+)/g;

// redact removes every value this process knows to be secret, and every value
// a structure marks as one, before a line reaches disk.
export function redact(text: string, secrets: readonly string[] = []): string {
  let out = String(text);
  for (const secret of secrets) {
    if (secret) out = out.split(secret).join(REDACTED);
  }
  return redactNamed(out.replace(AUTH_SCHEME, `$1$2${REDACTED}`).replace(USERINFO, `$1${REDACTED}@`));
}

// A field that is not a secret gives up only its name, so a value such as a
// URL is still searched for the secret-named fields inside it.
function redactNamed(text: string): string {
  let out = "";
  let from = 0;
  NAMED.lastIndex = 0;
  for (let m = NAMED.exec(text); m; m = NAMED.exec(text)) {
    const [whole, q, name, sep, value] = m;
    const head = q + name + q + sep;
    if (!SECRET_NAME.test(name)) {
      NAMED.lastIndex = m.index + head.length;
      continue;
    }
    const mark = value[0] === '"' || value[0] === "'" ? `${value[0]}${REDACTED}${value[0]}` : REDACTED;
    out += text.slice(from, m.index) + head + mark;
    from = m.index + whole.length;
  }
  return out + text.slice(from);
}

// redactArgv keeps the shape of a command line and drops the values of flags
// whose name says they carry a secret, in both `--flag=value` and
// `--flag value`.
export function redactArgv(argv: readonly unknown[]): string[] {
  const out: string[] = [];
  for (let i = 0; i < argv.length; i++) {
    const arg = String(argv[i]);
    const flag = /^(--?[^=\s]+)(=.*)?$/.exec(arg);
    if (!flag || !SECRET_NAME.test(flag[1].replace(/^-+/, ""))) {
      out.push(arg);
      continue;
    }
    if (flag[2] !== undefined) {
      out.push(`${flag[1]}=${REDACTED}`);
      continue;
    }
    out.push(arg);
    if (i + 1 < argv.length && !String(argv[i + 1]).startsWith("-")) {
      out.push(REDACTED);
      i++;
    }
  }
  return out;
}

export interface RotatingLog {
  readonly file: string;
  /** One timestamped entry; a multi-line message such as a stack stays one entry. */
  line: (message: string) => void;
  /** Redaction needs whole lines: a pipe chunk can end inside a secret. */
  raw: (chunk: string) => void;
  flush: () => void;
  tail: (lines?: number) => string;
}

// A log is best-effort: a disk that refuses a write must never be what stops
// the launch it was meant to explain.
export function rotatingLog(
  file: string,
  secrets: readonly string[],
  { maxBytes = MAX_BYTES, keep = KEEP, now = () => new Date() }: { maxBytes?: number; keep?: number; now?: () => Date } = {},
): RotatingLog {
  const sizeOnDisk = () => {
    try {
      return fs.statSync(file).size;
    } catch {
      return 0;
    }
  };
  let size = sizeOnDisk();
  let tail = "";
  let pending = "";

  // A live file that could not be renamed (held open by a scanner on Windows)
  // is emptied instead, so the cap holds either way.
  const rotate = () => {
    for (let i = keep - 1; i >= 1; i--) {
      try {
        fs.renameSync(i === 1 ? file : `${file}.${i - 1}`, `${file}.${i}`);
      } catch {
        if (i === 1) {
          try {
            fs.truncateSync(file, 0);
          } catch {}
        }
      }
    }
    size = sizeOnDisk();
  };
  const append = (text: string) => {
    tail = (tail + text).slice(-TAIL_BYTES);
    const bytes = Buffer.byteLength(text);
    if (size > 0 && size + bytes > maxBytes) rotate();
    try {
      fs.appendFileSync(file, text, { mode: 0o600 });
      size += bytes;
    } catch {}
  };
  return {
    file,
    line: (message) => append(`${now().toISOString()} ${redact(message, secrets)}\n`),
    raw: (chunk) => {
      pending += chunk;
      const end = pending.lastIndexOf("\n");
      if (end >= 0) {
        append(redact(pending.slice(0, end + 1), secrets));
        pending = pending.slice(end + 1);
      }
      if (pending.length > PENDING_LIMIT) {
        append(`[${pending.length} characters without a line break dropped]\n`);
        pending = "";
      }
    },
    flush: () => {
      if (pending) append(`${redact(pending, secrets)}\n`);
      pending = "";
    },
    tail: (lines = TAIL_LINES) => tail.split(/\r?\n/).filter((l) => l.trim() !== "").slice(-lines).join("\n"),
  };
}

export interface ShellLogs {
  readonly dir: string;
  readonly shell: RotatingLog;
  readonly host: RotatingLog;
  redact: (text: string) => string;
  /** Take a value learned at runtime out of both files before it can reach them. */
  addSecret: (value: unknown) => void;
}

// openLogs puts shell.log and host.log under <userData>/logs. addSecret takes a
// value learned at runtime — for this shell, nothing: the pipe is the boundary
// and there is no credential to protect. It stays in the shape because the
// kernel's stderr can quote one anyway (an Authorization header echoed by a
// failing provider call), and a shell log that leaks it would be this
// process's fault.
export function openLogs(userData: string, options: { maxBytes?: number; keep?: number; now?: () => Date } = {}): ShellLogs {
  const dir = path.join(userData, "logs");
  try {
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  } catch {}
  const secrets: string[] = [];
  return {
    dir,
    shell: rotatingLog(path.join(dir, "shell.log"), secrets, options),
    host: rotatingLog(path.join(dir, "host.log"), secrets, options),
    redact: (text) => redact(text, secrets),
    addSecret: (value) => {
      if (typeof value === "string" && value.length >= 8 && !secrets.includes(value)) secrets.push(value);
    },
  };
}

const HINTS: Record<string, [string, string]> = {
  timeout: [
    "The kernel started but did not finish starting in time. Security software scanning a new or updated install is a common cause; starting Ultrac again usually works.",
    "内核已启动，但没有在限定时间内完成启动。安全软件扫描新安装或刚更新的程序是常见原因；再次启动通常可以。",
  ],
  exited: [
    "The kernel process stopped before it was ready. Anything it printed is above; host.log has the rest.",
    "内核进程在就绪前就停止了。它输出过的内容在上面，host.log 里有更多。",
  ],
  spawn: [
    "The kernel program could not be run. Security software blocking or locking it is a common cause.",
    "无法运行内核程序。安全软件拦截或占用它是常见原因。",
  ],
  rejected: [
    "The kernel answered the ready probe with an error. host.log has what it said.",
    "内核以错误回答了就绪探针。host.log 里有它说的内容。",
  ],
};

export function startupFailure(locale: string, reason: string, logDir: string, code?: string): { title: string; detail: string } {
  const zh = String(locale).toLowerCase().startsWith("zh");
  const hint = code ? HINTS[code]?.[zh ? 1 : 0] : undefined;
  const body = hint ? `${reason}\n\n${hint}` : reason;
  if (zh) {
    return { title: "Ultrac 无法启动", detail: `${body}\n\n日志位于：\n${logDir}` };
  }
  return { title: "Ultrac could not start", detail: `${body}\n\nLogs are in:\n${logDir}` };
}

export interface FailStartupDeps {
  app: { quit: () => void };
  dialog: { showErrorBox: (title: string, detail: string) => void };
  logs: Pick<ShellLogs, "shell" | "redact" | "dir">;
  locale: string;
}

// failStartup is the one way a launch that cannot come up ends: recorded, told
// to the person who started it, and then quit. hostOutput is the kernel's last
// words when it never finished starting, and is already redacted.
export function failStartup(deps: FailStartupDeps, err: unknown, hostOutput = ""): void {
  deps.logs.shell.line(`startup failed: ${(err as Error)?.stack || err}`);
  const tried = (err as { attempts?: number })?.attempts > 1 ? ` (after ${(err as { attempts: number }).attempts} attempts)` : "";
  const reason = [`${String((err as Error)?.message || err)}${tried}`, hostOutput].filter(Boolean).join("\n\n");
  const text = startupFailure(deps.locale, deps.logs.redact(reason), deps.logs.dir, (err as { code?: string })?.code);
  try {
    deps.dialog.showErrorBox(text.title, text.detail);
  } catch (e) {
    deps.logs.shell.line(`startup failure dialog: ${(e as Error)?.message || e}`);
  }
  deps.app.quit();
}

export { MAX_BYTES, KEEP };
