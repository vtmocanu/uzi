// The in-app changelog bundles the repo-root `CHANGELOG.md` at build time — one
// source of truth, no runtime service — mirroring the docs viewer in docs.ts. The
// glob at the bottom resolves RELATIVE TO THIS FILE, so `CHANGELOG.md` must stay
// at the repo root as an ancestor of `web/` (see web/Dockerfile's
// `COPY CHANGELOG.md` and the root .dockerignore that keeps `*.md` in the build
// context). In dev, Vite reads it off disk via `server.fs.allow`.
//
// `parseChangelog` is pure (no `import.meta`), so a test can import it directly.
// Its `body` field is the PARITY SURFACE: it must equal
// `bash scripts/changelog-section.sh body <version>` for every emitted version,
// so the extraction below matches that script's awk/grep byte-for-byte — content
// strictly between `## [<ver>]` and the next `## [`, the release-title marker line
// dropped, leading/trailing blank lines trimmed, and (for the oldest section) the
// EOF compare-link footer retained.

export interface ReleaseGroup {
  category: string;
  bullets: string[];
}

export interface Release {
  /** e.g. "0.48.0", or "Unreleased"/the raw token for the working head. */
  version: string;
  /** e.g. "2026-08-20", null when absent. */
  date: string | null;
  /** The release-title marker text, if present. */
  titleMarker?: string;
  /** The PARITY SURFACE — see the module comment; footer retained for the oldest. */
  body: string;
  /** The RENDER SURFACE — `### Category` subsections and their bullets. */
  groups: ReleaseGroup[];
  /** True for the `## [Unreleased]` head. */
  unreleased: boolean;
  /** True for a `## [X.Y.Z] ... [NOT RELEASED]` section. */
  notReleased: boolean;
}

// Section boundary, matching the shell's `^## \[` exactly so the two agree on what
// a "section" is. Version is the first `[...]`; a looser regex would risk drifting
// from the parity oracle.
const SECTION_HEAD_RE = /^## \[/;
const VERSION_RE = /^## \[([^\]]*)\]/;
const DATE_RE = /-\s*(\d{4}-\d{2}-\d{2})/;
const NOT_RELEASED_RE = /\[NOT RELEASED\]/;

// The release-title marker: same shape the shell drops/extracts. LINE matches a
// whole marker line (dropped from `body`); EXTRACT pulls the trimmed marker text.
const MARKER_LINE_RE = /^<!--\s*release-title:.*-->\s*$/;
const MARKER_EXTRACT_RE = /^<!--\s*release-title:\s*(.*?)\s*-->/;

const BLANK_RE = /^\s*$/;
const SUBHEAD_RE = /^###\s+(.*\S)\s*$/;
const BULLET_RE = /^- (.*)$/;

// Parse the section into `### Category` subsections, each collecting `- ` bullets.
// A bullet may wrap onto indented continuation lines; those are folded into the
// bullet's text. Blank lines and unindented non-bullet lines (e.g. `[x]: url`
// reference-definition footers) close the current bullet and render as nothing.
function parseGroups(sectionLines: string[]): ReleaseGroup[] {
  const groups: ReleaseGroup[] = [];
  let current: ReleaseGroup | null = null;
  let bulletParts: string[] | null = null;

  const flush = () => {
    if (current && bulletParts) {
      current.bullets.push(bulletParts.join(" ").trim());
    }
    bulletParts = null;
  };

  for (const line of sectionLines) {
    const head = SUBHEAD_RE.exec(line);
    if (head) {
      flush();
      current = { category: head[1].trim(), bullets: [] };
      groups.push(current);
      continue;
    }
    if (!current) continue;
    const bullet = BULLET_RE.exec(line);
    if (bullet) {
      flush();
      bulletParts = [bullet[1].trim()];
      continue;
    }
    if (BLANK_RE.test(line)) {
      flush();
      continue;
    }
    // An indented, non-blank line continues the current bullet's text.
    if (bulletParts && /^\s+\S/.test(line)) {
      bulletParts.push(line.trim());
      continue;
    }
    // Anything else (a footer ref-def line, stray prose) ends the bullet.
    flush();
  }
  flush();
  return groups;
}

function buildRelease(headingLine: string, sectionLines: string[]): Release | null {
  const vm = VERSION_RE.exec(headingLine);
  if (!vm) return null;
  const version = vm[1].trim();
  const dm = DATE_RE.exec(headingLine);
  const date = dm ? dm[1] : null;
  const notReleased = NOT_RELEASED_RE.test(headingLine);
  const unreleased = version === "Unreleased";

  // First release-title marker wins.
  let titleMarker: string | undefined;
  for (const line of sectionLines) {
    const m = MARKER_EXTRACT_RE.exec(line);
    if (m) {
      titleMarker = m[1].trim();
      break;
    }
  }

  // body: drop marker lines, then trim leading and trailing blank lines. The
  // oldest section's footer is non-blank, so it survives the trailing trim.
  const bodyLines = sectionLines.filter((l) => !MARKER_LINE_RE.test(l));
  let start = 0;
  let end = bodyLines.length - 1;
  while (start <= end && BLANK_RE.test(bodyLines[start])) start++;
  while (end >= start && BLANK_RE.test(bodyLines[end])) end--;
  const body = bodyLines.slice(start, end + 1).join("\n");

  const groups = parseGroups(sectionLines);

  const release: Release = { version, date, body, groups, unreleased, notReleased };
  if (titleMarker !== undefined) release.titleMarker = titleMarker;
  return release;
}

// Parse the raw CHANGELOG into releases, newest-first (file order is already
// newest-first). An empty `[Unreleased]` (no groups and blank body) is omitted; a
// populated one is kept. Fail-safe: malformed or empty input returns a best-effort
// list (possibly empty) and never throws, mirroring docs.ts's posture.
export function parseChangelog(raw: string): Release[] {
  try {
    const lines = raw.split("\n");
    const releases: Release[] = [];
    let i = 0;
    while (i < lines.length && !SECTION_HEAD_RE.test(lines[i])) i++;
    while (i < lines.length) {
      const headingLine = lines[i];
      i++;
      const sectionLines: string[] = [];
      while (i < lines.length && !SECTION_HEAD_RE.test(lines[i])) {
        sectionLines.push(lines[i]);
        i++;
      }
      const release = buildRelease(headingLine, sectionLines);
      if (
        release &&
        !(release.unreleased && release.groups.length === 0 && release.body === "")
      ) {
        releases.push(release);
      }
    }
    return releases;
  } catch {
    return [];
  }
}

export interface BulletTitleSplit {
  /** The bold title text WITHOUT the surrounding `**`, or null if the bullet has no leading bold span. */
  title: string | null;
  /** The remaining description; "" when the bullet is title-only. */
  description: string;
}

// A changelog bullet is authored as a bold title followed by its description
// (folded to one string by parseGroups). Split the LEADING bold span off as the
// title so the drawer can render it on its own line. Both bullet shapes fold to
// the same string, so this handles the current title-line-then-description form
// and the legacy single-physical-line inline form identically. A bullet with no
// leading bold span has no separable title (title = null) and renders as-is.
//
// The inner capture is NON-GREEDY (`[\s\S]+?`) so it stops at the FIRST closing
// `**`, capturing only the leading bold span as the title even when the
// description itself contains later `**bold**` runs — a greedy match would
// swallow everything up to the last `**` and mis-split the bullet.
export function splitBulletTitle(bullet: string): BulletTitleSplit {
  const m = /^\*\*([\s\S]+?)\*\*\s*([\s\S]*)$/.exec(bullet);
  if (!m) return { title: null, description: bullet };
  return { title: m[1].trim(), description: m[2].trim() };
}

// Browser-only bundle: the repo-root CHANGELOG.md, resolved relative to this file
// (there is exactly one match). Kept fail-safe if the glob is empty.
const rawChangelog = import.meta.glob("../../../CHANGELOG.md", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

export const releases: Release[] = parseChangelog(Object.values(rawChangelog)[0] ?? "");
