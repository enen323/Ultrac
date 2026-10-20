// Composer.tsx — the input box (plan §4.2, row three). One textarea with
// three modes the kernel's prompt command already understands:
//
//   idle             prompt starts a run
//   running + steer  the message joins the current run's input (steering)
//   running + follow the message waits for the run to end (follow-up)
//
// The steer/follow-up choice is per message — a segmented control that only
// exists while a run is in flight, because that is the only time the kernel
// requires streamingBehavior (server.go cmdPrompt: a prompt without one
// while running is an error). The queue pill shows pendingMessageCount from
// get_state (no event carries it, so the store polls while streaming) and
// clear_queue empties both queues.
//
// Enter sends, Shift+Enter makes a newline, and a composition in flight —
// the Chinese IME case — owns Enter until it commits (isComposing is the
// only honest signal; keydown alone cannot see it).

import { type KeyboardEvent, useEffect, useRef, useState } from "react";
import type { SendBehavior } from "../state/session.ts";

interface Props {
  streaming: boolean;
  pendingCount: number;
  disabled: boolean;
  onSend: (text: string, behavior?: SendBehavior) => void;
  onAbort: () => void;
  onClearQueue: () => void;
}

const MAX_HEIGHT = 180;

export function Composer({ streaming, pendingCount, disabled, onSend, onAbort, onClearQueue }: Props) {
  const [text, setText] = useState("");
  const [behavior, setBehavior] = useState<SendBehavior>("steer");
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  // Grow with the text, up to a cap; past it the textarea scrolls.
  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, MAX_HEIGHT)}px`;
  }, [text]);

  const submit = (): void => {
    const value = text.trim();
    if (!value || disabled) return;
    onSend(value, streaming ? behavior : undefined);
    setText("");
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>): void => {
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      submit();
    }
  };

  return (
    <div className="composer">
      <div className="composer-inner">
        <div className="composer-box">
          <textarea
            ref={textareaRef}
            rows={1}
            disabled={disabled}
            placeholder={streaming ? "运行中——消息进入所选队列（Enter 发送）" : "说点什么…（Enter 发送，Shift+Enter 换行）"}
            value={text}
            onChange={(event) => setText(event.target.value)}
            onKeyDown={onKeyDown}
          />
          {streaming ? (
            <button type="button" className="composer-stop" onClick={onAbort}>
              停止
            </button>
          ) : (
            <button type="button" className="composer-send" onClick={submit} disabled={disabled || !text.trim()}>
              发送
            </button>
          )}
        </div>
        <div className="composer-foot">
          {streaming && (
            <>
              <div className="segmented" role="group" aria-label="排队方式">
                <button
                  type="button"
                  aria-pressed={behavior === "steer"}
                  onClick={() => setBehavior("steer")}
                  title="插入当前运行的对话"
                >
                  插入
                </button>
                <button
                  type="button"
                  aria-pressed={behavior === "followUp"}
                  onClick={() => setBehavior("followUp")}
                  title="等当前运行结束后再发送"
                >
                  排后
                </button>
              </div>
              {pendingCount > 0 && (
                <span className="queue-pill">
                  {pendingCount} 条排队
                  <button type="button" onClick={onClearQueue}>
                    清空
                  </button>
                </span>
              )}
            </>
          )}
          <span className="spacer" />
        </div>
      </div>
    </div>
  );
}
