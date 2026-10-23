// ForkPicker.tsx — the fork selector (plan P-C4's third shell piece).
// get_fork_messages lists the session's user messages; picking one sends
// fork(entryId), which moves the kernel's leaf to that message's parent
// and answers with the message's text for the composer (plan D7).
//
// The list is the kernel's, in file order — oldest first, which is the
// order a "go back to where we branched off" picker wants. A session
// with no user messages says so instead of showing an empty box.

import type { ForkMessage } from "../state/wire.ts";

interface Props {
  messages: ForkMessage[];
  loading: boolean;
  onPick: (entryId: string) => void;
  onCancel: () => void;
}

/** One line per candidate: the prompt, shortened for the list. */
function preview(text: string): string {
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length > 48 ? `${flat.slice(0, 48)}…` : flat;
}

export function ForkPicker({ messages, loading, onPick, onCancel }: Props) {
  return (
    <div className="fork-picker" role="dialog" aria-label="选择分叉点">
      <div className="fork-head">
        <span>从一条旧消息分叉——选中的文本会放回输入框，改不改随你</span>
        <button type="button" className="fork-cancel" onClick={onCancel}>
          取消
        </button>
      </div>
      {loading ? (
        <p className="fork-empty">读取中…</p>
      ) : messages.length === 0 ? (
        <p className="fork-empty">这个会话还没有可以分叉的消息。</p>
      ) : (
        <ul className="fork-list">
          {messages.map((message) => (
            <li key={message.entryId}>
              <button type="button" title={message.text} onClick={() => onPick(message.entryId)}>
                {preview(message.text)}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
