// ToolCard.tsx — one tool call as a折叠卡片: the assistant's toolCall block
// (plan §4.2) wearing the live status the tool_execution_* events carry.
// Collapsed by default — the header alone says what ran and how it ended;
// the arguments and the output are one click away.
//
// The block and the run are two views of one call: the block (from the
// assistant message) holds id/name/arguments, the run (keyed by the same
// toolCallId) holds status/partial output/duration. A call still streaming
// its arguments shows the raw JSON fragments; once toolcall_end lands, the
// parsed object is shown instead.

import { useState } from "react";
import type { BlockView, ToolRun } from "../state/transcript.ts";

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function statusMeta(run: ToolRun | undefined): string {
  if (!run) return "";
  if (run.status === "running") return "运行中…";
  const parts: string[] = [];
  if (run.durationMs !== undefined) parts.push(formatDuration(run.durationMs));
  if (run.isError) parts.push("失败");
  return parts.join(" · ");
}

export function ToolCard({ block, run }: { block: Extract<BlockView, { kind: "toolCall" }>; run?: ToolRun }) {
  const [open, setOpen] = useState(false);
  const running = run?.status === "running";
  const dotClass = run ? (running ? "running" : run.isError ? "done-err" : "done-ok") : "done-ok";

  // While the call streams its arguments the parsed object is still empty;
  // the raw fragments are the honest preview.
  const argsText = block.id || Object.keys(block.args).length > 0 ? JSON.stringify(block.args, null, 2) : block.rawArgs;
  const output = run?.text ?? "";
  const meta = statusMeta(run);

  return (
    <div className="tool-card">
      <button type="button" className="tool-head" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        <span className={`dot ${dotClass}`} />
        <span className="tool-name">{block.name || "tool"}</span>
        {meta && <span className="tool-meta">{meta}</span>}
        <span className="tool-meta">{open ? "收起" : "展开"}</span>
      </button>
      {open && (
        <>
          {argsText && (
            <div className="tool-args">
              <div className="label">参数</div>
              <pre>{argsText}</pre>
            </div>
          )}
          <div className={`tool-out${run?.isError ? " is-error" : ""}`}>
            <div className="label">输出</div>
            <pre>{output || (running ? "…" : "（无输出）")}</pre>
          </div>
        </>
      )}
    </div>
  );
}
