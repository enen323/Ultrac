// powerguard.ts — hold the machine awake while a turn is in flight. The
// structure is Reasonix's powerguard.js; the §3.2 change is where the answer
// comes from. There it polled the kernel's HTTP tray endpoint for a pane
// count; here running() is one get_state over the pipe, and the shape it
// answers with is the session's isStreaming (plan §3.2: get_state /
// message_start / agent_end feed the guard).
//
// The semantics are kept exactly: streaming takes the block at once; letting
// go needs IDLE_PASSES clean not-streaming answers in a row, so a turn that
// ends and another that starts do not flap it; a pass that could not be
// answered (kernel unreachable) neither takes it nor counts toward letting
// go, and a kernel that never answers again cannot hold the block forever
// (SILENT_PASSES).

const POLL_MS = 5000;
const KIND = "prevent-app-suspension";
export const PREF_KEY = "ultrac-keep-awake";
// Consecutive idle answers before the block is let go: about fifteen seconds,
// so a turn that ends and another that starts do not flap it.
const IDLE_PASSES = 3;
// Consecutive passes with no usable answer before the block is let go anyway:
// about a minute, so a kernel that never answers again cannot hold it forever.
const SILENT_PASSES = 12;

/** The subset of Electron's powerSaveBlocker this module uses. */
export interface PowerBlocker {
  start: (kind: string) => number;
  stop: (id: number) => void;
  isStarted: (id: number) => boolean;
}

/** What running() answers: whether a turn is in flight, or null when unknown. */
export interface RunningAnswer {
  streaming?: boolean;
}

// The page stores the preference as "off" or "on"; anything else is the default.
export function keepsAwake(prefs: Record<string, string> | null | undefined): boolean {
  return prefs?.[PREF_KEY] !== "off";
}

export interface PowerGuard {
  refresh: () => Promise<void>;
  held: () => boolean;
  begin: () => void;
  close: () => void;
}

export function createPowerGuard({
  blocker,
  running,
  enabled,
  setInterval: every = setInterval,
  clearInterval: stop = clearInterval,
}: {
  blocker: PowerBlocker;
  running: () => Promise<RunningAnswer | null>;
  enabled: () => boolean;
  setInterval?: (fn: () => void, ms: number) => unknown;
  clearInterval?: (handle: unknown) => void;
}): PowerGuard {
  let held: number | null = null;
  let pass = 0;
  let timer: unknown = null;
  let closed = false;
  let idle = 0;
  let silent = 0;

  const hold = () => {
    if (held !== null && !blocker.isStarted(held)) held = null;
    if (held === null) held = blocker.start(KIND);
  };
  const release = () => {
    idle = 0;
    silent = 0;
    if (held === null) return;
    if (blocker.isStarted(held)) blocker.stop(held);
    held = null;
  };

  async function refresh() {
    if (closed) return;
    const mine = ++pass;
    if (!enabled()) {
      release();
      return;
    }
    let answer: RunningAnswer | null = null;
    try {
      answer = await running();
    } catch {
      answer = null;
    }
    if (mine !== pass || closed) return;
    const streaming = typeof answer?.streaming === "boolean" ? answer.streaming : null;
    if (streaming === true) {
      idle = 0;
      silent = 0;
      hold();
    } else if (streaming === false) {
      silent = 0;
      if (held === null || ++idle >= IDLE_PASSES) release();
    } else if (held !== null && ++silent >= SILENT_PASSES) {
      release();
    }
  }

  return {
    refresh,
    held: () => held !== null,
    begin() {
      if (timer) return;
      timer = every(() => void refresh(), POLL_MS);
      (timer as { unref?: () => void }).unref?.();
      void refresh();
    },
    close() {
      closed = true;
      pass++;
      if (timer) stop(timer);
      timer = null;
      release();
    },
  };
}

export { POLL_MS, KIND, IDLE_PASSES, SILENT_PASSES };
