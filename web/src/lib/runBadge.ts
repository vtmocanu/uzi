// Pure, framework-free board-card run logic (PRD #12 M3): the badge taxonomy, the
// "cancelled is not failure" heuristic, the start-run gate input, and the run-view
// link gate. Kept out of Board.tsx so the mapping is unit-tested in isolation
// (see runBadge.test.ts), the same split the run stream uses (runStream.ts).

import { isTerminalRun, type LatestRun } from "./api";

export type BadgeTone = "neutral" | "warning" | "danger";

// RunBadge is a card's primary status pill. kind "mr" is the completed-with-MR
// chip (rendered as a link to the merge request); kind "badge" is a plain pill.
export type RunBadge =
  | { kind: "mr"; mrIid: number }
  | { kind: "badge"; label: string; tone: BadgeTone; pulse: boolean; title?: string };

// isStoppedRun folds the cancelled nuance (PRD §1): a live-poller cancel reaches
// the server as `failed` with a "run cancelled" reason, while the no-poller branch
// yields a true `cancelled` status. Both are deliberate human stops — never
// breakage — so the board and RunsList style them calm/neutral, never rose.
export function isStoppedRun(status: string, failureReason: string | null | undefined): boolean {
  if (status === "cancelled") return true;
  if (status === "failed" && failureReason != null && /cancel/i.test(failureReason)) return true;
  return false;
}

// formatElapsed renders a coarse duration for the running badge ("running 4m").
export function formatElapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

// runBadge maps a card's latest_run to its primary status pill. nowMs is passed in
// (not read from Date.now) so the running elapsed is deterministic under test.
export function runBadge(run: LatestRun, nowMs: number): RunBadge {
  // The stopped heuristic wins over the raw status so a cancel-shaped `failed`
  // never renders as breakage.
  if (isStoppedRun(run.status, run.failure_reason)) {
    return { kind: "badge", label: "stopped", tone: "neutral", pulse: false };
  }
  switch (run.status) {
    case "queued":
      return { kind: "badge", label: "queued", tone: "neutral", pulse: false };
    case "claimed":
      return { kind: "badge", label: "claimed", tone: "neutral", pulse: false };
    case "running": {
      const elapsed = formatElapsed(nowMs - Date.parse(run.created_at));
      return { kind: "badge", label: `running ${elapsed}`, tone: "neutral", pulse: true };
    }
    case "awaiting_approval":
      return { kind: "badge", label: "awaiting approval", tone: "warning", pulse: false };
    case "failed":
      return {
        kind: "badge",
        label: "failed",
        tone: "danger",
        pulse: false,
        title: run.failure_reason ?? undefined,
      };
    case "completed":
      // A completed run with an MR becomes a link chip; without one it must still
      // be visible, so a plain "completed" badge stands in (never invisible).
      if (run.mr_iid != null) return { kind: "mr", mrIid: run.mr_iid };
      return { kind: "badge", label: "completed", tone: "neutral", pulse: false };
    default:
      return { kind: "badge", label: run.status.replace(/_/g, " "), tone: "neutral", pulse: false };
  }
}

// hasActiveRun reports whether a card's latest run is still non-terminal. The
// start-run gate reads this instead of a separate listRuns fan-in (PRD §2).
export function hasActiveRun(run: LatestRun | null | undefined): boolean {
  return run != null && !isTerminalRun(run.status);
}

// canOpenRunView gates the in-app run-view link: only the owner may open a run (a
// non-owner would 403 on GetRunByIDForUser), so the link renders only when the
// server flagged the latest run is_mine.
export function canOpenRunView(run: LatestRun | null | undefined): boolean {
  return run != null && run.is_mine;
}

// retryHint is the "×N" suffix shown when an issue has run more than once.
export function retryHint(runCount: number): string | null {
  return runCount > 1 ? `×${runCount}` : null;
}

// isAwaitingApproval flags a run that should raise the board's attention strip —
// a human is the blocker while a worker is held busy.
export function isAwaitingApproval(status: string): boolean {
  return status === "awaiting_approval";
}
