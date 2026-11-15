import type { DocHeading } from "./docHeadingsPipeline";

// Per-doc heading ids computed at build time by vite-doc-headings.ts with the same
// pipeline DocMarkdown renders, so `/docs/<slug>#<id>` links match the page.
const index = import.meta.glob<DocHeading[]>("../../../docs/*.md", {
  query: "?doc-headings",
  import: "default",
  eager: true,
});

const bySlug = new Map<string, DocHeading[]>(
  Object.entries(index).map(([key, headings]) => [
    key.slice(key.lastIndexOf("/") + 1).replace(/\.md$/, ""),
    headings,
  ]),
);

export function docHeadingsFor(slug: string): DocHeading[] {
  return bySlug.get(slug) ?? [];
}
