// editmenu.test.ts — the menus' shape, kept apart from the platform so it
// can be checked without one. Ported from Reasonix's unit suite (same
// assertions, same reason): the Edit menu is the only copy/paste a macOS
// WebContents has, and a right-click that offers nothing reads as a broken
// text editor.

import { test } from "node:test";
import assert from "node:assert/strict";
import { applicationMenuTemplate, contextTemplate, editMenuTemplate, menuInstaller } from "../src/editmenu.ts";

test("the context menu offers nothing where nothing can be edited", () => {
  assert.deepEqual(contextTemplate({ isEditable: false, selectionText: "", editFlags: {} }, "en"), []);
  assert.ok(contextTemplate({ isEditable: true, selectionText: "", editFlags: {} }, "en").length > 0);
  assert.ok(contextTemplate({ isEditable: false, selectionText: "picked", editFlags: {} }, "en").length > 0);
});

test("the context menu mirrors what the page says is possible", () => {
  const flags = { canUndo: true, canRedo: false, canCut: true, canCopy: true, canPaste: false, canSelectAll: true };
  const items = contextTemplate({ isEditable: true, selectionText: "x", editFlags: flags }, "en");
  const byRole = Object.fromEntries(items.filter((i) => i.role).map((i) => [i.role, i.enabled]));
  assert.deepEqual(byRole, { undo: true, redo: false, cut: true, copy: true, paste: false, selectAll: true });
});

test("the edit menu's labels follow the interface language, not the system's", () => {
  const params = { isEditable: true, selectionText: "", editFlags: {} };
  const labels = (lang: string) => contextTemplate(params, lang).filter((i) => i.role).map((i) => i.label);
  assert.deepEqual(labels("zh"), ["撤销", "重做", "剪切", "复制", "粘贴", "全选"]);
  assert.deepEqual(labels("en"), ["Undo", "Redo", "Cut", "Copy", "Paste", "Select All"]);
  assert.equal(editMenuTemplate("zh").label, "编辑");
});

test("the edit menu keeps everything the platform's own one carries", () => {
  const roles = (items: { role?: string; submenu?: unknown[] }[]) =>
    items.flatMap((i) => [i.role, ...(i.submenu ? roles(i.submenu as never) : [])]).filter(Boolean);
  const want = [
    "undo", "redo", "cut", "copy", "paste", "pasteAndMatchStyle", "delete", "selectAll",
    "showSubstitutions", "toggleSmartQuotes", "toggleSmartDashes", "toggleTextReplacement", "startSpeaking", "stopSpeaking",
  ];
  for (const lang of ["zh", "en"]) {
    assert.deepEqual(roles(editMenuTemplate(lang).submenu ?? []), want);
    const labels = (items: { label?: string; submenu?: unknown[] }[]) =>
      items.flatMap((i) => [i.label, ...(i.submenu ? labels(i.submenu as never) : [])]).filter((l) => l !== undefined);
    assert.ok(labels(editMenuTemplate(lang).submenu ?? []).every((l) => (l as string).length > 0));
  }
  assert.deepEqual(
    (editMenuTemplate("en").submenu ?? []).map((i) => i.role ?? i.type ?? i.label),
    ["undo", "redo", "separator", "cut", "copy", "paste", "pasteAndMatchStyle", "delete", "selectAll", "separator", "Substitutions", "Speech"],
  );
  assert.deepEqual(applicationMenuTemplate("zh").map((i) => i.role ?? "edit"), ["appMenu", "edit", "windowMenu"]);
});

test("the application menu is rebuilt when the language changes and not otherwise", () => {
  const built: unknown[] = [];
  const Menu = {
    buildFromTemplate: (t: unknown) => (built.push(t), { template: t }),
    setApplicationMenu: () => {},
    getApplicationMenu: () => null,
  };
  const install = menuInstaller(Menu, "darwin");
  let lang = "en";
  install(() => lang);
  install(() => lang);
  assert.equal(built.length, 1);
  lang = "zh";
  install(() => lang);
  assert.equal(built.length, 2);
});

test("off macOS there is no application menu to install", () => {
  let cleared = 0;
  const Menu = { buildFromTemplate: () => ({}), setApplicationMenu: () => cleared++, getApplicationMenu: () => null };
  const install = menuInstaller(Menu, "win32");
  assert.equal(install(() => "en"), null);
  assert.equal(cleared, 1);
});
