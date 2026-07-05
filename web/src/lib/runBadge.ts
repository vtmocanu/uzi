// Pure, framework-free board-card run logic (PRD #12 M3): the badge taxonomy, the
// "cancelled is not failure" heuristic, the start-run gate input, and the run-view
// link gate. Kept out of Board.tsx so the mapping is unit-tested in isolation
// (see runBadge.test.ts), the same split the run stream uses (runStream.ts).

import { isTerminalRun, type LatestRun } from "./api";

// Tones mirror StatusPill's RUN_STATUS_TONES (ui.tsx) so one status renders one
// color everywhere: neutral (queued/stopped), info (claimed/running), warning
// (awaiting), ok (completed), danger (failure).
export type BadgeTone = "neutral" | "warning" | "danger" | "info" | "ok";

// RunBadge is a card's primary status pill. kind "mr" is the completed-with-MR
// chip (rendered as a link to the merge request); kind "badge" is a plain pill.
export type RunBadge =
  | { kind: "mr"; mrIid: number }
  | { kind: "badge"; label: string; tone: BadgeTone; pulse: boolean; title?: string };

// STOPPED_FAILURE_REASONS are the exact failure_reason strings the SERVER writes
// for a deliberate stop that surfaces as status `failed`: a live-poller cancel
// ("run cancelled" — agent/src/executor.ts, sdk-executor.ts) and a server-side
// plan rejection ("plan rejected" — workersvc SubmitInput reject branch). Matched
// exactly, not by substring, so an arbitrary agent error that merely contains the
// word "cancel" is NOT mistaken for a deliberate stop.
const STOPPED_FAILURE_REASONS = new Set(["run cancelled", "plan rejected"]);

// isStoppedRun folds the cancelled nuance (PRD §1): a deliberate human stop is not
// breakage, so the board and RunsList style it calm/neutral, never rose. It covers
// status `cancelled` (server-side no-poller cancel) and status `failed` carrying
// one of the server's known stop reasons above.
//
// Known limitation (same class as the PRD's "known soft heuristic" note): a
// live-poller plan rejection carries the user's VERBATIM reject reason
// (agent/src/steering.ts), which no client-side match can recognize — it stays
// rose "failed". Only an empty-reason reject falls back to the literal "plan
// rejected" and is caught here.
export function isStoppedRun(status: string, failureReason: string | null | undefined): boolean {
  if (status === "cancelled") return true;
  if (status === "failed" && failureReason != null && STOPPED_FAILURE_REASONS.has(failureReason)) return true;
  return false;
}

// runStatusTone maps a run status to a list-row badge tone (a simpler view than
// runBadge, which also carries label/pulse/MR-chip). Same taxonomy as StatusPill
// and the board badge so one status is one color everywhere: a deliberate stop is
// calm neutral, awaiting-approval is amber, a genuine failure is rose, completed
// is green, claimed/running are sky. Shared by the issue-view history rows.
export function runStatusTone(status: string, failureReason: string | null | undefined): BadgeTone {
  if (status === "awaiting_approval") return "warning";
  if (isStoppedRun(status, failureReason)) return "neutral";
  if (status === "failed") return "danger";
  if (status === "completed") return "ok";
  if (status === "claimed" || status === "running") return "info";
  return "neutral";
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
      return { kind: "badge", label: "claimed", tone: "info", pulse: false };
    case "running": {
      const elapsed = formatElapsed(nowMs - Date.parse(run.created_at));
      return { kind: "badge", label: `running ${elapsed}`, tone: "info", pulse: true };
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
      // A completed run with an MR becomes a link chip (ok-accented); without one
      // it must still be visible, so a plain ok "completed" badge stands in.
      if (run.mr_iid != null) return { kind: "mr", mrIid: run.mr_iid };
      return { kind: "badge", label: "completed", tone: "ok", pulse: false };
    default:
      return { kind: "badge", label: run.status.replace(/_/g, " "), tone: "neutral", pulse: false };
  }
}

// hasActiveRun reports whether a card's latest run is still non-terminal. The
// start-run gate reads this instead of a separate listRuns fan-in (PRD §2).
export function hasActiveRun(run: LatestRun | null | undefined): boolean {
  return run != null && !isTerminalRun(run.status);
}

// activeRunInHistory is the issue view's start-run gate input (PRD §3): the board
// reads a card's single latest_run, but the issue view already has full run
// history, so it checks whether any run in that history is still non-terminal.
export function activeRunInHistory(runs: { status: string }[]): boolean {
  return runs.some((r) => !isTerminalRun(r.status));
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
