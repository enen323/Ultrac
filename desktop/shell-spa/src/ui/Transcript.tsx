// Transcript.tsx — the message flow (plan §4.2, row one). User messages are
// bubbles; assistant messages render their content blocks in order — prose
// through MarkdownView, thinking in a collapsed block, tool calls as cards
// wearing the live tool_execution_* status. The typewriter is not an
// animation here: the kernel streams real deltas (message_update), and the
// row simply re-renders as they arrive; message_end swaps in the
// authoritative content.
//
// Scrolling sticks to the bottom while a run streams, but a person who
// scrolled up to read is never yanked back down — the check runs after the
// render, when the content has already grown.

import { useEffect, useRef } from "react";
import type { UltracBridge } from "../port/bridge.ts";
import type { BlockView, TranscriptMessage, TranscriptState } from "../state/transcript.ts";
import { MarkdownView } from "./Markdown.tsx";
import { ToolCard } from "./ToolCard.tsx";

export function Transcript({ transcript, bridge }: { transcript: TranscriptState; bridge: UltracBridge }) {
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
    if (distance < 90) el.scrollTop = el.scrollHeight;
  });

  if (transcript.messages.length === 0) {
    return (
      <div className="transcript" ref={scrollRef}>
        <div className="transcript-empty">
          <div className="big">新会话</div>
          <div>输入第一条消息开始。思考过程、工具执行与错误都会呈现在这里。</div>
        </div>
      </div>
    );
  }

  return (
    <div className="transcript" ref={scrollRef}>
      <div className="column">
        {transcript.messages.map((row) => (
          <Row key={row.key} row={row} transcript={transcript} bridge={bridge} />
        ))}
      </div>
    </div>
  );
}

function Row({
  row,
  transcript,
  bridge,
}: {
  row: TranscriptMessage;
  transcript: TranscriptState;
  bridge: UltracBridge;
}) {
  // A boundary the session file records as an entry rather than a turn: a
  // compaction cut or the summary of a branch this conversation came back
  // from. It is a divider, never a bubble — the summary is the model's
  // words, and the turns it replaced are still above it.
  if (row.role === "notice") {
    return (
      <div className="row notice">
        <details className="notice-card">
          <summary>
            <span className="notice-title">{row.notice === "branch" ? "分支摘要" : "上下文已压缩"}</span>
            {row.tokensBefore !== undefined ? (
              <span className="notice-meta">压缩前约 {row.tokensBefore.toLocaleString()} tokens</span>
            ) : null}
          </summary>
          <div className="notice-body">
            {row.blocks.map((block, index) => (
              <BlockFragment key={index} block={block} bridge={bridge} />
            ))}
          </div>
        </details>
      </div>
    );
  }

  // A run that failed lands as an assistant message carrying errorMessage
  // (internal/agent handleRunFailure). It is the run态错误呈现 the plan
  // asks for: the row itself says what went wrong.
  if (row.role === "assistant" && row.errorMessage) {
    return (
      <div className="row error">
        <span className="who">出错了</span>
        <span>{row.errorMessage}</span>
      </div>
    );
  }

  if (row.role === "user") {
    return (
      <div className="row user">
        <div className="bubble">
          {row.blocks.map((block, index) => (
            <BlockFragment key={index} block={block} />
          ))}
        </div>
      </div>
    );
  }

  return (
    <div className={`row assistant${row.streaming ? " streaming" : ""}`}>
      <div className="body">
        {row.blocks.map((block, index) => (
          <BlockFragment key={index} block={block} transcript={transcript} bridge={bridge} />
        ))}
      </div>
    </div>
  );
}

function BlockFragment({
  block,
  transcript,
  bridge,
}: {
  block: BlockView;
  transcript?: TranscriptState;
  bridge?: UltracBridge;
}) {
  switch (block.kind) {
    case "text":
      return <MarkdownView text={block.text} bridge={bridge} />;
    case "thinking":
      return (
        <details className="think">
          <summary>思考过程</summary>
          <div className="think-body">{block.text || "…"}</div>
        </details>
      );
    case "image":
      return <img src={`data:${block.mimeType};base64,${block.data}`} alt="" />;
    case "toolCall":
      return <ToolCard block={block} run={transcript?.toolRuns[block.id]} />;
  }
}
