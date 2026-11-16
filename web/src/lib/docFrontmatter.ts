export type Audience = "user" | "operator" | "design" | "contributor";
const AUDIENCES: readonly Audience[] = ["user", "operator", "design", "contributor"];

export interface DocMeta {
  title: string;
  order: number | null;
  audience: Audience;
}


// A tiny parser for our own constrained frontmatter (title/order/audience). We
// avoid gray-matter, which drags Buffer polyfills into the browser bundle. Only
// a leading `---` fence at byte 0 is consumed; a `---` later in the body (e.g.
// inside a code fence) is content. A file with no leading fence — or a
// malformed one — renders as audience "design" (repo-only) rather than erroring,
// so a pre-content build stays green.
export function parseFrontmatter(raw: string): { meta: DocMeta; body: string } {
  const fallback: DocMeta = { title: "", order: null, audience: "design" };
  if (!raw.startsWith("---\n")) return { meta: fallback, body: raw };

  const close = raw.indexOf("\n---", 4);
  if (close === -1) return { meta: fallback, body: raw };

  const yaml = raw.slice(4, close);
  // Body starts after the closing fence's own line (which may carry trailing
  // whitespace); drop any leading blank lines so it begins at real content.
  const afterFence = raw.indexOf("\n", close + 4);
  const body = (afterFence === -1 ? "" : raw.slice(afterFence + 1)).replace(/^\n+/, "");

  const meta: DocMeta = { title: "", order: null, audience: "design" };
  for (const line of yaml.split("\n")) {
    const m = /^([A-Za-z]+):\s*(.*)$/.exec(line.trim());
    if (!m) continue;
    const [, key, rawValue] = m;
    const value = rawValue.trim();
    if (key === "title") {
      meta.title = value;
    } else if (key === "order") {
      const n = Number(value);
      meta.order = value !== "" && Number.isFinite(n) ? n : null;
    } else if (key === "audience" && (AUDIENCES as readonly string[]).includes(value)) {
      meta.audience = value as Audience;
    }
  }
  return { meta, body };
}
