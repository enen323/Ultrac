// ModelPicker.tsx — the model and thinking-level selectors (plan §4.2, row
// five). Two <select>s over get_available_models / get_available_thinking_levels;
// picking one is a set_model / set_thinking_level command. The level list is
// model-dependent (ai.ClampThinkingLevel clamps a level the model does not
// have), which is why the store refetches it after a model switch.
//
// The thinking labels map the kernel's own level names (internal/ai
// model.go: off/minimal/low/medium/high/xhigh/max) to words; an unknown
// level — a newer kernel than this page — shows its raw name rather than
// being dropped.

import type { RpcModel } from "../state/wire.ts";

const LEVEL_LABELS: Record<string, string> = {
  off: "关",
  minimal: "极简",
  low: "低",
  medium: "中",
  high: "高",
  xhigh: "极高",
  max: "最大",
};

function levelLabel(level: string): string {
  return LEVEL_LABELS[level] ?? level;
}

interface Props {
  models: RpcModel[];
  thinkingLevels: string[];
  model?: RpcModel;
  thinkingLevel: string;
  onSetModel: (provider: string, modelId: string) => void;
  onSetThinkingLevel: (level: string) => void;
}

export function ModelPicker({
  models,
  thinkingLevels,
  model,
  thinkingLevel,
  onSetModel,
  onSetThinkingLevel,
}: Props) {
  // Grouped by provider: the settings file can carry several, and a flat
  // list of ids is unreadable.
  const groups = new Map<string, RpcModel[]>();
  for (const entry of models) {
    const list = groups.get(entry.provider) ?? [];
    list.push(entry);
    groups.set(entry.provider, list);
  }
  // The current model is always selectable, even if the catalog fetch failed
  // and it is the only entry there is.
  const hasCurrent = model ? models.some((entry) => entry.id === model.id) : false;

  return (
    <>
      <label className="picker" htmlFor="model-select">
        模型
      </label>
      <select
        id="model-select"
        value={model?.id ?? ""}
        onChange={(event) => {
          const entry = models.find((candidate) => candidate.id === event.target.value);
          if (entry) onSetModel(entry.provider, entry.id);
        }}
      >
        {model && !hasCurrent && <option value={model.id}>{model.name}</option>}
        {[...groups].map(([provider, entries]) => (
          <optgroup key={provider} label={provider}>
            {entries.map((entry) => (
              <option key={entry.id} value={entry.id}>
                {entry.name}
                {entry.reasoning ? " · 推理" : ""}
              </option>
            ))}
          </optgroup>
        ))}
      </select>
      <label className="picker" htmlFor="thinking-select">
        思考
      </label>
      <select
        id="thinking-select"
        value={thinkingLevel}
        onChange={(event) => onSetThinkingLevel(event.target.value)}
        disabled={thinkingLevels.length === 0}
      >
        {thinkingLevels.length === 0 && <option value={thinkingLevel}>{levelLabel(thinkingLevel)}</option>}
        {thinkingLevels.map((level) => (
          <option key={level} value={level}>
            {levelLabel(level)}
          </option>
        ))}
      </select>
    </>
  );
}
