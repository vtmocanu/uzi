import Markdown from "react-markdown";
import { DOC_REHYPE_PLUGINS, DOC_REMARK_PLUGINS } from "./docMarkdownPlugins";

export type DocHeading = { depth: number; text: string; id: string };

type HastNode = {
  type?: string;
  tagName?: string;
  value?: string;
  properties?: Record<string, unknown>;
  children?: HastNode[];
};

// Runs the doc page's own markdown pipeline (react-markdown is synchronous and
// hook-free, so it can be called as a plain function) and records every heading's
// id after rehypeHeadingIds ran. Used at build time by vite-doc-headings.ts so the
// ids are identical to what DocMarkdown renders.
export function collectDocHeadings(body: string): DocHeading[] {
  const out: DocHeading[] = [];
  const capture = () => (tree: HastNode) => {
    function text(n: HastNode): string {
      return n.type === "text" ? (n.value ?? "") : (n.children ?? []).map(text).join("");
    }
    function walk(n: HastNode) {
      if (n.type === "element" && /^h[1-6]$/.test(n.tagName ?? "")) {
        out.push({
          depth: Number((n.tagName as string)[1]),
          text: text(n),
          id: String(n.properties?.id ?? ""),
        });
      }
      (n.children ?? []).forEach(walk);
    }
    walk(tree);
  };
  Markdown({
    children: body,
    remarkPlugins: DOC_REMARK_PLUGINS,
    rehypePlugins: [...(DOC_REHYPE_PLUGINS ?? []), capture],
  });
  return out;
}
