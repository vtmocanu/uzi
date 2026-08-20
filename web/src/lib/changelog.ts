// The in-app changelog panel bundles the repo's `CHANGELOG.md` at build time —
// one source of truth, no runtime service, no committed second copy. Mirrors the
// docs viewer (web/src/lib/docs.ts): a framework-agnostic PURE parser plus a
// browser-only `?raw` import. The `?raw` path resolves RELATIVE TO THIS FILE, so
// `../../../CHANGELOG.md` is the repo root — which only works if the web/ <->
// repo-root layout is preserved in the image (see web/Dockerfile's
// `COPY CHANGELOG.md`). In dev, Vite reads it off disk via `server.fs.allow:[".."]`.
//
// `parseChangelog` is exported and PURE (no `import.meta`) so a plain vitest test
// (and M4's parity test) can import it without executing the browser-only glob.
import changelogRaw from "../../../CHANGELOG.md?raw";

export interface ReleaseGroup {
  /** Text after `### `, e.g. "Added". */
  category: string;
  /** Bullet contents, inline markdown (`[#N](…)` links) preserved verbatim. */
  bullets: string[];
}

export interface Release {
  /** Token inside the FIRST `[...]` of the heading: "0.48.0", or "Unreleased". */
  version: string;
  /** Text after "] - " up to any trailing " [NOT RELEASED]" / EOL, else null. */
  date: string | null;
  /** The release-title marker text if the section carries one. */
  titleMarker?: string;
  /**
   * PARITY SURFACE — equals `scripts/changelog-section.sh body <ver>` modulo the
   * shell's single trailing newline: the lines strictly between this heading and
   * the next `## [` (or EOF), with the `release-title` marker line dropped and
   * leading/trailing blank lines trimmed. Joined with "\n" and carrying NO trailing
   * newline (awk's `print` appends one to stdout; command substitution and M4's
   * parity comparator both strip it, so the two agree byte-for-byte once normalized).
   * The oldest section retains the trailing reference-link footer, exactly as the
   * shell does.
   */
  body: string;
  /** RENDER SURFACE — the `### <Category>` subsections split into bullets. */
  groups: ReleaseGroup[];
  /** True iff a real semver version AND not [Unreleased] AND not [NOT RELEASED]. */
  released: boolean;
}

// A version heading, matched as a PREFIX so a trailing " - date [NOT RELEASED]"
// is tolerated — identical boundary set to changelog-section.sh's awk.
const HEADING_RE = /^## \[/;
// Body-removal form: mirrors the shell's `grep -vE '^<!--…release-title:.*-->…$'`.
const MARKER_LINE_RE = /^<!--\s*release-title:.*-->\s*$/;
// Extraction form: mirrors the shell's looser awk `match(...)` on the prefix.
const MARKER_PREFIX_RE = /^<!--\s*release-title:\s*/;
const MARKER_SUFFIX_RE = /\s*-->.*$/;
const BLANK_RE = /^\s*$/;
// Reference-definition footer line, e.g. `[0.48.0]: https://…` — belongs to no
// category and renders as nothing.
const FOOTER_REF_RE = /^\[[^\]]+\]:\s/;
const BULLET_RE = /^-\s+/;
const CATEGORY_RE = /^###\s+(.*)$/;
const CONTINUATION_RE = /^\s+\S/;
const SEMVER_RE = /^\d+\.\d+\.\d+$/;

function parseHeading(line: string): { version: string; date: string | null; released: boolean } {
  const open = line.indexOf("[");
  const close = line.indexOf("]");
  const version = open !== -1 && close > open ? line.slice(open + 1, close) : "";
  const rest = close !== -1 ? line.slice(close + 1) : "";
  const notReleased = /\[NOT RELEASED\]/.test(rest);

  let date: string | null = null;
  const m = /^\s*-\s*(.*)$/.exec(rest);
  if (m) {
    const d = m[1].replace(/\s*\[NOT RELEASED\]\s*$/, "").trim();
    date = d === "" ? null : d;
  }

  const released = SEMVER_RE.test(version) && !notReleased;
  return { version, date, released };
}

// The parity body: drop the marker line, then trim leading/trailing blank lines
// (interior blanks kept). No trailing newline (join with "\n").
function buildBody(slice: string[]): string {
  const kept = slice.filter((l) => !MARKER_LINE_RE.test(l));
  let start = 0;
  let end = kept.length - 1;
  while (start <= end && BLANK_RE.test(kept[start])) start++;
  while (end >= start && BLANK_RE.test(kept[end])) end--;
  if (start > end) return "";
  return kept.slice(start, end + 1).join("\n");
}

function extractTitleMarker(slice: string[]): string | undefined {
  for (const line of slice) {
    if (MARKER_PREFIX_RE.test(line)) {
      return line.replace(MARKER_PREFIX_RE, "").replace(MARKER_SUFFIX_RE, "").trim();
    }
  }
  return undefined;
}

// The render surface: each `### <Category>` becomes a group; its `- ` lines become
// bullets, with indented continuation lines appended (joined with "\n"). Footer
// reference-definitions and content before the first category are ignored.
function buildGroups(slice: string[]): ReleaseGroup[] {
  const groups: ReleaseGroup[] = [];
  let current: ReleaseGroup | null = null;
  let bulletLines: string[] | null = null;

  const flushBullet = () => {
    if (bulletLines && current) current.bullets.push(bulletLines.join("\n"));
    bulletLines = null;
  };

  for (const line of slice) {
    const cat = CATEGORY_RE.exec(line);
    if (cat) {
      flushBullet();
      current = { category: cat[1].trim(), bullets: [] };
      groups.push(current);
      continue;
    }
    if (!current || FOOTER_REF_RE.test(line)) {
      flushBullet();
      continue;
    }
    if (BULLET_RE.test(line)) {
      flushBullet();
      bulletLines = [line.replace(BULLET_RE, "")];
      continue;
    }
    if (bulletLines && CONTINUATION_RE.test(line)) {
      bulletLines.push(line);
      continue;
    }
    flushBullet();
  }
  flushBullet();
  return groups;
}

// Parse a Keep-a-Changelog document into releases, newest-first (file order).
// Fails SAFE: empty input, or no `## [` heading, returns [] rather than throwing.
export function parseChangelog(raw: string): Release[] {
  if (!raw) return [];
  const lines = raw.split("\n");

  const headings: number[] = [];
  for (let i = 0; i < lines.length; i++) {
    if (HEADING_RE.test(lines[i])) headings.push(i);
  }
  if (headings.length === 0) return [];

  const releases: Release[] = [];
  for (let h = 0; h < headings.length; h++) {
    const headingIdx = headings[h];
    const nextIdx = h + 1 < headings.length ? headings[h + 1] : lines.length;
    // Strictly between this heading and the next (or EOF) — excludes both headings.
    const slice = lines.slice(headingIdx + 1, nextIdx);

    const { version, date, released } = parseHeading(lines[headingIdx]);
    const body = buildBody(slice);
    const groups = buildGroups(slice);

    // An empty [Unreleased] carries nothing to show — omit it.
    if (version === "Unreleased" && body === "" && groups.length === 0) continue;

    const release: Release = { version, date, body, groups, released };
    const titleMarker = extractTitleMarker(slice);
    if (titleMarker !== undefined) release.titleMarker = titleMarker;
    releases.push(release);
  }
  return releases;
}

// The bundled instance, parsed from the build-time `?raw` import.
export const releases: Release[] = parseChangelog(changelogRaw);
