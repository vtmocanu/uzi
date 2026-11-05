// Pure helpers shared by the two Overview pages (Dashboard and the /overview/next
// preview, PRD #3878). Nothing here fetches or renders.

import { isTerminalRun, type RunStatus, type RunSummaryItem, type Worker } from "./api";
import { isAccountCooldownWaiting } from "./runBadge";

// Exhaustive over the wire status union; a new status needs an explicit bucket.
export const activeRunBuckets: Record<RunStatus, "running" | "needsYou" | "queued" | "waiting" | null> = {
  running: "running",
  awaiting_approval: "needsYou",
  awaiting_input: "needsYou",
  awaiting_followup: "needsYou",
  queued: "queued",
  claimed: "queued",
  limit_wait: "waiting",
  pool_wait: "waiting",
  recovery_wait: "waiting",
  paused: "waiting",
  completed: null,
  failed: null,
  cancelled: null,
};

export interface OverviewCounts {
  executing: number;
  queued: number;
  parked: number;
}

// Executing, queued and parked counts over the non-terminal runs. Buckets on the RAW
// status, as Dashboard's tile always has, with an account cooldown counted as waiting;
// feeding it effective statuses would drop runs. Runs that need the owner (the
// needsYou bucket) are deliberately not counted here: the decision clause of the
// status sentence covers them.
export function overviewCounts(runs: ReadonlyArray<Pick<RunSummaryItem, "status" | "health">>): OverviewCounts {
  const counts: OverviewCounts = { executing: 0, queued: 0, parked: 0 };
  for (const run of runs) {
    if (isTerminalRun(run.status)) continue;
    const bucket = isAccountCooldownWaiting(run) ? "waiting" : activeRunBuckets[run.status];
    if (bucket === "running") counts.executing++;
    else if (bucket === "queued") counts.queued++;
    else if (bucket === "waiting") counts.parked++;
  }
  return counts;
}

// An admin's row set: the viewer's own runs plus every OTHER user's runs from the admin
// list, deduplicated by run id with the viewer's own row winning (the admin list also
// carries the admin's own runs).
export function mergeOverviewRows(
  own: RunSummaryItem[],
  admin: RunSummaryItem[],
  viewerEmail: string,
): RunSummaryItem[] {
  const seen = new Set(own.map((r) => r.id));
  const merged = [...own];
  for (const r of admin) {
    if (r.owner_email === viewerEmail || seen.has(r.id)) continue;
    seen.add(r.id);
    merged.push(r);
  }
  return merged;
}

// PRD #1349 M6 (D8/D10): the count of the viewer's runs currently self-healing in the
// recovery_wait park, derived client-side from runs the page already polls. Both Overview
// pages feed it the viewer's OWN runs only.
export function ownRecoveryWaitCount(runs: ReadonlyArray<Pick<RunSummaryItem, "status">>): number {
  return runs.filter((r) => r.status === "recovery_wait").length;
}

// Step 4 of the onboarding checklist is a SETUP fact, not live health: a worker counts as
// brought online if it is online now OR has ever registered (last_heartbeat_at is stamped
// at register/heartbeat and never cleared by the offline sweep), so a fleet mid-roll
// (every worker offline + upgrading) does not reopen the step. Live health stays on the
// "Workers online N/M" tile.
export function workerJoined(workers: ReadonlyArray<Pick<Worker, "status" | "last_heartbeat_at">>): boolean {
  return workers.some((w) => w.status === "online" || w.last_heartbeat_at != null);
}

// True once every onboarding step is done (the checklist then hides).
export function setupComplete(
  data: { hasToken: boolean; hasForge: boolean; reposEnabled: number },
  joined: boolean,
): boolean {
  return data.hasToken && data.hasForge && data.reposEnabled > 0 && joined;
}

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

// The page header's status sentence. Decisions and pull requests come from the viewer's own
// items only (M2 wires them; M1 passes 0). `acrossFactory` is the admin's factory-wide scope;
// when the admin list is unavailable the sentence counts only the viewer's runs, says so,
// and never claims the factory.
export function statusSentence(input: {
  decisions: number;
  prs: number;
  counts: OverviewCounts;
  acrossFactory: boolean;
  adminUnavailable: boolean;
}): string {
  const { decisions, prs, counts, adminUnavailable } = input;
  const across = input.acrossFactory && !adminUnavailable;

  const subject = [
    decisions > 0 ? plural(decisions, "decision", "decisions") : null,
    prs > 0 ? plural(prs, "pull request", "pull requests") : null,
  ].filter((s): s is string => s !== null);
  const waiting =
    subject.length === 0
      ? "Nothing needs you right now."
      : `${subject.join(" and ")} ${decisions + prs === 1 ? "is" : "are"} waiting on you.`;

  const rest = [
    counts.queued > 0 ? `${counts.queued} queued` : null,
    counts.parked > 0 ? `${counts.parked} parked` : null,
  ].filter((s): s is string => s !== null);
  const runs =
    `${counts.executing} ${counts.executing === 1 ? "run is" : "runs are"} executing` +
    (across ? " across the factory" : "") +
    (rest.length === 2 ? `, ${rest[0]} and ${rest[1]}` : rest.length === 1 ? ` and ${rest[0]}` : "") +
    ".";

  return `${waiting} ${runs}${adminUnavailable ? " Counting only your runs." : ""}`;
}
