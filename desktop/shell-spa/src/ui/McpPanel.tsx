// McpPanel.tsx — the MCP inventory
// (docs/开发计划/新功能计划/mcp-plan.md P5).
//
// # What it shows, and what it deliberately does not
//
// One row per configured server: its name, its transport, where it is, and
// how many tools it brought. The row expands to show the server's own
// description of what it is for and, when it is not usable, the reason.
//
// It has no toggle, no add button and no remove button. A server's
// membership is decided at boot — the kernel's tool set is fixed for the
// process (§3.1) — so a switch in this panel could only lie: it would report
// a state no restart has agreed to. Editing mcpServers is `ultrac mcp
// import` and a text editor's job, and the panel says so.
//
// # Why the rows are this quiet
//
// The three states a user will actually see are `cached`, `ready` and
// `failed`, and the difference matters: `cached` is not a promise that the
// server works, only that it worked at some point and nothing has contacted
// it since. The label says so instead of showing a green dot that means
// nothing.
//
// # Why it is a dialog rather than a dock
//
// The list is a read of state that changes at most once per server per
// process, and it is worth the window while it is open — the same treatment
// the custom-model form gets, for the same reason (a small, rare surface does
// not deserve a permanent strip). Escape, the close button and a backdrop
// click all dismiss it; the store keeps pulling while it is open, so a tool
// call that moves a server from `cached` to `ready` shows up here without
// closing and reopening.

import { useEffect, useState } from "react";
import type { McpServerStatus, McpState } from "../state/wire.ts";

/** The state vocabulary, in the plan's order. Unlisted states render raw. */
const STATE_LABEL: Record<McpState, string> = {
  cached: "未连接",
  spawning: "连接中…",
  ready: "已连接",
  failed: "不可用",
};

/** A short note on what a state does and does not promise. */
const STATE_HINT: Record<McpState, string> = {
  cached: "工具清单读自缓存，本进程还没联系过它",
  spawning: "有工具调用了它，握手正在跑",
  ready: "握手已完成，工具可用",
  failed: "连接失败，本次运行用不了它",
};

export function McpPanel({
  servers,
  onClose,
}: {
  servers: McpServerStatus[];
  onClose: () => void;
}) {
  const [open, setOpen] = useState<string | null>(null);

  // Escape closes — the same key the rename field and the custom-model
  // dialog honour. A dismissable surface with no keyboard way out is a bug.
  useEffect(() => {
    const onKey = (event: KeyboardEvent): void => {
      if (event.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className="mcp-dialog"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <div className="mcp-panel" role="dialog" aria-label="MCP 服务器">
        <div className="mcp-head">
          <span>MCP 服务器</span>
          <button type="button" className="mcp-close" onClick={onClose} title="关闭">
            关闭
          </button>
        </div>

        <p className="mcp-note">
          工具集在启动时定下来，这份清单是那一刻的；改动要重启内核才生效。
        </p>

        {servers.length === 0 ? (
          <p className="mcp-empty">
            没有配置 MCP 服务器。在 <code>settings.json</code> 的 <code>mcpServers</code>{" "}
            段里声明，或者用 <code>ultrac mcp import &lt;path&gt;</code> 导入一个{" "}
            <code>.mcp.json</code>。
          </p>
        ) : (
          <ul className="mcp-list">
            {servers.map((server) => {
              const expanded = open === server.name;
              return (
                <li key={server.name} className={`mcp-row ${stateTone(server.state)}`}>
                  <button
                    type="button"
                    className="mcp-summary"
                    aria-expanded={expanded}
                    onClick={() => setOpen(expanded ? null : server.name)}
                  >
                    <span className="mcp-name">{server.name}</span>
                    <span className="mcp-transport">{server.transport}</span>
                    <span className="mcp-state">{STATE_LABEL[server.state] ?? server.state}</span>
                    <span className="mcp-tools">{server.tools} 个工具</span>
                    <span className="mcp-chevron" aria-hidden="true">
                      {expanded ? "▾" : "▸"}
                    </span>
                  </button>
                  {expanded && (
                    <div className="mcp-detail">
                      <p className="mcp-hint">{STATE_HINT[server.state] ?? ""}</p>
                      {server.instructions ? <p className="mcp-instructions">{server.instructions}</p> : null}
                      {server.error ? <p className="mcp-error">{server.error}</p> : null}
                      {server.malformedLines ? (
                        <p className="mcp-error">
                          这个服务器往 stdout 打了 {server.malformedLines} 行非 JSON
                          内容，通常是日志——被跳过了，但它可能在吞掉真正的响应。
                        </p>
                      ) : null}
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </div>
  );
}

/** The row's tone: only two states are worth colouring. */
function stateTone(state: McpState): string {
  if (state === "failed") return "is-failed";
  if (state === "ready") return "is-ready";
  return "";
}
