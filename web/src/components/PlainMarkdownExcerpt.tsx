import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { stripUnsafeChars } from "../lib/safeText";

type Node = {
  type: string;
  tagName?: string;
  value?: string;
  properties?: { alt?: unknown };
  children?: Node[];
};

// Flatten parsed Markdown rather than guessing at delimiters. Raw HTML is omitted;
// entities have already decoded when the final unsafe-character scrub runs.
// Each node is visited once; work and stack are bounded by the parsed input size.
function plainExcerpt() {
  return (tree: Node) => {
    const parts: string[] = [];
    const pending = [tree];
    while (pending.length) {
      const node = pending.pop()!;
      if (node.type === "text") parts.push(node.value ?? "");
      if (node.tagName === "img" && typeof node.properties?.alt === "string") parts.push(node.properties.alt);
      if (node.tagName === "br") parts.push(" ");
      if (node.children) {
        if (node.tagName && /^(p|h[1-6]|li|pre|tr|td|th|blockquote)$/.test(node.tagName)) {
          pending.push({ type: "text", value: " " });
        }
        for (let i = node.children.length - 1; i >= 0; i--) pending.push(node.children[i]);
      }
    }
    tree.children = [{ type: "text", value: stripUnsafeChars(parts.join("")).replace(/\s+/g, " ").trim().slice(0, 150) }];
  };
}

export function PlainMarkdownExcerpt({ content }: { content: string }) {
  return <ReactMarkdown remarkPlugins={[remarkGfm]} rehypePlugins={[plainExcerpt]}>{stripUnsafeChars(content)}</ReactMarkdown>;
}
