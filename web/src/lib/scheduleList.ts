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
