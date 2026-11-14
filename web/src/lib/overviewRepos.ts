// Pure model for the new Overview's Repos chip (admins, issue #4334). Nothing here
// fetches or renders. "Active" is derived from the same run rows the People chip reads (the
// viewer's own runs plus the admin run list, through peopleRows / runPart), so the two chips
// agree on what a listed non-terminal run is; the repo total and the last-run times come from
// the admin repo-activity read. Each source can fail on its own and each part of the model
// degrades independently instead of guessing.

import type { RepoActivity, RunSummaryItem } from "./api";
import { maskRepoPath } from "./demoMask";
import {
  ADMIN_RUNS_CAP,
  peopleRows,
  pillText,
  pillTone,
  runPart,
  type IdleSummary,
  type PillParts,
} from "./overviewPeople";
import { formatAgo } from "./rateLimits";
import { stripUnsafeChars } from "./safeText";
import type { AdminRunsState } from "./useOverviewData";

const DAY_MS = 24 * 60 * 60 * 1000;
// A repo whose newest run is younger than this is "recent"; older (or exactly this old) is idle.
const RECENT_MS = 7 * DAY_MS;

export interface ActiveRepo {
  key: string;
  path: string;
  badge: string;
  parts: PillParts;
  pill: string;
  tone: "warn" | "ok" | "neutral";
}

export interface RecentRepo {
  key: string;
  path: string;
  badge: string;
  ago: string;
}

export type RepoActiveState =
  | { state: "unavailable" }
  | { state: "loading" }
  | { state: "ok"; repos: ActiveRepo[]; atLeast: boolean; count: number };

export interface RepoActivityModel {
  active: RepoActiveState;
  total: number | null;
  recent: RecentRepo[] | null;
  idle: IdleSummary | null;
  listFailed: boolean;
}

// The text shown for a repo: the read's path when the repo is in it, else the run's own
// repo_path. Always stripped of control and bidi characters, then demo-masked.
function displayPath(raw: string, demo: boolean): string {
  const stripped = stripUnsafeChars(raw);
  // An empty last segment must not let maskRepoPath's filter(Boolean) promote the namespace to the repo name.
  if ((stripped.split("/").pop() ?? "").trim() === "") return "Unknown";
  const clean = maskRepoPath(stripped, demo).trim();
  return clean === "" ? "Unknown" : clean;
}

// The first two letters or digits of the last path segment, case kept; "?" when it has none.
function badgeOf(path: string): string {
  const segments = path.split("/");
  const last = segments[segments.length - 1] ?? "";
  const out = [...last.replace(/[^\p{L}\p{N}]/gu, "")].slice(0, 2).join("");
  return out === "" ? "?" : out;
}

function parseMs(iso: string | null): number | null {
  if (iso == null) return null;
  const t = Date.parse(iso);
  return Number.isFinite(t) ? t : null;
}

export function repoActivityModel(input: {
  ownRuns: RunSummaryItem[];
  adminRuns: AdminRunsState;
  viewerEmail: string;
  repoActivity: { repos: RepoActivity[] | null; failed: boolean };
  now: number;
  demo: boolean;
}): RepoActivityModel {
  const { ownRuns, adminRuns, viewerEmail, repoActivity, now, demo } = input;
  const readRepos = repoActivity.repos;
  const readByID = new Map<string, RepoActivity>();
  for (const r of readRepos ?? []) readByID.set(r.id, r);
  const readOk = readRepos !== null && !repoActivity.failed;

  let active: RepoActiveState;
  const activeIDs = new Set<string>();
  if (adminRuns.failed) {
    active = { state: "unavailable" };
  } else if (adminRuns.runs === null) {
    active = { state: "loading" };
  } else {
    const byRepo = new Map<string, { parts: PillParts; runPath: string }>();
    for (const { run } of peopleRows(ownRuns, adminRuns.runs, viewerEmail)) {
      if (run.repo_id == null) continue;
      const part = runPart(run);
      if (part === null) continue;
      let entry = byRepo.get(run.repo_id);
      if (!entry) {
        entry = { parts: { running: 0, queued: 0, waiting: 0, owner: 0 }, runPath: run.repo_path };
        byRepo.set(run.repo_id, entry);
      }
      entry.parts[part]++;
    }
    const repos: ActiveRepo[] = [];
    for (const [id, { parts, runPath }] of byRepo) {
      activeIDs.add(id);
      const path = displayPath(readByID.get(id)?.path_with_namespace ?? runPath, demo);
      repos.push({ key: id, path, badge: badgeOf(path), parts, pill: pillText(parts), tone: pillTone(parts) });
    }
    repos.sort((a, b) => b.parts.running - a.parts.running || a.path.localeCompare(b.path));
    active = {
      state: "ok",
      repos,
      atLeast: adminRuns.runs.length >= ADMIN_RUNS_CAP,
      count: repos.length,
    };
  }

  // No total while the read is loading or has failed. Otherwise the read's rows plus any
  // active repo it lacks (a run list can name a repo the read does not, so count never
  // exceeds the total).
  let total: number | null = null;
  if (readRepos !== null && !repoActivity.failed) {
    total = readRepos.length;
    for (const id of activeIDs) if (!readByID.has(id)) total++;
  }

  // A capped run list cannot prove a repo has nothing active: a repo whose only active run
  // falls past the cap is absent from activeIDs, so the recent ("nothing active") and idle
  // groups are left out rather than guessed.
  let recent: RecentRepo[] | null = null;
  let idle: IdleSummary | null = null;
  if (active.state === "ok" && readOk && !active.atLeast) {
    const found: Array<RecentRepo & { t: number }> = [];
    idle = { count: 0, minDays: null, maxDays: null, never: 0 };
    for (const r of readRepos) {
      if (activeIDs.has(r.id)) continue;
      const t = parseMs(r.last_run_at);
      if (r.last_run_at == null) {
        idle.never++;
      } else if (t !== null && now - t < RECENT_MS) {
        const path = displayPath(r.path_with_namespace, demo);
        found.push({ key: r.id, path, badge: badgeOf(path), ago: formatAgo(r.last_run_at, now), t });
      } else {
        idle.count++;
        if (t !== null) {
          const days = Math.floor((now - t) / DAY_MS);
          idle.minDays = idle.minDays === null ? days : Math.min(idle.minDays, days);
          idle.maxDays = idle.maxDays === null ? days : Math.max(idle.maxDays, days);
        }
      }
    }
    found.sort((a, b) => b.t - a.t);
    recent = found.map(({ key, path, badge, ago }) => ({ key, path, badge, ago }));
  }

  return { active, total, recent, idle, listFailed: repoActivity.failed };
}

// The idle group's one line, or null when no repo is idle or never ran.
export function repoIdleText(idle: IdleSummary): string | null {
  const never = idle.never > 0 ? `${idle.never} never ran` : null;
  if (idle.count === 0) return never;
  const days = (n: number) => `${n} ${n === 1 ? "day" : "days"}`;
  let seen: string;
  if (idle.minDays === null || idle.maxDays === null) seen = "last ran more than 7 days ago";
  else if (idle.minDays === idle.maxDays) seen = `last ran ${days(idle.minDays)} ago`;
  else seen = `last ran ${idle.minDays} to ${days(idle.maxDays)} ago`;
  const base = `${idle.count} ${seen}`;
  return never ? `${base}, ${never}` : base;
}
