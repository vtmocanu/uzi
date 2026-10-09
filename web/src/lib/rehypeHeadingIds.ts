import { createSlugger } from "./headingSlug";

// Local structural HAST view: no dependency on transitive AST packages.
type HastNode = {
  type?: string;
  tagName?: string;
  value?: string;
  properties?: Record<string, unknown>;
  children?: HastNode[];
};

export function rehypeHeadingIds() {
  return (tree: HastNode) => {
    const slugger = createSlugger();
    // One recursive walk, bounded by the document tree. Reserve heading slots
    // on entry to preserve document order while collecting descendant text.
    const headings: { node: HastNode; text: string }[] = [];
    function walk(node: HastNode): string {
      const heading = node.type === "element" && /^h[1-6]$/.test(node.tagName ?? "")
        ? { node, text: "" }
        : undefined;
      if (heading) headings.push(heading);
      const text = node.type === "text"
        ? node.value ?? ""
        : (node.children ?? []).map(walk).join("");
      if (heading) heading.text = text;
      return text;
    }
    walk(tree);
    for (const { node, text } of headings) {
      node.properties = { ...node.properties, id: slugger.slug(text) };
    }
  };
}
