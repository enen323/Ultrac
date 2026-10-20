// boot.ts — the index.html boot screen's other half. The markup paints
// before any bundle exists (that is the point of it living in the HTML);
// this module arms its patience cap and takes it down when the app is ready.
// Reasonix's main.tsx does the same dance with its wordmark intro.

let capTimer: ReturnType<typeof setTimeout> | null = null;

/** After this long without a ready store, say so on the boot screen. */
export function armBootCap(ms = 8000): void {
  capTimer = setTimeout(() => {
    const hint = document.querySelector("#boot .hint");
    if (hint) hint.textContent = "内核没有回应——桌面壳可能没有正常启动";
  }, ms);
}

/** Remove the boot screen; the app's own render takes over. */
export function dismissBoot(): void {
  if (capTimer) clearTimeout(capTimer);
  document.getElementById("boot")?.remove();
}
