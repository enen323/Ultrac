// App.tsx — the page: sidebar + one conversation column + the composer dock.
// The component tree is the plan §4.2 face and nothing else — no approvals,
// no plan/goal panes, no market, no file tree (plan §4.3 keeps them out).
//
// Until session_loaded arrives the component renders nothing: index.html's
// boot screen owns the window (ui/boot.ts takes it down once the store is
// ready). That is the same handover Reasonix's main.tsx performs with its
// wordmark intro — the page that decides what to show is not the markup
// that held the window open.

import { useEffect, useState, useSyncExternalStore } from "react";
import type { UltracBridge } from "../port/bridge.ts";
import type { SessionStore } from "../state/session.ts";
import { contextTokens } from "../state/transcript.ts";
import { dismissBoot } from "./boot.ts";
import { Composer } from "./Composer.tsx";
import { ModelPicker } from "./ModelPicker.tsx";
import { Sidebar } from "./Sidebar.tsx";
import { Transcript } from "./Transcript.tsx";
import { useTheme } from "./theme.ts";

export function App({ store, bridge }: { store: SessionStore; bridge: UltracBridge }) {
  const snap = useSyncExternalStore(store.subscribe, store.getSnapshot);
  const [theme, cycleTheme] = useTheme();
  const [renaming, setRenaming] = useState(false);
  const [draftName, setDraftName] = useState("");

  useEffect(() => {
    if (snap.ready) dismissBoot();
  }, [snap.ready]);

  if (!snap.ready) return null;

  const state = snap.state;
  // The event-derived flag: message_start opens a run, agent_end closes it.
  // get_state's isStreaming lags by up to one poll and is only the backstop.
  const streaming = snap.transcript.streamingKey !== null;
  const error = snap.bootError ?? snap.actionError;

  const commitRename = (): void => {
    const name = draftName.trim();
    setRenaming(false);
    if (name && name !== state?.sessionName) void store.renameSession(name);
  };

  return (
    <div className="app">
      <Sidebar
        sessions={snap.sessions}
        currentPath={state?.sessionFile}
        onResume={(path) => void store.resumeSession(path)}
        onNew={() => void store.newSession()}
        theme={theme}
        onCycleTheme={cycleTheme}
      />
      <div className="main">
        <header className="main-head">
          {renaming ? (
            <input
              className="session-title"
              autoFocus
              value={draftName}
              placeholder="会话名字"
              onChange={(event) => setDraftName(event.target.value)}
              onBlur={commitRename}
              onKeyDown={(event) => {
                if (event.key === "Enter") commitRename();
                if (event.key === "Escape") setRenaming(false);
              }}
            />
          ) : (
            <button
              type="button"
              className="session-title"
              title="点击重命名会话"
              onClick={() => {
                setDraftName(state?.sessionName ?? "");
                setRenaming(true);
              }}
            >
              {state?.sessionName?.trim() || "未命名会话"}
            </button>
          )}
          <ModelPicker
            models={snap.models}
            thinkingLevels={snap.thinkingLevels}
            model={state?.model}
            thinkingLevel={state?.thinkingLevel ?? ""}
            onSetModel={(provider, modelId) => void store.setModel(provider, modelId)}
            onSetThinkingLevel={(level) => void store.setThinkingLevel(level)}
          />
        </header>

        <Transcript transcript={snap.transcript} bridge={bridge} />

        <div className="under-transcript">
          {error && (
            <div className="banner error" role="alert">
              <span>{error}</span>
            </div>
          )}
          <Composer
            streaming={streaming}
            pendingCount={state?.pendingMessageCount ?? 0}
            disabled={!snap.ready}
            onSend={(text, behavior) => void store.send(text, behavior)}
            onAbort={() => void store.abort()}
            onClearQueue={() => void store.clearQueue()}
          />
          <StatusLine tokens={contextTokens(snap.transcript)} contextWindow={state?.model?.contextWindow} />
        </div>
      </div>
    </div>
  );
}

/**
 * The context water mark. There is no compaction in this port (plan §4.2),
 * so the page counts what the kernel reports: the last assistant message's
 * input plus cache reads and writes, against the model's context window. An
 * estimate — the label says so — and the only context signal there is.
 */
function StatusLine({ tokens, contextWindow }: { tokens?: number; contextWindow?: number }) {
  const percent = tokens !== undefined && contextWindow ? Math.min(100, Math.round((tokens / contextWindow) * 100)) : null;
  const tone = percent === null ? "" : percent >= 95 ? "over" : percent >= 75 ? "warn" : "";
  return (
    <div className="status">
      <span>上下文</span>
      {tokens !== undefined && contextWindow ? (
        <>
          <span>
            ≈ {(tokens / 1000).toFixed(1)}k / {(contextWindow / 1000).toFixed(0)}k
          </span>
          <span className={`meter ${tone}`}>
            <i style={{ width: `${percent}%` }} />
          </span>
        </>
      ) : (
        <span>—</span>
      )}
      <span className="spacer" />
    </div>
  );
}
