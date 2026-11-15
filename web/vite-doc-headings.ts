import { readFileSync } from "node:fs";
import type { Plugin } from "vite";
import { parseFrontmatter } from "./src/lib/docFrontmatter";
import { collectDocHeadings } from "./src/lib/docHeadingsPipeline";

// Virtual `<doc>.md?doc-headings` module: the doc's heading ids, computed at build
// time with the same pipeline DocMarkdown runs. All logic lives in src/lib.
const SUFFIX = "?doc-headings";

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
      const raw = readFileSync(id.slice(0, -SUFFIX.length), "utf8");
      const headings = collectDocHeadings(parseFrontmatter(raw).body);
      return { code: "export default " + JSON.stringify(headings), map: null };
    },
  };
}
