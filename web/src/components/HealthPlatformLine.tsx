// Non-admin Overview platform line (PRD #1484 M5, D3/D4). Shown to a NON-admin viewer whose
// own hosted fleet cannot start, so a stuck user can tell a platform problem from a problem
// with their own run. It is derived ENTIRELY from the viewer's own listRuns + listWorkers
// (GET /api/workers, the user's OWN workers — never the admin route), which the Dashboard
// already polls. It names no other user and needs no new endpoint (D4: workers are per owner,
// so "the platform cannot run my work" is already visible in the viewer's own worker list).
//
// Admins get the HealthOverviewCard instead — the card and this line are mutually exclusive.

import type { RunListItem, Worker } from "../lib/api";
import { likelyCause } from "./WorkerUpgradeBadge";

// The line is a fixed frame + reassurance tail (server-authored, PRD #1484), with the CAUSE
// clause between them DERIVED — not hard-coded. The cause follows the viewer's own
// upgrade_blocking_container / upgrade_blocking_reason / upgrade_last_exit_code through
// likelyCause (WorkerUpgradeBadge), the closed reason→text lookup the Workers page already
// uses. So an ImagePullBackOff reads the image-pull cause, while a non-image-pull failure (a
// seed-nix CrashLoopBackOff, say) reads ITS own cause instead of a wrong image-pull string.
// likelyCause returns a full server-authored sentence (or null); nothing untrusted is ever
// interpolated — only the closed likelyCause output and these fixed frames.
const LINE_FRAME = "Your hosted workers cannot start right now.";
const LINE_TAIL =
  "This is a platform problem, not your run. Queued runs start on their own once it is fixed.";
// The fallback cause when likelyCause has no grounded reason (returns null): a generic,
// server-authored clause, still never free text.
const GENERIC_CAUSE = "The worker image cannot start.";

// The three conditions, all read from the viewer's own runs and workers: NOT admin, at least
// one queued run, at least one hosted worker they own, and EVERY hosted worker they own reads
// upgrade_failed. Any one failing hides the line.
function platformLineVisible(isAdmin: boolean, runs: RunListItem[], workers: Worker[]): boolean {
  if (isAdmin) return false;
  const hasQueuedRun = runs.some((r) => r.status === "queued");
  const hosted = workers.filter((w) => w.kind === "hosted");
  const everyHostedFailed = hosted.length > 0 && hosted.every((w) => w.upgrade_status === "upgrade_failed");
  return hasQueuedRun && everyHostedFailed;
}

// The cause clause, derived from a representative failed hosted worker the viewer owns (the
// visibility gate guarantees every hosted worker they own reads upgrade_failed, so the first
// one is representative). likelyCause is the closed lookup; null falls back to GENERIC_CAUSE.
function platformCause(workers: Worker[]): string {
  const rep = workers.find((w) => w.kind === "hosted" && w.upgrade_status === "upgrade_failed");
  const cause = rep
    ? likelyCause(rep.upgrade_blocking_container, rep.upgrade_blocking_reason, rep.upgrade_last_exit_code)
    : null;
  return cause ?? GENERIC_CAUSE;
}

export function HealthPlatformLine({
  isAdmin,
  runs,
  workers,
}: {
  isAdmin: boolean;
  runs: RunListItem[];
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
