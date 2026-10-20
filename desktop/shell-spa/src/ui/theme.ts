// theme.ts — the page's theme preference. The shell keeps it on disk
// (prefs.ts watches every ultrac- key in localStorage), and index.html's
// pre-paint script applies it before the first frame; this module is the
// runtime half: it writes the key and moves data-theme on <html>.
//
// "system" removes the attribute and lets the CSS media query decide —
// which is why there are three states and not two.

import { useCallback, useState } from "react";

const KEY = "ultrac-theme";

export type ThemeChoice = "system" | "light" | "dark";

export function readTheme(): ThemeChoice {
  try {
    const value = localStorage.getItem(KEY);
    if (value === "light" || value === "dark") return value;
  } catch {
    // Storage unavailable: the system preference stands.
  }
  return "system";
}

export function applyTheme(choice: ThemeChoice): void {
  const root = document.documentElement;
  if (choice === "system") delete root.dataset.theme;
  else root.dataset.theme = choice;
  try {
    if (choice === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, choice);
  } catch {
    // A theme that does not persist is one relaunch of lost state.
  }
}

/** The theme state plus a cycler (system → light → dark → system). */
export function useTheme(): [ThemeChoice, () => void] {
  const [choice, setChoice] = useState<ThemeChoice>(readTheme);
  const cycle = useCallback(() => {
    setChoice((current) => {
      const next: ThemeChoice = current === "system" ? "light" : current === "light" ? "dark" : "system";
      applyTheme(next);
      return next;
    });
  }, []);
  return [choice, cycle];
}
