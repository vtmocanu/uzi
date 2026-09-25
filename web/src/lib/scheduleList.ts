// scheduleList — pure helpers behind the Schedules tab's flat list (PRD #1645).
//
// The Schedules tab renders every schedule, catalog-derived and user-authored, as its own
// row (D2). These helpers name a row and order the list (D3); they read nothing but the
// Schedule DTO and the catalog, so the page and its tests share one definition.

import type { CatalogEntry, Schedule } from "./api";

// nextFireOf is a row's next fire instant: the live preview's first entry, else the
// persisted next_fire_at (D3). null when the row has no upcoming fire.
export function nextFireOf(s: Schedule): string | null {
  return s.next_fires?.[0] ?? s.next_fire_at ?? null;
}

// targetTitle renders a user schedule's human target line (the first column's name).
export function targetTitle(s: Schedule): string {
  switch (s.target) {
    case "issue":
      return s.issue_iid != null ? `#${s.issue_iid}` : "Pinned issue";
    case "sweep":
      return s.labels && s.labels.length > 0
        ? `Sweep · label ${s.labels.join(", ")}`
        : "Sweep eligible issues";
    case "prompt":
      return s.prompt ? `Prompt: ${truncate(s.prompt, 42)}` : "Prompt";
    case "self_improve":
      return "Self-improvement";
  }
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

// scheduleDisplayName is the row's name (D4): the catalog entry's name for a default row,
// today's target title for a user row. A default whose slug is no longer in the catalog
// falls back to the slug, then the target title, so a row is never nameless.
export function scheduleDisplayName(
  s: Schedule,
  entriesBySlug: ReadonlyMap<string, CatalogEntry>,
): string {
  if (s.origin === "default" && s.catalog_slug) {
    return entriesBySlug.get(s.catalog_slug)?.name ?? s.catalog_slug;
  }
  return targetTitle(s);
}

// The D3 bands, in display order.
function band(s: Schedule): number {
  if (s.status === "error") return 0; // parked: needs attention
  if (!s.enabled) return 3; // paused
  return nextFireOf(s) ? 1 : 2; // enabled with / without a next fire
}

function fireTime(s: Schedule): number {
  const iso = nextFireOf(s);
  const t = iso ? new Date(iso).getTime() : Number.NaN;
  return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
}

// sortSchedules orders the list per D3: parked rows, then enabled rows with a next fire
// (soonest first), then enabled rows with none, then paused rows. Within a band, and as
// band 2's tie-break: display name, repo path, then id, so the order is stable across
// reloads. Returns a new array; the input is not mutated.
export function sortSchedules(rows: readonly Schedule[], nameOf: (s: Schedule) => string): Schedule[] {
  const cmpText = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);
  return [...rows].sort((a, b) => {
    const ba = band(a);
    const bb = band(b);
    if (ba !== bb) return ba - bb;
    if (ba === 1) {
      const ta = fireTime(a);
      const tb = fireTime(b);
      if (ta !== tb) return ta - tb;
    }
    return (
      nameOf(a).localeCompare(nameOf(b)) ||
      a.repo_path.localeCompare(b.repo_path) ||
      cmpText(a.id, b.id)
    );
  });
}

// ── D5 filters and the D6 fold ───────────────────────────────────────────────

// The single-select source chip group (D5): All · From catalog · Mine · Paused.
export type SourceFilter = "all" | "catalog" | "mine" | "paused";

// The page-level filter state (D5). `repoId` and `jobSlug` are null when unset. Not
// persisted and not in the URL: it lives in the page's state only.
export interface ScheduleFilter {
  source: SourceFilter;
  repoId: string | null;
  jobSlug: string | null;
}

export const NO_FILTER: ScheduleFilter = { source: "all", repoId: null, jobSlug: null };

function matchesSource(s: Schedule, source: SourceFilter): boolean {
  switch (source) {
    case "all":
      return true;
    case "catalog":
      return s.origin === "default";
    case "mine":
      return s.origin === "user";
    case "paused":
      return !s.enabled;
  }
}

// matchesFilter is the intersection of every active dimension: source chip, repo, job.
// The Job filter names a catalog entry, so it matches that entry's default rows.
export function matchesFilter(s: Schedule, f: ScheduleFilter): boolean {
  if (!matchesSource(s, f.source)) return false;
  if (f.repoId !== null && s.repo_id !== f.repoId) return false;
  if (f.jobSlug !== null && !(s.origin === "default" && s.catalog_slug === f.jobSlug)) return false;
  return true;
}

// sourceCounts is what each source chip would show, computed over ALL schedules rather
// than the current intersection (D5), so a count never depends on the other filters.
export function sourceCounts(all: readonly Schedule[]): Record<SourceFilter, number> {
  const counts: Record<SourceFilter, number> = { all: all.length, catalog: 0, mine: 0, paused: 0 };
  for (const s of all) {
    if (s.origin === "default") counts.catalog++;
    else counts.mine++;
    if (!s.enabled) counts.paused++;
  }
  return counts;
}

// repoOptions lists the distinct repos the schedules span, by path. The Repo select is
// offered only when there are two or more (D5).
export function repoOptions(all: readonly Schedule[]): { id: string; path: string }[] {
  const byId = new Map<string, string>();
  for (const s of all) if (!byId.has(s.repo_id)) byId.set(s.repo_id, s.repo_path);
  return [...byId].map(([id, path]) => ({ id, path })).sort((a, b) => a.path.localeCompare(b.path));
}

// clearHidingFilters returns `f` with every dimension that would hide `s` reset (D12:
// revealing a new row clears only the filters in its way). Returns `f` itself when
// nothing hides the row, so a caller can compare by identity.
export function clearHidingFilters(f: ScheduleFilter, s: Schedule): ScheduleFilter {
  const next: ScheduleFilter = {
    source: matchesSource(s, f.source) ? f.source : "all",
    repoId: f.repoId !== null && s.repo_id !== f.repoId ? null : f.repoId,
    jobSlug: matchesFilter(s, { ...NO_FILTER, jobSlug: f.jobSlug }) ? f.jobSlug : null,
  };
  return next.source === f.source && next.repoId === f.repoId && next.jobSlug === f.jobSlug ? f : next;
}

// isFoldedOnce: a fired one-time schedule, which folds away (D6). A parked row carries
// status "error", never "fired", so it is never folded.
export function isFoldedOnce(s: Schedule): boolean {
  return s.timing === "once" && s.status === "fired";
}

// splitFold applies the D5 → D6 → D3 order: filter, then separate the fired one-shots
// that survived the filter, then sort each part.
export function splitFold(
  all: readonly Schedule[],
  f: ScheduleFilter,
  nameOf: (s: Schedule) => string,
): { shown: Schedule[]; folded: Schedule[] } {
  const matching = all.filter((s) => matchesFilter(s, f));
  return {
    shown: sortSchedules(
      matching.filter((s) => !isFoldedOnce(s)),
      nameOf,
    ),
    folded: sortSchedules(matching.filter(isFoldedOnce), nameOf),
  };
}

// foldRefs lists the folded rows' issue refs for the disclosure line ("#158"), in fold
// order, without duplicates. Rows with no issue target carry no ref.
export function foldRefs(folded: readonly Schedule[]): string[] {
  const refs: string[] = [];
  for (const s of folded) {
    if (s.target !== "issue" || s.issue_iid == null) continue;
    const ref = `#${s.issue_iid}`;
    if (!refs.includes(ref)) refs.push(ref);
  }
  return refs;
}
