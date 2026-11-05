// Pure helpers shared by the two Overview pages (Dashboard and the /overview/next
// preview, PRD #3878). Nothing here fetches or renders.

import { isTerminalRun, type RunStatus, type RunSummaryItem, type Worker } from "./api";
import { effectiveRunStatus, isAccountCooldownWaiting, isReserveHeldPause } from "./runBadge";

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

type ParkRun = Parameters<typeof effectiveRunStatus>[0] & { health?: string };

// True when recovery_retry_not_before is a parseable timestamp: the server has scheduled an
// automatic retry. Same test as effectiveRunStatus uses for the *_retrying statuses.
function hasScheduledRetry(run: { recovery_retry_not_before?: string | null }): boolean {
  return (
    typeof run.recovery_retry_not_before === "string" &&
    Number.isFinite(Date.parse(run.recovery_retry_not_before))
  );
}

const PARKED_STATUSES = new Set(["paused", "limit_wait", "pool_wait", "recovery_wait"]);

// A run parked on the raw status, or holding for an account cooldown.
function isParked(run: ParkRun): boolean {
  return PARKED_STATUSES.has(run.status) || isAccountCooldownWaiting(run);
}

// Who moves a parked run: the system on its own, or only the owner. Keyed on the effective
// status, with an explicit table; anything unlisted (a future status) is "owner", so a new
// park never silently hides from the decision card.
const PARK_TABLE: Record<string, "automatic" | "owner"> = {
  limit_wait: "automatic",
  pool_wait: "automatic",
  retained_recovery_retrying: "automatic",
  codex_idle_stall_retrying: "automatic",
  paused: "owner",
  codex_policy_hold: "owner",
  iteration_budget_exhausted: "owner",
  provider_unavailable: "owner",
  credential_switch_settling: "owner",
  credential_switch_unsettled: "owner",
  base_attestation_missing: "owner",
  completion_blocked: "owner",
  credential_disabled: "owner",
  retained_recovery_adoption_failed: "owner",
  retained_recovery_held: "owner",
  retained_recovery_unknown: "owner",
  codex_idle_stall_held: "owner",
  codex_idle_stall_unknown: "owner",
  worker_requeue_exhausted: "owner",
  worker_memory_pressure: "owner",
};

// Documented divergences from the server's needs_owner / progressing tag in
// ListMaintenanceHeldRuns (pinned by overview.serverPin.test.ts):
//   - A plain recovery_wait or provider_outage with no parseable retry stamp is "owner" here
//     (nothing is scheduled, so waiting for it is not safe); the server tags it progressing.
//   - retained_recovery_exhausted / codex_idle_stall with an omitted or invalid stamp
//     (*_unknown) is "owner" here, fail-closed; the server only treats a NULL stamp as owner.
//   - A weekly-reserve pause is "automatic" here (it releases itself); the server tags every
//     paused run needs_owner.
export function parkDisposition(run: ParkRun): "automatic" | "owner" {
  if (isAccountCooldownWaiting(run)) return "automatic";
  if (isReserveHeldPause(run)) return "automatic";
  const eff = effectiveRunStatus(run);
  if (eff === "recovery_wait" || eff === "provider_outage") {
    return hasScheduledRetry(run) ? "automatic" : "owner";
  }
  return PARK_TABLE[eff] ?? "owner";
}

type PrRun = Pick<
  RunSummaryItem,
  "status" | "kind" | "mr_iid" | "mr_state" | "repo_id" | "finished_at" | "created_at"
>;

// The viewer's completed issue runs whose MR is still open, one per (repo, MR): when several
// runs share an MR the newest wins.
export function openPrs<T extends PrRun>(runs: ReadonlyArray<T>): T[] {
  const newest = (r: T) => {
    const t = Date.parse(r.finished_at ?? r.created_at);
    return Number.isNaN(t) ? 0 : t;
  };
  const byMr = new Map<string, T>();
  for (const r of runs) {
    if (r.status !== "completed" || r.kind !== "issue" || r.mr_iid == null || r.mr_state !== "opened") continue;
    const key = `${r.repo_id}:${r.mr_iid}`;
    const prev = byMr.get(key);
    if (!prev || newest(r) > newest(prev)) byMr.set(key, r);
  }
  return [...byMr.values()];
}

export type DecisionKind = "plan" | "question" | "park" | "pr";

export interface DecisionItem {
  kind: DecisionKind;
  run: RunSummaryItem;
  verb: "Review plan" | "Answer" | "Resume" | "Review PR";
  since: string;
}

const DECISION_VERB: Record<DecisionKind, DecisionItem["verb"]> = {
  plan: "Review plan",
  question: "Answer",
  park: "Resume",
  pr: "Review PR",
};

// The viewer's decision items, grouped plan, question, park, pr and oldest first inside a
// group. The groups are disjoint by raw status, so no run-level guard is needed: plan is
// awaiting_approval, question is awaiting_input / awaiting_followup, pr is completed, and
// park is a PARKED_STATUSES status, or an account cooldown, which parkDisposition always
// calls "automatic" and so never lands in the owner-only park group. Callers pass ONLY the
// viewer's own rows (listRuns), never the admin rows.
export function overviewDecisions(ownRuns: ReadonlyArray<RunSummaryItem>): DecisionItem[] {
  const out: DecisionItem[] = [];
  const add = (kind: DecisionKind, runs: ReadonlyArray<RunSummaryItem>, sinceOf: (r: RunSummaryItem) => string) => {
    const items: DecisionItem[] = runs.map((run) => ({ kind, run, verb: DECISION_VERB[kind], since: sinceOf(run) }));
    // An unparseable timestamp sorts last; two of them compare equal (Infinity - Infinity is NaN).
    const t = (i: DecisionItem) => {
      const ms = Date.parse(i.since);
      return Number.isNaN(ms) ? Number.POSITIVE_INFINITY : ms;
    };
    items.sort((a, b) => {
      const ta = t(a);
      const tb = t(b);
      return ta === tb ? 0 : ta < tb ? -1 : 1;
    });
    out.push(...items);
  };
  const since = (r: RunSummaryItem) => r.status_since ?? r.created_at;
  add("plan", ownRuns.filter((r) => r.status === "awaiting_approval" && !r.is_revising), since);
  add("question", ownRuns.filter((r) => r.status === "awaiting_input" || r.status === "awaiting_followup"), since);
  add("park", ownRuns.filter((r) => isParked(r) && parkDisposition(r) === "owner"), since);
  add("pr", openPrs(ownRuns), (r) => r.finished_at ?? r.status_since ?? r.created_at);
  return out;
}

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

// The page header's status sentence. Decisions and pull requests come from the viewer's own
// items only (decisions excludes pull requests; prs is the open-PR count). `acrossFactory` is the admin's factory-wide scope;
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
