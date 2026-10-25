// Sidebar.tsx — the session rail (plan §4.2, row six): new / rename / list /
// resume over new_session, set_session_name, list_sessions and
// resume_session. Rename lives on the main header's title (it renames the
// session that is open, which is the only one set_session_name can address);
// this rail lists the rest and reopens them.
//
// A row's title is the display name when the session has one, else the
// derived first-user-message title (internal/session/list.go keeps them
// separate so the client can choose). aria-current marks the open session —
// it is not a style hook, it is the honest state for anything that reads it.

import type { SessionSummary } from "../state/wire.ts";
import { relativeTime } from "./time.ts";
import type { ThemeChoice } from "./theme.ts";

interface Props {
  sessions: SessionSummary[];
  /** sessionFile of the open session, from get_state. */
  currentPath?: string;
  onResume: (sessionPath: string) => void;
  onNew: () => void;
  theme: ThemeChoice;
  onCycleTheme: () => void;
}

const THEME_LABELS: Record<ThemeChoice, string> = {
  system: "主题：跟随系统",
  light: "主题：浅色",
  dark: "主题：深色",
};

export function Sidebar({ sessions, currentPath, onResume, onNew, theme, onCycleTheme }: Props) {
  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <span className="wordmark">Ultrac</span>
        <button type="button" className="sidebar-new" onClick={onNew}>
          ＋ 新会话
        </button>
      </div>
      <div className="session-list">
        {sessions.length === 0 ? (
          <div className="sidebar-empty">还没有历史会话。开始的第一句会出现在这里。</div>
        ) : (
          sessions.map((session) => (
            <button
              key={session.path}
              type="button"
              className="session-row"
              aria-current={session.path === currentPath ? "true" : "false"}
              onClick={() => onResume(session.path)}
            >
              <span className="title">{session.name?.trim() || session.title}</span>
              <span className="meta">{relativeTime(session.timestamp)}</span>
            </button>
          ))
        )}
      </div>
      <div className="sidebar-foot">
        <button type="button" className="theme-toggle" onClick={onCycleTheme}>
          {THEME_LABELS[theme]}
        </button>
      </div>
    </aside>
  );
}
