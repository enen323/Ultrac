// custom-model.test.ts — the dialog's pure logic (custom-model-plan.md P4):
// provider id derivation, the validation rules (a mirror of the kernel's
// protocol layer, so a refusal costs no round trip), the dialect guess,
// and the spec the command finally carries. No DOM and no kernel: node
// --test runs this file directly, which is why the logic lives in a .ts
// of its own rather than inside the .tsx.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  API_ANTHROPIC,
  API_OPENAI,
  DEFAULT_CONTEXT_WINDOW,
  DEFAULT_MAX_TOKENS,
  THINKING_ORDER,
  WINDOW_OPTIONS,
  buildSpec,
  deriveProviderId,
  effectiveProvider,
  emptyForm,
  inferAPI,
  toggleThinkingLevel,
  validateForm,
  type CustomModelForm,
} from "../src/ui/custom-model.ts";

/** A filled form, broken one field at a time by the validation tests. */
function form(overrides: Partial<CustomModelForm> = {}): CustomModelForm {
  return {
    ...emptyForm(),
    baseUrl: "https://api.deepseek.com/v1",
    apiKey: "sk-test",
    modelId: "deepseek-chat",
    ...overrides,
  };
}

// -- derivation -----------------------------------------------------------------

test("deriveProviderId: the host with dots as dashes", () => {
  assert.equal(deriveProviderId("https://api.deepseek.com/v1"), "api-deepseek-com");
  assert.equal(deriveProviderId("http://localhost:11434"), "localhost-11434");
  // A pasted bare host still derives — the scheme is supplied for the parse.
  assert.equal(deriveProviderId("api.example.com"), "api-example-com");
  // Case and whitespace do not change the id.
  assert.equal(deriveProviderId("  https://API.DeepSeek.Com  "), "api-deepseek-com");
  // Unparseable: no host, no id (the caller says so).
  assert.equal(deriveProviderId(""), "");
  assert.equal(deriveProviderId("   "), "");
});

test("inferAPI: anthropic-flavoured endpoints, openai otherwise", () => {
  assert.equal(inferAPI("https://api.deepseek.com/anthropic"), API_ANTHROPIC);
  assert.equal(inferAPI("https://anthropic.example.com/v1"), API_ANTHROPIC);
  assert.equal(inferAPI("https://api.deepseek.com/v1"), API_OPENAI);
  assert.equal(inferAPI(""), API_OPENAI);
});

test("effectiveProvider: the explicit id wins, else the derived one", () => {
  assert.equal(effectiveProvider(form({ provider: "my-proxy" })), "my-proxy");
  assert.equal(effectiveProvider(form({ provider: "  my-proxy  " })), "my-proxy");
  assert.equal(effectiveProvider(form()), "api-deepseek-com");
  assert.equal(effectiveProvider(form({ baseUrl: "" })), "");
});

// -- validation -----------------------------------------------------------------

test("validateForm: a complete form passes", () => {
  assert.deepEqual(validateForm(form()), {});
  // An explicit legal provider id and an explicit dialect are fine too.
  assert.deepEqual(validateForm(form({ provider: "my-proxy", api: API_ANTHROPIC })), {});
});

test("validateForm: the required fields", () => {
  assert.ok(validateForm(form({ baseUrl: "" })).baseUrl);
  assert.ok(validateForm(form({ apiKey: "" })).apiKey);
  assert.ok(validateForm(form({ apiKey: "   " })).apiKey);
  assert.ok(validateForm(form({ modelId: "" })).modelId);
});

test("validateForm: the baseUrl must be http(s) with a host", () => {
  assert.ok(validateForm(form({ baseUrl: "ftp://api.deepseek.com" })).baseUrl);
  assert.ok(validateForm(form({ baseUrl: "https://" })).baseUrl);
  assert.ok(!validateForm(form({ baseUrl: "http://localhost:11434" })).baseUrl);
});

test("validateForm: an explicit provider id must be a legal models.json key", () => {
  // A "/" is set_model's separator and a space is not a key character;
  // the kernel refuses both, so the form refuses them first.
  assert.ok(validateForm(form({ provider: "deepseek/chat" })).provider);
  assert.ok(validateForm(form({ provider: "deep seek" })).provider);
  assert.ok(!validateForm(form({ provider: "deepseek-chat" })).provider);
});

test("validateForm: the limits come from the option set, output must fit the window", () => {
  // The dropdowns cannot produce a bad value, so the only ways in are a
  // stale form or a caller bypassing the UI — both get refused rather than
  // silently defaulted. The zero window (R1) is structurally impossible now.
  assert.ok(validateForm(form({ contextWindow: 0 })).contextWindow);
  assert.ok(validateForm(form({ contextWindow: 12345 })).contextWindow);
  assert.ok(validateForm(form({ maxTokens: -1 })).maxTokens);
  assert.ok(validateForm(form({ maxTokens: 8192 })).maxTokens); // not an option any more
  // Output above the window is the pair every provider rejects (D7).
  assert.equal(
    validateForm(form({ contextWindow: 32768, maxTokens: 1048576 })).maxTokens,
    "最大输出不能大于上下文窗口",
  );
  // Equal is legal; so is a window above the cap.
  assert.deepEqual(validateForm(form({ contextWindow: 32768, maxTokens: 32768 })), {});
  assert.deepEqual(validateForm(form({ contextWindow: 1048576, maxTokens: 65536 })), {});
  // Every option is accepted in both fields — the set and the validator
  // cannot drift apart.
  for (const option of WINDOW_OPTIONS) {
    const errors = validateForm(form({ contextWindow: option.value, maxTokens: option.value }));
    assert.deepEqual(errors, {}, `${option.label} must be a legal pair`);
  }
});

test("validateForm: thinking levels are the three the kernel accepts", () => {
  // No ticked box is legal — it means "not a reasoning model" (D2).
  assert.deepEqual(validateForm(form({ thinkingLevels: [] })), {});
  assert.deepEqual(validateForm(form({ thinkingLevels: ["low", "high"] })), {});
  // The kernel refuses everything outside {low, medium, high}, so the form
  // does too rather than spending a round trip on it.
  assert.ok(validateForm(form({ thinkingLevels: ["xhigh"] })).thinkingLevels);
  assert.ok(validateForm(form({ thinkingLevels: ["max"] })).thinkingLevels);
  assert.ok(validateForm(form({ thinkingLevels: [""] })).thinkingLevels);
  assert.ok(validateForm(form({ thinkingLevels: ["HIGH"] })).thinkingLevels);
});

test("toggleThinkingLevel: ticked levels are independent", () => {
  // The core of D1: ticking 高 must not drag 中 along, and un-ticking one
  // level leaves the others alone.
  assert.deepEqual(toggleThinkingLevel([], "high"), ["high"]);
  assert.deepEqual(toggleThinkingLevel(["low"], "high"), ["low", "high"]);
  assert.deepEqual(toggleThinkingLevel(["low", "high"], "high"), ["low"]);
  assert.deepEqual(toggleThinkingLevel(["low", "high"], "medium"), ["low", "high", "medium"]);
});

// -- the spec the command carries ------------------------------------------------

test("buildSpec: derivations applied, defaults filled, nothing left to guess", () => {
  const spec = buildSpec(form());
  assert.equal(spec.provider, "api-deepseek-com");
  assert.equal(spec.api, API_OPENAI);
  assert.equal(spec.modelName, "deepseek-chat"); // empty display name falls back to the id
  assert.equal(spec.contextWindow, DEFAULT_CONTEXT_WINDOW);
  assert.equal(spec.maxTokens, DEFAULT_MAX_TOKENS);
  assert.deepEqual(spec.thinkingLevels, []); // non-reasoning unless ticked
});

test("buildSpec: thinking levels ride along as a set, in canonical order", () => {
  // "Only high" stays only high — no implicit medium (D1).
  assert.deepEqual(buildSpec(form({ thinkingLevels: ["high"] })).thinkingLevels, ["high"]);
  // Click order must not reach the command (D4).
  assert.deepEqual(buildSpec(form({ thinkingLevels: ["high", "low"] })).thinkingLevels, ["low", "high"]);
  assert.deepEqual(
    buildSpec(form({ thinkingLevels: ["high", "medium", "low"] })).thinkingLevels,
    THINKING_ORDER,
  );
});

test("buildSpec: explicit values ride along verbatim", () => {
  const spec = buildSpec(
    form({
      provider: "my-proxy",
      api: API_ANTHROPIC,
      modelName: "DeepSeek Chat",
      thinkingLevels: ["low", "medium"],
      contextWindow: 262144,
      maxTokens: 65536,
    }),
  );
  assert.equal(spec.provider, "my-proxy");
  assert.equal(spec.api, API_ANTHROPIC);
  assert.equal(spec.modelName, "DeepSeek Chat");
  assert.equal(spec.contextWindow, 262144);
  assert.equal(spec.maxTokens, 65536);
});

test("buildSpec: the endpoint is trimmed, the key is not guessed at", () => {
  const spec = buildSpec(form({ baseUrl: "  https://api.deepseek.com/v1  ", apiKey: " sk-test " }));
  assert.equal(spec.baseUrl, "https://api.deepseek.com/v1");
  assert.equal(spec.apiKey, "sk-test");
});

test("buildSpec: the anthropic path is inferred, not assumed", () => {
  const spec = buildSpec(form({ baseUrl: "https://api.deepseek.com/anthropic" }));
  assert.equal(spec.api, API_ANTHROPIC);
  assert.equal(spec.provider, "api-deepseek-com");
});
