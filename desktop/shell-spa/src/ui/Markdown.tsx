// Markdown.tsx — the one renderer for model prose. react-markdown v10 with
// remark-gfm (tables, strikethrough, task lists) and rehype-highlight
// (fenced code), the same trio Reasonix's Markdown.tsx uses.
//
// Safety is structural, not configured: v10 renders no raw HTML unless
// rehype-raw is added, and it is not — model output can never inject markup.
// Links are the one deliberate override: a plain <a> inside a file:// page
// would navigate the window away from the app (and the shell's will-navigate
// guard would then block it, leaving a dead link). Every http(s) link goes
// through the bridge instead, where links.ts decides — anything else is left
// to the default behaviour, which for a non-http href is nothing.

import type { UltracBridge } from "../port/bridge.ts";
import Markdown from "react-markdown";
import rehypeHighlight from "rehype-highlight";
import remarkGfm from "remark-gfm";

export function MarkdownView({ text, bridge }: { text: string; bridge?: UltracBridge }) {
  return (
    <div className="md">
      <Markdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeHighlight]}
        components={{
          a: ({ href, children }) => (
            <a
              href={href}
              onClick={(event) => {
                if (!href || !/^https?:/i.test(href)) return;
                event.preventDefault();
                void bridge?.openExternal(href);
              }}
            >
              {children}
            </a>
          ),
        }}
      >
        {text}
      </Markdown>
    </div>
  );
}
