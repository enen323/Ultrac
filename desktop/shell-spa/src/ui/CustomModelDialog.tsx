// CustomModelDialog.tsx — the 自定义 model dialog (custom-model-plan.md
// P4). One form, one command: baseUrl / apiKey / 模型 id are what the
// user knows; the provider id and the dialect are derived from the
// baseUrl with the derivation shown (D2 — a smart default the user can
// see and override, never a hidden one).
//
// Submitting is add_custom_model: the kernel writes the credential to
// settings' providers section at the user layer,
// so the model is there whatever directory the next launch starts from
// (D8) — reloads, and answers with the fresh catalog, which the store
// patches into the picker (D4). No restart, no second round trip.
//
// The validation is local first (custom-model.ts mirrors the kernel's
// protocol rules, so a refusal costs no round trip); the kernel's own
// message — a write failure, an unregistered dialect — lands in the
// error bar. A provider id that already exists is warned about in red
// (R2): the merge rule would silently overwrite its declaration.

import { useEffect, useState } from "react";
import type { CustomModelResult, CustomModelSpec, RpcModel } from "../state/wire.ts";
import {
  API_OPTIONS,
  DEFAULT_CONTEXT_WINDOW,
  DEFAULT_MAX_TOKENS,
  buildSpec,
  deriveProviderId,
  effectiveProvider,
  emptyForm,
  inferAPI,
  validateForm,
  type CustomModelForm,
} from "./custom-model.ts";

interface Props {
  /** The current catalog — a provider id in it gets the overwrite warning (R2). */
  models: RpcModel[];
  /** The last action's error: the kernel's refusal, shown while the dialog is open. */
  error: string | null;
  /** Answers with the fresh catalog, or null when the kernel refused. */
  onAdd: (spec: CustomModelSpec) => Promise<CustomModelResult | null>;
  onClose: () => void;
}

export function CustomModelDialog({ models, error, onAdd, onClose }: Props) {
  const [form, setForm] = useState<CustomModelForm>(emptyForm);
  const [submitting, setSubmitting] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  // The error bar shows a refusal from THIS dialog's submit, not whatever
  // the page's last action left behind — actionError is page-global, and
  // a failed send from before the dialog opened is not this form's news.
  const [attempted, setAttempted] = useState(false);

  // Escape closes — the same key the rename field honours, and a dialog
  // that traps the user with no keyboard way out is a bug.
  useEffect(() => {
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === "Escape" && !submitting) onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose, submitting]);

  const set = (patch: Partial<CustomModelForm>): void =>
    setForm((prev) => ({ ...prev, ...patch }));

  const derived = deriveProviderId(form.baseUrl);
  const providerId = effectiveProvider(form);
  const known = new Set(models.map((entry) => entry.provider));
  const overwrites = providerId !== "" && known.has(providerId);

  const submit = async (): Promise<void> => {
    const errors = validateForm(form);
    setFieldErrors(errors);
    if (Object.keys(errors).length > 0) return;
    setSubmitting(true);
    setAttempted(true);
    // The store records the kernel's refusal in actionError, which the
    // page passes back as `error` — the bar below. A null answer is
    // that refusal; only a real result closes the dialog.
    const result = await onAdd(buildSpec(form));
    setSubmitting(false);
    if (result) onClose();
  };

  const field = (name: keyof CustomModelForm): string => fieldErrors[name] ?? "";
  const kernelError = attempted ? error : null;

  return (
    <div
      className="custom-dialog"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !submitting) onClose();
      }}
    >
      <div className="custom-panel" role="dialog" aria-label="自定义模型">
        <div className="custom-head">
          <span>自定义模型</span>
          <button type="button" className="custom-close" onClick={onClose} disabled={submitting}>
            关闭
          </button>
        </div>

        {kernelError && (
          <p className="custom-banner" role="alert">
            {kernelError}
          </p>
        )}

        <form
          className="custom-form"
          onSubmit={(event) => {
            event.preventDefault();
            void submit();
          }}
        >
          <div className="custom-row">
            <label htmlFor="custom-baseurl">接口地址</label>
            <input
              id="custom-baseurl"
              value={form.baseUrl}
              placeholder="https://api.deepseek.com/v1"
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => set({ baseUrl: event.target.value })}
            />
            <p className="custom-hint">端点的根地址；路径后缀（/v1）随服务商而定。</p>
            {field("baseUrl") && <p className="custom-err">{field("baseUrl")}</p>}
          </div>

          <div className="custom-row">
            <label htmlFor="custom-apikey">密钥</label>
            <input
              id="custom-apikey"
              type="password"
              value={form.apiKey}
              placeholder="sk-…"
              autoComplete="off"
              onChange={(event) => set({ apiKey: event.target.value })}
            />
            <p className="custom-hint">
              只写入本机 settings.json 的 providers 段（仅用户层）。本地无鉴权端点（如 Ollama）可填任意占位符。
            </p>
            {field("apiKey") && <p className="custom-err">{field("apiKey")}</p>}
          </div>

          <div className="custom-row">
            <label htmlFor="custom-provider">provider id（选填）</label>
            <input
              id="custom-provider"
              value={form.provider}
              placeholder={derived || "留空则从接口地址推导"}
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => set({ provider: event.target.value })}
            />
            <p className="custom-hint">
              {form.provider.trim()
                ? "将使用你填写的 id。"
                : derived
                  ? `将使用 ${derived}（由接口地址推导）。`
                  : "填了接口地址就能推导。"}
            </p>
            {overwrites && (
              <p className="custom-err">
                已存在同名 provider，保存将覆盖它的声明（密钥另行管理，不受影响）。
              </p>
            )}
            {field("provider") && <p className="custom-err">{field("provider")}</p>}
          </div>

          <div className="custom-row">
            <label htmlFor="custom-modelid">模型 id</label>
            <input
              id="custom-modelid"
              value={form.modelId}
              placeholder="deepseek-chat"
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => set({ modelId: event.target.value })}
            />
            {field("modelId") && <p className="custom-err">{field("modelId")}</p>}
          </div>

          <div className="custom-row">
            <label htmlFor="custom-modelname">显示名（选填）</label>
            <input
              id="custom-modelname"
              value={form.modelName}
              placeholder={form.modelId.trim() || "默认同模型 id"}
              onChange={(event) => set({ modelName: event.target.value })}
            />
          </div>

          <div className="custom-row">
            <label htmlFor="custom-api">协议</label>
            <select
              id="custom-api"
              value={form.api || inferAPI(form.baseUrl)}
              onChange={(event) => set({ api: event.target.value })}
            >
              {API_OPTIONS.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
            <p className="custom-hint">按接口地址猜了一个，不对就改。</p>
          </div>

          <div className="custom-pair">
            <div className="custom-row">
              <label htmlFor="custom-window">上下文窗口</label>
              <input
                id="custom-window"
                value={form.contextWindow}
                inputMode="numeric"
                onChange={(event) => set({ contextWindow: event.target.value })}
              />
              {field("contextWindow") && <p className="custom-err">{field("contextWindow")}</p>}
            </div>
            <div className="custom-row">
              <label htmlFor="custom-tokens">最大输出</label>
              <input
                id="custom-tokens"
                value={form.maxTokens}
                inputMode="numeric"
                onChange={(event) => set({ maxTokens: event.target.value })}
              />
              {field("maxTokens") && <p className="custom-err">{field("maxTokens")}</p>}
            </div>
          </div>
          <p className="custom-hint">
            默认 {DEFAULT_CONTEXT_WINDOW.toLocaleString()} / {DEFAULT_MAX_TOKENS.toLocaleString()}；
            留空用默认。窗口填 0 会让自动压缩永不触发，别这么做。
          </p>

          <div className="custom-foot">
            <button type="button" className="custom-cancel" onClick={onClose} disabled={submitting}>
              取消
            </button>
            <button type="submit" className="custom-save" disabled={submitting}>
              {submitting ? "保存中…" : "保存并添加"}
            </button>
          </div>
          <p className="custom-hint">
            保存写用户层配置，换目录启动也在；添加后不用重启，模型下拉里立刻能选。
          </p>
        </form>
      </div>
    </div>
  );
}
