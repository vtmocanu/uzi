// Pure model for the Overview preview's People chip (admins, issue #4042). Nothing here
// fetches or renders. The roster is a "who is doing what" read over the admin run list, the
// user list and the usage aggregate; every source can fail on its own, so each part of the
// model degrades independently instead of guessing.

import type { AdminUsage, RunSummaryItem, User } from "./api";
import { isTerminalRun } from "./api";
import { maskEmail, maskName } from "./demoMask";
import { overviewCounts, parkDisposition } from "./overview";
import { formatAgo } from "./rateLimits";
import { isAccountCooldownWaiting, isRevisingRun } from "./runBadge";
import { stripUnsafeChars } from "./safeText";
import type { AdminRunsState } from "./useOverviewData";

// The admin run list is capped server-side at this many rows; a list this long may be
// missing active runs, so the count is shown as "at least".
export const ADMIN_RUNS_CAP = 500;

const DAY_MS = 24 * 60 * 60 * 1000;

export type RunPart = "running" | "queued" | "waiting" | "owner";

export interface PillParts {
  running: number;
  queued: number;
  waiting: number;
  owner: number;
}

// What a non-terminal run is doing for its owner's pill. Account cooldown wins first (the
// run is holding, not executing). A status this table does not know (a future one) falls
// to "owner": a new state surfaces as "waits on them" rather than vanishing from the pill.
export function runPart(run: RunSummaryItem): RunPart | null {
  if (isTerminalRun(run.status)) return null;
  if (isAccountCooldownWaiting(run)) return "waiting";
  switch (run.status) {
    case "running":
      return "running";
    case "awaiting_approval":
      return isRevisingRun(run) ? "running" : "owner";
    case "queued":
    case "claimed":
      return "queued";
    case "awaiting_input":
    case "awaiting_followup":
      return "owner";
    case "limit_wait":
    case "pool_wait":
    case "recovery_wait":
    case "paused":
      return parkDisposition(run) === "owner" ? "owner" : "waiting";
    default:
      return "owner";
  }
}

export function pillText(parts: PillParts): string {
  const out: string[] = [];
  if (parts.running > 0) out.push(`${parts.running} running`);
  if (parts.queued > 0) out.push(`${parts.queued} queued`);
  if (parts.waiting > 0) out.push(`${parts.waiting} waiting`);
  if (parts.owner > 0) out.push("waits on them");
  return out.join(" · ");
}

export function pillTone(parts: PillParts): "warn" | "ok" | "neutral" {
  if (parts.owner > 0) return "warn";
  return parts.running > 0 ? "ok" : "neutral";
}

export interface PeopleRow {
  run: RunSummaryItem;
  owner: string;
}

// Every personal row (owned by the viewer) plus every admin row not already present. The
// personal list is capped at the 200 newest runs, so an admin row the viewer owns is still
// kept when the personal list lacks it. Personal rows win on a duplicate id.
export function peopleRows(
  ownRuns: RunSummaryItem[],
  adminRuns: RunSummaryItem[],
  viewerEmail: string,
): PeopleRow[] {
  const seen = new Set(ownRuns.map((r) => r.id));
  const rows: PeopleRow[] = ownRuns.map((run) => ({ run, owner: viewerEmail }));
  for (const run of adminRuns) {
    if (seen.has(run.id)) continue;
    seen.add(run.id);
    rows.push({ run, owner: run.owner_email ?? "" });
  }
  return rows;
}

// Distinct owners with a run the status sentence counts as executing. Feed it the SAME rows
// the sentence counts (mergeOverviewRows output), never peopleRows, so "for N people" can
// never exceed the executing count beside it.
export function executingPeople(rows: ReadonlyArray<RunSummaryItem>, viewerEmail: string): number {
  const owners = new Set<string>();
  for (const r of rows) {
    if (overviewCounts([r]).executing > 0) owners.add(r.owner_email ?? viewerEmail);
  }
  return owners.size;
}

export interface ActivePerson {
  key: string;
  name: string;
  initials: string;
  you: boolean;
  deactivated: boolean;
  parts: PillParts;
  pill: string;
  tone: "warn" | "ok" | "neutral";
}

export interface SignedInPerson {
  key: string;
  name: string;
  initials: string;
  ago: string;
}

export interface IdleSummary {
  count: number;
  minDays: number | null;
  maxDays: number | null;
  never: number;
}

export type ActiveState =
  | { state: "unavailable" }
  | { state: "loading" }
  | { state: "ok"; people: ActivePerson[]; atLeast: boolean; count: number };

export interface PeopleModel {
  active: ActiveState;
  total: number | null;
  signedIn: SignedInPerson[] | null;
  idle: IdleSummary | null;
  week: number | null;
  usageAction: "available" | "unavailable";
}

function displayName(email: string, user: User | undefined, demo: boolean): string {
  const display = user?.display_name?.trim();
  const raw = display ? maskName(display, demo) : maskEmail(email, demo);
  const clean = stripUnsafeChars(raw).trim();
  return clean === "" ? "Unknown" : clean;
}

function initialsOf(name: string): string {
  const letters = (s: string) => s.replace(/[^\p{L}\p{N}]/gu, "");
  const first = (tokens: string[]) =>
    tokens
      .slice(0, 2)
      .map((t) => letters(t).charAt(0))
      .join("");
  const spaced = name.split(/\s+/).filter((t) => t !== "");
  let out: string;
  if (spaced.length >= 2) {
    out = first(spaced);
  } else if (name.includes("@")) {
    out = first(name.split("@")[0].split(/[._+-]/).filter((t) => t !== ""));
  } else {
    out = first(spaced);
  }
  out = out.toUpperCase();
  return out === "" ? "?" : out;
}

function parseMs(iso: string | null): number | null {
  if (iso == null) return null;
  const t = Date.parse(iso);
  return Number.isFinite(t) ? t : null;
}

export function peopleModel(input: {
  ownRuns: RunSummaryItem[];
  adminRuns: AdminRunsState;
  users: User[] | null;
  usersFailed: boolean;
  viewer: { email: string };
  adminUsage: AdminUsage | null | undefined;
  usageTargetAvailable: boolean;
  now: number;
  demo: boolean;
}): PeopleModel {
  const { ownRuns, adminRuns, users, usersFailed, viewer, adminUsage, now, demo } = input;
  const userByEmail = new Map<string, User>();
  for (const u of users ?? []) userByEmail.set(u.email, u);

  let active: ActiveState;
  const ownerKeys = new Set<string>();
  if (adminRuns.failed) {
    active = { state: "unavailable" };
  } else if (adminRuns.runs === null) {
    active = { state: "loading" };
  } else {
    const byOwner = new Map<string, PillParts>();
    for (const { run, owner } of peopleRows(ownRuns, adminRuns.runs, viewer.email)) {
      const part = runPart(run);
      if (part === null) continue;
      let parts = byOwner.get(owner);
      if (!parts) {
        parts = { running: 0, queued: 0, waiting: 0, owner: 0 };
        byOwner.set(owner, parts);
      }
      parts[part]++;
    }
    const people: ActivePerson[] = [];
    for (const [email, parts] of byOwner) {
      ownerKeys.add(email);
      const user = userByEmail.get(email);
      const name = displayName(email, user, demo);
      people.push({
        key: email,
        name,
        initials: initialsOf(name),
        you: email === viewer.email,
        deactivated: user !== undefined && !user.is_active,
        parts,
        pill: pillText(parts),
        tone: pillTone(parts),
      });
    }
    people.sort((a, b) => Number(b.you) - Number(a.you) || a.name.localeCompare(b.name));
    active = {
      state: "ok",
      people,
      atLeast: adminRuns.runs.length >= ADMIN_RUNS_CAP,
      count: people.length,
    };
  }

  const usersOk = users !== null && !usersFailed;
  let total: number | null = null;
  let signedIn: SignedInPerson[] | null = null;
  let idle: IdleSummary | null = null;
  if (usersOk) {
    total = users.filter((u) => u.is_active).length;
    if (active.state === "ok") {
      // Owners who are deactivated or have no user row are still people doing work, so the
      // total includes them and count never exceeds it.
      for (const key of ownerKeys) {
        const u = userByEmail.get(key);
        if (u === undefined || !u.is_active) total++;
      }
      const recent: Array<SignedInPerson & { t: number }> = [];
      idle = { count: 0, minDays: null, maxDays: null, never: 0 };
      for (const u of users) {
        if (!u.is_active || ownerKeys.has(u.email)) continue;
        const t = parseMs(u.last_login);
        if (u.last_login == null) {
          idle.never++;
        } else if (t !== null && now - t < DAY_MS) {
          const name = displayName(u.email, u, demo);
          recent.push({ key: u.email, name, initials: initialsOf(name), ago: formatAgo(u.last_login, now), t });
        } else {
          idle.count++;
          if (t !== null) {
            const days = Math.floor((now - t) / DAY_MS);
            idle.minDays = idle.minDays === null ? days : Math.min(idle.minDays, days);
            idle.maxDays = idle.maxDays === null ? days : Math.max(idle.maxDays, days);
          }
        }
      }
      recent.sort((a, b) => b.t - a.t);
      signedIn = recent.map(({ key, name, initials, ago }) => ({ key, name, initials, ago }));
    }
  }

  let week: number | null = null;
  if (adminUsage != null && adminUsage.users.every((u) => typeof u.last7_run_count === "number")) {
    week = adminUsage.users.filter((u) => (u.last7_run_count ?? 0) > 0).length;
  }

  return {
    active,
    total,
    signedIn,
    idle,
    week,
    usageAction: adminUsage != null && input.usageTargetAvailable ? "available" : "unavailable",
  };
}

// The idle group's one line, or null when nobody is idle or never signed in.
export function idleText(idle: IdleSummary): string | null {
  const never = idle.never > 0 ? `${idle.never} never signed in` : null;
  if (idle.count === 0) return never;
  const days = (n: number) => `${n} ${n === 1 ? "day" : "days"}`;
  const who = `${idle.count} ${idle.count === 1 ? "person" : "people"}`;
  let seen: string;
  if (idle.minDays === null || idle.maxDays === null) seen = "last seen more than a day ago";
  else if (idle.minDays === idle.maxDays) seen = `last seen ${days(idle.minDays)} ago`;
  else seen = `last seen ${idle.minDays} to ${days(idle.maxDays)} ago`;
  const base = `${who}, ${seen}`;
  return never ? `${base} · ${never}` : base;
}
