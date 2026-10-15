// Non-admin Overview platform line (PRD #1484 M5, D3/D4). Shown to a NON-admin viewer whose
// own hosted fleet cannot start, so a stuck user can tell a platform problem from a problem
// with their own run. It is derived ENTIRELY from the viewer's own listRuns + listWorkers
// (GET /api/workers, the user's OWN workers — never the admin route), which the Dashboard
// already polls. It names no other user and needs no new endpoint (D4: workers are per owner,
// so "the platform cannot run my work" is already visible in the viewer's own worker list).
//
// Admins get the HealthOverviewCard instead — the card and this line are mutually exclusive.

import type { RunSummaryItem, Worker } from "../lib/api";
import { workerFailed, workerFailureCause } from "./WorkerUpgradeBadge";

// The line is a fixed frame + reassurance tail (server-authored, PRD #1484), with the CAUSE
// clause between them DERIVED — not hard-coded. The cause follows the viewer's own
// upgrade_blocking_kind / upgrade_blocking_container / upgrade_blocking_reason through
// workerFailureCause (WorkerUpgradeBadge), the closed lookup the Workers page also uses.
// Scheduler capacity guidance and container failures remain distinct. Only fixed copy is
// interpolated, never free-form diagnostic fields.
const LINE_FRAME = "Your hosted workers cannot start right now.";
const LINE_TAIL =
  "This is a platform problem, not your run. Queued runs start on their own once it is fixed.";
// The fallback cause when likelyCause has no grounded reason (returns null): a generic,
// server-authored clause, still never free text.
const GENERIC_CAUSE = "A hosted worker cannot start.";

// The three conditions, all read from the viewer's own runs and workers: NOT admin, at least
// one queued run, at least one hosted worker they own, and EVERY hosted worker they own reads
// a failed roll, provisioning failure or capacity block. Any condition failing hides it.
function platformLineVisible(isAdmin: boolean, runs: RunSummaryItem[], workers: Worker[]): boolean {
  if (isAdmin) return false;
  const hasQueuedRun = runs.some((r) => r.status === "queued");
  const hosted = workers.filter((w) => w.kind === "hosted");
  const everyHostedFailed = hosted.length > 0 && hosted.every(workerFailed);
  return hasQueuedRun && everyHostedFailed;
}

// Include each distinct cause in a mixed failed fleet, preserving both capacity guidance
// and actual container/storage failures rather than choosing the first worker.
function platformCause(workers: Worker[]): string {
  const causes = workers.filter((w) => w.kind === "hosted" && workerFailed(w)).map((w) => {
    const label = w.upgrade_status === "provisioning_failed" ? "Provisioning failed. " : "";
    return label + (workerFailureCause(w) ?? GENERIC_CAUSE);
  });
  return [...new Set(causes)].join(" ");
}

export function HealthPlatformLine({
  isAdmin,
  runs,
  workers,
}: {
  isAdmin: boolean;
  runs: RunSummaryItem[];
  workers: Worker[];
}) {
  if (!platformLineVisible(isAdmin, runs, workers)) return null;
  const copy = `${LINE_FRAME} ${platformCause(workers)} ${LINE_TAIL}`;
  return (
    // role="status" (a polite live region), scoped for tests by its own label rather than a
    // bare getByRole("status") — RateLimitAnnouncer owns one too.
    <div
      role="status"
      aria-label="Your hosted workers"
      className="rounded-xl border border-edge-strong bg-raised px-4 py-3 text-sm text-fg"
    >
      {copy}
    </div>
  );
}
