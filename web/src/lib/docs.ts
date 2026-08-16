// The docs viewer bundles the repo's `docs/` at build time — one source of
// truth, no runtime service. The globs below resolve RELATIVE TO THIS FILE, so
// `docs/` must stay a sibling of `web/` (see web/Dockerfile's `COPY docs/` and
// the root .dockerignore that keeps `*.md` in the build context). In dev, Vite
// reads these off disk, which needs `server.fs.allow` to include the repo root.

// Raw markdown for every `docs/*.md`. README.md is filtered out below.
const rawDocs = import.meta.glob("../../../docs/*.md", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

// Hashed asset URLs for `docs/img/*` (emitted as lazily-fetched files, not
// inlined into the JS bundle). Empty until M3 adds screenshots — that is fine.
const rawAssets = import.meta.glob("../../../docs/img/*", {
  query: "?url",
  import: "default",
  eager: true,
}) as Record<string, string>;

// Pinned GitLab blob base for links to repo-only files (design/operator docs,
// ../plan.md, etc.) that are not bundled as in-app pages.
export const GITLAB_BLOB_BASE = "https://gitlab.example.com/vtmocanu/uzi/-/blob/main/";

const DOCS_GLOB_PREFIX = "../../../docs/";

export type Audience = "user" | "operator" | "design" | "contributor";
const AUDIENCES: readonly Audience[] = ["user", "operator", "design", "contributor"];

export interface DocMeta {
  title: string;
  order: number | null;
  audience: Audience;
}

export interface Doc {
  slug: string;
  meta: DocMeta;
  body: string;
  summary: string;
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

// A one-line blurb for the index: the first real paragraph after the leading
// `# Heading`, stripped of markdown syntax and truncated.
export function summarize(body: string): string {
  const lines = body.split("\n");
  let i = 0;
  while (i < lines.length && lines[i].trim() === "") i++;
  if (i < lines.length && lines[i].startsWith("# ")) i++; // skip the page's own H1
  while (i < lines.length && lines[i].trim() === "") i++;

  const paragraph: string[] = [];
  for (; i < lines.length; i++) {
    const t = lines[i].trim();
    if (t === "") break;
    if (t.startsWith("#") || t.startsWith("|") || t.startsWith("```")) break;
    paragraph.push(t);
  }
  const text = paragraph
    .join(" ")
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1") // links/images -> their text
    .replace(/[*_`]/g, "")
    .replace(/\s+/g, " ")
    .trim();
  return text.length > 160 ? `${text.slice(0, 157).trimEnd()}…` : text;
}

function slugOf(globKey: string): string {
  return globKey.slice(globKey.lastIndexOf("/") + 1).replace(/\.md$/, "");
}

const docsBySlug = new Map<string, Doc>();
for (const [key, raw] of Object.entries(rawDocs)) {
  const slug = slugOf(key);
  if (slug === "README") continue;
  const { meta, body } = parseFrontmatter(raw);
  docsBySlug.set(slug, { slug, meta, body, summary: summarize(body) });
}

// Asset lookup keyed by `docs/`-relative path, e.g. "img/board-move-card.png".
const imageUrlByPath = new Map<string, string>();
for (const [key, url] of Object.entries(rawAssets)) {
  imageUrlByPath.set(key.slice(DOCS_GLOB_PREFIX.length), url);
}

export function getDoc(slug: string): Doc | undefined {
  return docsBySlug.get(slug);
}

// Whether a doc slug is routable in-app for this viewer: `user` pages are
// routable for everyone; `operator` pages are additionally routable for admins
// (role-aware in-app docs, issue #75). Everything else stays repo-only.
export function isRoutableDoc(slug: string, isAdmin: boolean): boolean {
  const audience = docsBySlug.get(slug)?.meta.audience;
  return audience === "user" || (isAdmin && audience === "operator");
}

// Index order: `order` ascending, missing order last, slug as a stable
// tiebreak. Pure (operates on its input) so the ordering rule is unit-testable.
export function sortDocsForIndex(docs: Doc[]): Doc[] {
  return [...docs].sort((a, b) => {
    const ao = a.meta.order ?? Number.POSITIVE_INFINITY;
    const bo = b.meta.order ?? Number.POSITIVE_INFINITY;
    return ao - bo || a.slug.localeCompare(b.slug);
  });
}

// In-app index: only `audience: user` pages, ordered by sortDocsForIndex.
export function listUserDocs(): Doc[] {
  return sortDocsForIndex([...docsBySlug.values()].filter((d) => d.meta.audience === "user"));
}

// Admin-only in-app index: the `audience: operator` pages, ordered the same way.
export function listOperatorDocs(): Doc[] {
  return sortDocsForIndex([...docsBySlug.values()].filter((d) => d.meta.audience === "operator"));
}

// The listed/searchable corpus for a viewer: user docs for everyone, plus the
// operator docs (user docs first) for an admin. Both the index and the search
// corpus derive from this.
export function docsForIndex(isAdmin: boolean): Doc[] {
  return isAdmin ? [...listUserDocs(), ...listOperatorDocs()] : listUserDocs();
}

// Resolve a doc-relative POSIX path against `docs/` and normalize `.`/`..` into
// a repo-relative path (e.g. "configuration.md" -> "docs/configuration.md",
// "../plan.md" -> "plan.md", "img/x.png" -> "docs/img/x.png").
export function resolveFromDocs(rel: string): string {
  const out: string[] = [];
  for (const part of `docs/${rel}`.split("/")) {
    if (part === "" || part === ".") continue;
    if (part === "..") out.pop();
    else out.push(part);
  }
  return out.join("/");
}

export interface RewrittenHref {
  href: string;
  /** External (off-app) target — gets target=_blank + rel=noopener noreferrer. */
  external: boolean;
  /** In-app SPA route — render with react-router's <Link>. */
  internal: boolean;
}

// javascript:/vbscript:/data:/file: are unsafe as link or image targets. Strip
// ASCII control/space chars first so a smuggled scheme (e.g. "java\tscript:")
// cannot slip past the check.
const DANGEROUS_SCHEME = /^(?:javascript|vbscript|data|file):/i;
export function schemeIsDangerous(url: string): boolean {
  // 🔴 THE CONTROL-CHARACTER RANGE IS THE SECURITY GUARD, not an oversight: this
  // strip is what stops `java\tscript:` reaching the scheme test. Do not narrow
  // the class to satisfy the linter — that reopens the smuggling path the comment
  // above describes.
  // eslint-disable-next-line no-control-regex
  return DANGEROUS_SCHEME.test(url.replace(/[\x00-\x20]+/g, ""));
}

// Pure core of link rewriting. `isRoutablePage(slug)` reports whether a doc slug
// is routable in-app for this viewer (see isRoutableDoc: `user` for everyone,
// `operator` too for admins); passing it in (rather than reading module state)
// keeps this rule unit-testable without the build-time doc glob.
//   - `#anchor` and absolute/external URLs pass through untouched.
//   - dangerous schemes are neutralized to an empty href.
//   - a relative `*.md` that resolves to an in-app-routable page -> `/docs/:slug`
//     (in-app route), preserving any `#anchor`.
//   - any other relative path (non-routable doc, ../plan.md, ...) -> the pinned
//     GitLab blob URL, preserving any `#anchor`.
export function resolveHref(href: string, isRoutablePage: (slug: string) => boolean): RewrittenHref {
  // Protocol-relative (`//host`, and the `/\` variant browsers also treat as
  // such) is an off-app URL, not an in-app route — classify before the
  // single-slash internal case below.
  if (/^\/[/\\]/.test(href)) {
    return { href, external: true, internal: false };
  }
  if (href.startsWith("#") || href.startsWith("/")) {
    return { href, external: false, internal: href.startsWith("/") };
  }
  // Defense-in-depth: independently neutralize dangerous URL schemes rather than
  // leaning only on react-markdown's defaultUrlTransform (which a future
  // urlTransform override could disable). Content is repo-trusted, so this only
  // fires on a mistake, but it closes the XSS door structurally.
  if (schemeIsDangerous(href)) {
    return { href: "", external: false, internal: false };
  }
  if (/^[a-z][a-z0-9+.-]*:/i.test(href)) {
    return { href, external: /^https?:/i.test(href), internal: false };
  }

  const hashIdx = href.indexOf("#");
  const anchor = hashIdx === -1 ? "" : href.slice(hashIdx);
  const path = hashIdx === -1 ? href : href.slice(0, hashIdx);
  if (path === "") return { href, external: false, internal: false };

  const repoPath = resolveFromDocs(path);
  if (repoPath.startsWith("docs/") && repoPath.endsWith(".md")) {
    const slug = repoPath.slice("docs/".length, -".md".length);
    if (isRoutablePage(slug)) {
      return { href: `/docs/${slug}${anchor}`, external: false, internal: true };
    }
  }
  return { href: `${GITLAB_BLOB_BASE}${repoPath}${anchor}`, external: true, internal: false };
}

// Bound to the docs actually bundled in this build. A relative `*.md` link
// routes in-app when its target is routable for this viewer — user pages for
// everyone, and operator pages too for an admin (issue #75 M1); anything else
// resolves to the pinned GitLab blob URL.
export function rewriteHref(href: string, isAdmin: boolean): RewrittenHref {
  return resolveHref(href, (slug) => isRoutableDoc(slug, isAdmin));
}

// Resolve a relative image src in doc markdown to its hashed asset URL. Absolute
// (http[s]) and root-absolute srcs pass through; dangerous schemes (including
// data:) are neutralized to an empty src; an unbundled relative path falls back
// to the raw src.
export function resolveImageSrc(src: string): string {
  if (src === "") return "";
  if (schemeIsDangerous(src)) return "";
  if (/^[a-z][a-z0-9+.-]*:/i.test(src) || src.startsWith("/")) return src;
  const rel = resolveFromDocs(src).replace(/^docs\//, "");
  return imageUrlByPath.get(rel) ?? src;
}
