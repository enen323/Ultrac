// uilang.ts — the interface language the window draws in, decided from the
// same two facts the page uses: its saved choice (ultrac-lang) and, for
// anything but an explicit zh or en, the machine's first locale. The shell
// needs this answer before the page exists (the starting window, the menus),
// so it cannot be the page's decision alone (plan §3.1).

// A machine locale counts as Chinese for the common tags and subtags; the
// explicit "zh"/"en" preference is the only other input.
export function isChinese(tag: unknown): boolean {
  const s = String(tag ?? "").toLowerCase();
  return s.startsWith("zh") || s.startsWith("yue") || s.startsWith("cmn") || s.includes("hans") || s.includes("hant");
}

export function uiLanguage(prefs: Record<string, string> | null | undefined, machineLocale: unknown): "zh" | "en" {
  const want = String(prefs?.["ultrac-lang"] ?? "").trim().toLowerCase();
  if (want === "zh" || want === "en") return want;
  return isChinese(machineLocale) ? "zh" : "en";
}
