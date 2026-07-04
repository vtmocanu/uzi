import Markdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

// Policy-free markdown core shared by every renderer in the app. It fixes the
// pipeline (react-markdown + remark-gfm, no rehype-raw — raw HTML stays inert
// text) and the one policy-neutral element override (GFM tables scroll inside
// their own container so a wide table never scrolls the page). Link and image
// BEHAVIOUR is deliberately NOT decided here: each caller injects its own trust
// policy via `components` — trusted repo docs rewrite links to SPA routes and
// resolve hashed images (DocMarkdown); untrusted LLM output opens links in a new
// tab with `noopener` and size-caps images (Markdown). Never bake either policy
// into this core, and never add rehype-raw.
const baseComponents: Components = {
  table({ children, node: _node, ...props }) {
    return (
      <div className="my-4 overflow-x-auto">
        <table {...props}>{children}</table>
      </div>
    );
  },
};

export function MarkdownCore({
  content,
  className,
  components,
}: {
  content: string;
  className?: string;
  components?: Components;
}) {
  // Caller overrides win over the base (so a caller could replace the table
  // wrapper too), but callers only supply `a`/`img` in practice.
  const merged = components ? { ...baseComponents, ...components } : baseComponents;
  return (
    <div className={className}>
      <Markdown remarkPlugins={[remarkGfm]} components={merged}>
        {content}
      </Markdown>
    </div>
  );
}
