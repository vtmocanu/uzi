// Non-admin Overview platform line (PRD #1484 M5, D3/D4). Shown to a NON-admin viewer whose
// own hosted fleet cannot start, so a stuck user can tell a platform problem from a problem
// with their own run. It is derived ENTIRELY from the viewer's own listRuns + listWorkers
// (GET /api/workers, the user's OWN workers — never the admin route), which the Dashboard
// already polls. It names no other user and needs no new endpoint (D4: workers are per owner,
// so "the platform cannot run my work" is already visible in the viewer's own worker list).
//
// Admins get the HealthOverviewCard instead — the card and this line are mutually exclusive.

import type { RunListItem, Worker } from "../lib/api";

// Server-authored constant copy (PRD #1484). The image-pull phrasing follows the viewer's own
// upgrade_blocking_* through likelyCause (WorkerUpgradeBadge), which yields the image-pull
// cause for ImagePullBackOff / ErrImagePull — never free text.
const PLATFORM_LINE_COPY =
  "Your hosted workers cannot start right now: the worker image cannot be pulled. This is a platform problem, not your run. Queued runs start on their own once it is fixed.";

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
  return (
    // role="status" (a polite live region), scoped for tests by its own label rather than a
    // bare getByRole("status") — RateLimitAnnouncer owns one too.
    <div
      role="status"
      aria-label="Your hosted workers"
      className="rounded-xl border border-edge-strong bg-raised px-4 py-3 text-sm text-fg"
    >
      {PLATFORM_LINE_COPY}
    </div>
  );
}
