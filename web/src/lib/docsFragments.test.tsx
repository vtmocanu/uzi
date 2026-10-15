// @vitest-environment jsdom
import { expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { parseFrontmatter, resolveFromDocs } from "./docs";
import { rehypeHeadingIds } from "./rehypeHeadingIds";

// Include README and repo-only docs as well as the routable docs.ts corpus.
const rawDocs = import.meta.glob("../../../docs/*.md", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

// Local structural HAST view, matching the production heading plugin's approach.
type HastNode = {
  type?: string;
  tagName?: string;
  children?: HastNode[];
};

function rehypeFragmentTargets() {
  return (tree: HastNode) => {
    const targets: HastNode[] = [];
    // Bounded by the document tree. Keep entire target subtrees and stop there:
    // links inside headings must render once, with their inline formatting intact.
    function walk(node: HastNode) {
      if (node.type === "element" && /^(?:h[1-6]|a)$/.test(node.tagName ?? "")) {
        targets.push(node);
      } else {
        for (const child of node.children ?? []) walk(child);
      }
    }
    walk(tree);
    // With no custom components or raw-HTML plugin, other elements cannot render
    // headings or anchors. Production IDs have already been assigned before pruning.
    tree.children = targets;
  };
}

function collect(raw: string) {
  const template = document.createElement("template");
  template.innerHTML = renderToStaticMarkup(
    <ReactMarkdown remarkPlugins={[remarkGfm]} rehypePlugins={[rehypeHeadingIds, rehypeFragmentTargets]} urlTransform={(url) => url}>
      {parseFrontmatter(raw).body}
    </ReactMarkdown>,
  );
  return {
    ids: new Set(Array.from(template.content.querySelectorAll("h1,h2,h3,h4,h5,h6"), (node) => node.id)),
    hrefs: Array.from(template.content.querySelectorAll("a[href]"), (node) => node.getAttribute("href")!),
  };
}

it("collects parsed links and headings while ignoring fenced examples", () => {
  const parsed = collect("## A *heading*\n\n[real](#a-heading)\n\n\x60\x60\x60md\n[example](#broken)\n## fake\n\x60\x60\x60");
  expect([...parsed.ids]).toEqual(["a-heading"]);
  expect(parsed.hrefs).toEqual(["#a-heading"]);
});

it("collects nested targets in order with GFM and encoded hashes", () => {
  const parsed = collect([
    "> ## A *formatted* [link](#a-formatted-link)",
    ">",
    "> - [nested](guide.md#caf%C3%A9)",
    "",
    "## A *formatted* [link](#a-formatted-link-1)",
    "",
    "| Target |",
    "| --- |",
    "| [table](#table) |",
    "",
    "![image](image.png#ignored)",
    "",
    "<https://example.com/#auto> and https://example.com/#bare",
    "",
    '<h2 id="raw">Raw</h2><a href="#raw">raw</a>',
    "",
    "`inline [example](#ignored)`",
    "",
    "```md",
    "## Fake",
    "[example](#ignored)",
    "```",
  ].join("\n"));
  expect([...parsed.ids]).toEqual(["a-formatted-link", "a-formatted-link-1"]);
  expect(parsed.hrefs).toEqual([
    "#a-formatted-link",
    "guide.md#caf%C3%A9",
    "#a-formatted-link-1",
    "#table",
    "https://example.com/#auto",
    "https://example.com/#bare",
  ]);
});

it("resolves all parsed fragments targeting the docs corpus", () => {
  const docs = new Map(Object.entries(rawDocs).map(([key, raw]) => [
    "docs/" + key.slice(key.lastIndexOf("/") + 1), collect(raw),
  ]));
  expect(docs.has("docs/README.md")).toBe(true);
  expect(docs.size).toBeGreaterThan(1);
  const failures: string[] = [];
  let sameDoc = 0;
  let relativeDoc = 0;
  const readme = docs.get("docs/README.md")!;
  expect(readme.ids.has("adding-a-page")).toBe(true);
  expect(readme.ids.has("links")).toBe(true);
  for (const [source, { hrefs }] of docs) {
    for (const href of hrefs) {
      const hashIndex = href.indexOf("#");
      if (hashIndex === -1) continue;
      const path = href.slice(0, hashIndex);
      let target = source;
      if (path) {
        if (/^(?:[a-z][a-z0-9+.-]*:|\/)/i.test(path) || !path.endsWith(".md")) continue;
        target = resolveFromDocs(path);
        if (!docs.has(target)) continue;
        relativeDoc++;
      } else {
        sameDoc++;
      }
      const hash = href.slice(hashIndex + 1);
      let id: string;
      try {
        id = decodeURIComponent(hash);
      } catch {
        failures.push(`${source} -> ${target} #${hash}: malformed hash`);
        continue;
      }
      if (!docs.get(target)!.ids.has(id)) {
        failures.push(`${source} -> ${target} #${hash}: no rendered ID`);
      }
    }
  }
  console.info(`Docs fragments: documents=${docs.size}, same-doc=${sameDoc}, relative-md=${relativeDoc}, README-headings=${readme.ids.size}, failures=${failures.length}`);
  expect(sameDoc).toBeGreaterThan(0);
  expect(relativeDoc).toBeGreaterThan(0);
  expect(failures, failures.join("\n")).toEqual([]);
}, 30000);
