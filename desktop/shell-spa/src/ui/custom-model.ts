// custom-model.ts — the custom-model dialog's pure logic: what the user
// meant, decided without React and without the kernel. The dialog
// renders these; test/custom-model.test.ts pins them (node --test, no
// DOM — which is why this is a .ts and not part of the .tsx).
//
// Two derivations carry the dialog (plan D2, "协议显式必选 + 智能默认"):
// the provider id comes from the baseUrl's host, and the dialect is
// inferred from the same host. Both are shown in the form and both are
// overridable — the command that finally leaves always carries an
// explicit value, exactly like the kernel's protocol layer expects.

import type { CustomModelSpec } from "../state/wire.ts";

/** The two dialects the kernel registers (internal/ai/model.go, F2). */
export const API_OPENAI = "openai-completions";
export const API_ANTHROPIC = "anthropic-messages";

/** The dialect select's options, in the order the form shows them. */
export const API_OPTIONS: ReadonlyArray<{ value: string; label: string }> = [
  { value: API_OPENAI, label: "OpenAI 兼容（/chat/completions）" },
  { value: API_ANTHROPIC, label: "Anthropic 兼容（/messages）" },
];

/**
 * The thinking levels the dialog exposes, in the order the checkboxes are
 * drawn. These are exactly the values the kernel accepts in
 * add_custom_model's thinkingLevels (internal/rpc/server.go's
 * ValidateCustomModelSpec); "minimal"/"xhigh"/"max" exist in the kernel but
 * are provider opt-ins the form deliberately does not offer.
 */
export const THINKING_OPTIONS: ReadonlyArray<{ value: string; label: string }> = [
  { value: "low", label: "低" },
  { value: "medium", label: "中" },
  { value: "high", label: "高" },
];

/** The payload's canonical order (plan D4): a click order must not change
 * the command, so buildSpec normalises to this sequence. */
export const THINKING_ORDER: readonly string[] = THINKING_OPTIONS.map((option) => option.value);

/**
 * The window options, shared by 上下文窗口 and 最大输出: 32K to 1M. A closed
 * set is the point — a free number field let "0" through, and a zero window
 * silently disables auto-compaction (compact's `contextWindow <= 0` gate).
 *
 * The values are powers of two because the labels are; 128K means 131072,
 * not 128000, so the option a user picks is the number the kernel stores.
 */
export const WINDOW_OPTIONS: ReadonlyArray<{ value: number; label: string }> = [
  { value: 32768, label: "32K" },
  { value: 65536, label: "64K" },
  { value: 131072, label: "128K" },
  { value: 262144, label: "256K" },
  { value: 524288, label: "512K" },
  { value: 1048576, label: "1M" },
];

const WINDOW_VALUES: ReadonlySet<number> = new Set(WINDOW_OPTIONS.map((option) => option.value));

/**
 * The form's defaults (plan R1/D6): the context window is the largest value
 * that matches what endpoints call "128K", and the answer cap is 32K — an
 * upper bound, not a per-turn target. Both are prefilled and validated rather
 * than left to a zero that means nothing.
 */
export const DEFAULT_CONTEXT_WINDOW = 131072;
export const DEFAULT_MAX_TOKENS = 32768;

/** The form's shape: every text field a string, the choices typed. */
export interface CustomModelForm {
  baseUrl: string;
  apiKey: string;
  /** Empty means "derive from the baseUrl" — the smart default. */
  provider: string;
  modelId: string;
  /** Empty means "same as the model id". */
  modelName: string;
  /** Empty means "infer from the baseUrl". */
  api: string;
  /** The ticked levels; empty means "not a reasoning model" (plan D1/D2). */
  thinkingLevels: string[];
  contextWindow: number;
  maxTokens: number;
}

export function emptyForm(): CustomModelForm {
  return {
    baseUrl: "",
    apiKey: "",
    provider: "",
    modelId: "",
    modelName: "",
    api: "",
    thinkingLevels: [],
    contextWindow: DEFAULT_CONTEXT_WINDOW,
    maxTokens: DEFAULT_MAX_TOKENS,
  };
}

/**
 * Toggle one checkbox: ticked levels are independent, so adding "high" to
 * ["low"] yields ["low","high"] — never ["low","medium","high"]. Kept here
 * (not in the dialog) so the rule is testable without a DOM.
 */
export function toggleThinkingLevel(levels: readonly string[], level: string): string[] {
  return levels.includes(level) ? levels.filter((value) => value !== level) : [...levels, level];
}

/**
 * The host of an http(s) baseUrl — port included when it is not the
 * default, so two endpoints on one host (a plain and an Ollama on
 * :11434) do not collapse into one id — or "" when it is not an http(s)
 * URL with a host. The kernel's protocol layer accepts only these two
 * schemes (server.go's ValidateCustomModelSpec), and the derivation
 * must not be more permissive than the thing it feeds.
 */
function urlHost(baseUrl: string): string {
  const trimmed = baseUrl.trim();
  if (!trimmed) return "";
  // A scheme the kernel does not speak is refused here, not parsed
  // around: prepending https:// to "ftp://host" would parse it as the
  // host "ftp" and accept the endpoint.
  const scheme = /^([a-z][a-z0-9+.-]*):\/\//i.exec(trimmed);
  if (scheme && !/^https?$/i.test(scheme[1])) return "";
  // A bare host ("api.deepseek.com") parses as a path, so the parser
  // gets a scheme to chew on when the user typed none — the paste of
  // "api.deepseek.com/v1" is common and must not fail as "not a URL".
  const withScheme = scheme ? trimmed : `https://${trimmed}`;
  try {
    const url = new URL(withScheme);
    if (url.protocol !== "http:" && url.protocol !== "https:") return "";
    return url.host;
  } catch {
    return "";
  }
}

/**
 * Derive the provider id from a baseUrl: the host with dots (and a
 * non-default port's colon) as dashes ("api.deepseek.com" →
 * "api-deepseek-com", "localhost:11434" → "localhost-11434"). It is the
 * providers key and set_model's "provider/" part, so it must be a
 * stable, legal id — a host contains neither "/" nor whitespace, which
 * is exactly what the kernel's validation demands of the field.
 */
export function deriveProviderId(baseUrl: string): string {
  const host = urlHost(baseUrl);
  if (!host) return "";
  return host.replaceAll(/[.:]/g, "-").toLowerCase();
}

/**
 * Infer the dialect from the baseUrl: an Anthropic-flavoured endpoint
 * speaks anthropic-messages, everything else openai-completions. A
 * one-click default, never a hidden one — the select shows it and the
 * user can pick the other.
 */
export function inferAPI(baseUrl: string): string {
  if (baseUrl.trim().toLowerCase().includes("anthropic")) return API_ANTHROPIC;
  return API_OPENAI;
}

/**
 * The provider id the command will carry: the explicit one when the user
 * typed it, else the derived one. "" means "not derivable yet" — the
 * baseUrl is unparseable, and validation says so.
 */
export function effectiveProvider(form: CustomModelForm): string {
  const explicit = form.provider.trim();
  return explicit || deriveProviderId(form.baseUrl);
}

/**
 * Validate the form the way the kernel's protocol layer will
 * (server.go's ValidateCustomModelSpec), so a refusal is caught before
 * the round trip. Returns one message per bad field, keyed by field
 * name; an empty object is a form the kernel accepts.
 *
 * The limits are taken from the option set, and the answer cap must not
 * exceed the window: a provider rejects that pair outright, so it is caught
 * here rather than as a 400 on the first prompt (plan D7). Every rule the
 * kernel's protocol layer applies is mirrored, so a refusal costs no round
 * trip.
 */
export function validateForm(form: CustomModelForm): Record<string, string> {
  const errors: Record<string, string> = {};
  if (!form.baseUrl.trim()) {
    errors.baseUrl = "接口地址必填";
  } else if (!urlHost(form.baseUrl)) {
    errors.baseUrl = "接口地址要是一个 http(s) 地址，且带主机名";
  }
  if (!form.apiKey.trim()) {
    errors.apiKey = "密钥必填（本地无鉴权端点可填任意占位符）";
  }
  if (!form.modelId.trim()) {
    errors.modelId = "模型 id 必填";
  }
  // The explicit id must be a legal provider id: no "/" (it is
  // set_model's separator) and no whitespace. The derived one is legal
  // by construction — a host contains neither.
  const provider = form.provider.trim();
  if (provider && /[/\s]/.test(provider)) {
    errors.provider = "provider id 不能含 / 或空白";
  }
  const api = form.api || inferAPI(form.baseUrl);
  if (api !== API_OPENAI && api !== API_ANTHROPIC) {
    errors.api = "协议只能是这两种已注册的 dialect";
  }
  // The levels come from checkboxes, so a bad value means a stale form or a
  // caller bypassing the UI — the kernel refuses it the same way (only low,
  // medium and high are accepted).
  if (form.thinkingLevels.some((level) => !THINKING_ORDER.includes(level))) {
    errors.thinkingLevels = "思考强度只能是低、中、高";
  }
  if (!WINDOW_VALUES.has(form.contextWindow)) {
    errors.contextWindow = "要从列表里选一个上下文窗口档位";
  }
  if (!WINDOW_VALUES.has(form.maxTokens)) {
    errors.maxTokens = "要从列表里选一个输出档位";
  } else if (form.maxTokens > form.contextWindow) {
    errors.maxTokens = "最大输出不能大于上下文窗口";
  }
  return errors;
}

/**
 * Build the command's payload: every derivation applied, every default
 * filled, nothing left for the kernel to guess. Call it only when
 * validateForm came back empty — it does not re-check.
 *
 * The levels are normalised to THINKING_ORDER, so the checkbox click order
 * never leaks into the command (plan D4).
 */
export function buildSpec(form: CustomModelForm): CustomModelSpec {
  const modelId = form.modelId.trim();
  return {
    provider: effectiveProvider(form),
    baseUrl: form.baseUrl.trim(),
    apiKey: form.apiKey.trim(),
    api: form.api || inferAPI(form.baseUrl),
    modelId,
    modelName: form.modelName.trim() || modelId,
    thinkingLevels: THINKING_ORDER.filter((level) => form.thinkingLevels.includes(level)),
    contextWindow: form.contextWindow,
    maxTokens: form.maxTokens,
  };
}
