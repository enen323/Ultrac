// reload.ts — the recovery chords. A structure copy of Reasonix's reload.js
// (plan §3.1). A page gone blank, whether its tree unmounted or its renderer
// died, has no way back but reloading; the kernel owns every running turn and
// a loaded page finds it again through the history commands. F5 and
// Ctrl/Cmd+R belong to the page, so the recovery chord is the shifted one.

const CRASH_RELOADS = 3;
const CRASH_WINDOW_MS = 60_000;

/** The subset of a before-input-event input this module reads. */
export interface InputLike {
  type?: string;
  key?: string;
  code?: string;
  isAutoRepeat?: boolean;
  control?: boolean;
  alt?: boolean;
  shift?: boolean;
  meta?: boolean;
}

/** What a keypress means to the reload logic, or null when it is not one. */
export function reloadAction(input: InputLike, platform = process.platform): "reload" | "hard" | null {
  if (input.type !== "keyDown" || input.isAutoRepeat || input.alt) return null;
  const mod = platform === "darwin" ? input.meta && !input.control : input.control && !input.meta;
  const latin = typeof input.key === "string" && /^[a-z]$/i.test(input.key);
  const isR = latin ? input.key.toLowerCase() === "r" : input.code === "KeyR";
  if (mod && isR) return input.shift ? "hard" : "reload";
  if (!input.control && !input.meta && !input.shift && input.key === "F5") return "reload";
  return null;
}

/** The subset of a WebContents this module uses. */
export interface ContentsLike {
  on: (channel: string, listener: (event: { preventDefault: () => void }, input: InputLike) => void) => void;
  reload: () => void;
  reloadIgnoringCache: () => void;
  isCrashed: () => boolean;
}

/** The subset of a BrowserWindow this module uses. */
export interface WindowLike {
  isDestroyed: () => boolean;
  isVisible: () => boolean;
}

/** A render-process-gone detail, as far as this module reads it. */
export interface GoneDetail {
  reason?: string;
}

export function installPaneReload(contents: ContentsLike, platform = process.platform): void {
  contents.on("before-input-event", (event, input) => {
    const action = reloadAction(input, platform);
    if (!action) return;
    event.preventDefault();
    if (action === "hard") contents.reloadIgnoringCache();
    else contents.reload();
  });
}

// A dead renderer's input never reaches before-input-event, so the crash
// itself has to trigger the reload, or for a hidden window the next revive
// before it is shown. A page that dies on every load stops being reloaded.
export function installReload(
  contents: ContentsLike,
  window: WindowLike,
  { platform = process.platform, now = Date.now }: { platform?: string; now?: () => number } = {},
): { revive: () => void } {
  contents.on("before-input-event", (event, input) => {
    if (reloadAction(input, platform) !== "hard") return;
    event.preventDefault();
    contents.reload();
  });

  let recent: number[] = [];
  const recover = () => {
    const at = now();
    recent = recent.filter((t) => at - t < CRASH_WINDOW_MS);
    if (recent.length >= CRASH_RELOADS) return;
    recent.push(at);
    contents.reload();
  };
  contents.on("render-process-gone", (_event: unknown, details: GoneDetail) => {
    if (details.reason === "clean-exit" || window.isDestroyed() || !window.isVisible()) return;
    recover();
  });
  return {
    revive: () => {
      if (!window.isDestroyed() && contents.isCrashed()) recover();
    },
  };
}

export { CRASH_RELOADS, CRASH_WINDOW_MS };
