// fullscreen.ts — F11 where there is no menu to carry it. A structure copy of
// Reasonix's fullscreen.js (plan §3.1): off macOS the shell installs no
// application menu, and F11 is otherwise bound only through a menu role.
// macOS keeps its own control on the title bar.

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

/** The subset of a WebContents this module uses. */
export interface ContentsLike {
  on: (channel: string, listener: (event: { preventDefault: () => void }, input: InputLike) => void) => void;
}

/** The subset of a BrowserWindow this module uses. */
export interface WindowLike {
  setFullScreen: (flag: boolean) => void;
  isFullScreen: () => boolean;
}

export function installFullScreenKey(
  contents: ContentsLike,
  window: WindowLike,
  platform = process.platform,
): void {
  if (platform === "darwin") return;
  contents.on("before-input-event", (event, input) => {
    if (input.type !== "keyDown" || input.key !== "F11" || input.isAutoRepeat) return;
    if (input.control || input.alt || input.shift || input.meta) return;
    event.preventDefault();
    window.setFullScreen(!window.isFullScreen());
  });
}
