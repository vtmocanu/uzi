import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { isInsideDocsDir } from "./src/lib/docHeadingsFileGuard";
import type { Plugin } from "vite";
import { parseFrontmatter } from "./src/lib/docFrontmatter";
import { collectDocHeadings } from "./src/lib/docHeadingsPipeline";

// Virtual `<doc>.md?doc-headings` module: the doc's heading ids, computed at build
// time with the same pipeline DocMarkdown runs. All logic lives in src/lib.
const SUFFIX = "?doc-headings";
// Only the repo's own docs/ may be read: the dev server does not apply its fs.allow check
// to this query, so an unbounded load would serve the headings of any reachable .md file.
const DOCS_DIR = resolve(dirname(fileURLToPath(import.meta.url)), "..", "docs");


export function docHeadingsPlugin(): Plugin {
  return {
    name: "uzi-doc-headings",
    enforce: "pre",
    async resolveId(id, importer, options) {
      const [bare, query = ""] = id.split("?");
      if (!bare.endsWith(".md") || !query.split("&").includes("doc-headings")) return null;
      const r = await this.resolve(bare, importer, { ...options, skipSelf: true });
      return r ? r.id.split("?")[0] + SUFFIX : null;
    },
    load(id) {
      if (!id.endsWith(SUFFIX)) return null;
      const file = id.slice(0, -SUFFIX.length);
      if (!isInsideDocsDir(DOCS_DIR, file)) throw new Error(`doc-headings: refusing a file outside docs/: ${file}`);
      const raw = readFileSync(file, "utf8");
      const headings = collectDocHeadings(parseFrontmatter(raw).body);
      return { code: "export default " + JSON.stringify(headings), map: null };
    },
  };
}
