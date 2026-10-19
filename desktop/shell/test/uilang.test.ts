// uilang.test.ts — the language decision, decided from the same two facts
// the page uses: its saved choice and, for anything but an explicit zh or
// en, the machine's first locale.

import { test } from "node:test";
import assert from "node:assert/strict";
import { isChinese, uiLanguage } from "../src/uilang.ts";

test("an explicit preference wins over the machine", () => {
  assert.equal(uiLanguage({ "ultrac-lang": "en" }, "zh-CN"), "en");
  assert.equal(uiLanguage({ "ultrac-lang": "zh" }, "en-US"), "zh");
  // Anything that is not an explicit zh or en is not a preference.
  assert.equal(uiLanguage({ "ultrac-lang": "fr" }, "fr-FR"), "en");
  assert.equal(uiLanguage({ "ultrac-lang": "  ZH  " }, "en"), "zh");
  assert.equal(uiLanguage({}, "de-DE"), "en");
  assert.equal(uiLanguage(null, "zh-Hans-CN"), "zh");
});

test("a machine locale counts as Chinese for the common tags and subtags", () => {
  for (const tag of ["zh", "zh-CN", "zh-Hans", "zh-Hant-TW", "yue-Hant", "cmn-Hans"]) {
    assert.equal(isChinese(tag), true, `${tag} should count as Chinese`);
  }
  for (const tag of ["en", "en-US", "ja", "ko", "de", "", null, undefined]) {
    assert.equal(isChinese(tag), false, `${String(tag)} should not count as Chinese`);
  }
});
