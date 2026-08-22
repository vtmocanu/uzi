// Repo-base + PRD-link transform for the in-app changelog drawer (PRD #415 M3).
//
// CHANGELOG policy keeps `PRD #N` as PLAIN TEXT while PR refs are already
// `[#N](url)` links, so linkifying PRD refs is a safe POST-PARSE pass over plain
// text only (see linkifyPrdRefs). This module does NOT modify CHANGELOG.md and
// does NOT touch the parity surface — M4 compares the raw `Release.body`, never
// the rendered output, so a render-time link transform cannot drift the two.

// The repo the changelog's issue/tag links point at. Overridable at build time via
// VITE_UZI_CHANGELOG_REPO_URL (a fork can retarget its own issues) and defaults to
// the canonical repo. Any trailing slash is stripped so `${base}/issues/N` never
// doubles the separator.
const rawBase: string =
  import.meta.env.VITE_UZI_CHANGELOG_REPO_URL || "https://github.com/vtmocanu/uzi";
export const CHANGELOG_REPO_BASE = rawBase.replace(/\/+$/, "");

// prdIssueUrl points at the GitHub issue for a PRD number.
export function prdIssueUrl(n: number | string): string {
  return `${CHANGELOG_REPO_BASE}/issues/${n}`;
}

// releaseTagUrl points at the `vX.Y.Z` GitHub release tag. Only meaningful for a
// plain-semver version (Model B tags `v` + the version); the caller links a
// heading only for a released semver version.
export function releaseTagUrl(version: string): string {
  return `${CHANGELOG_REPO_BASE}/releases/tag/v${version}`;
}

// linkifyPrdRefs replaces each plain-text `PRD #<N>` with a markdown link to its
// issue. It runs on a bullet's markdown AFTER the changelog parse, before the
// string reaches react-markdown. It matches the literal `PRD #<digits>` only, so:
//   - an already-linked PR ref `[#413](…/pull/413)` is untouched (no `PRD #`),
//   - a bare `#7` is untouched (no `PRD #`),
//   - and because CHANGELOG policy never writes `[PRD #N](…)`, there is no
//     already-linked PRD ref to double-wrap in practice.
export function linkifyPrdRefs(md: string): string {
  return md.replace(/PRD #(\d+)/g, (_m, n) => `[PRD #${n}](${prdIssueUrl(n)})`);
}
