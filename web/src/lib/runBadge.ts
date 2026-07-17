// Pure, framework-free board-card run logic (PRD #12 M3): the badge taxonomy, the
// "cancelled is not failure" heuristic, the start-run gate input, and the run-view
// link gate. Kept out of Board.tsx so the mapping is unit-tested in isolation
// (see runBadge.test.ts), the same split the run stream uses (runStream.ts).

import { isTerminalRun, type LatestRun, type RunHealth, type StopKind } from "./api";
import { forgeNounSentence, forgeNounLower } from "./forgeNoun";

// Tones mirror StatusPill's RUN_STATUS_TONES (ui.tsx) so one status renders one
// color everywhere: queue (queued), neutral (stopped/idle), info
// (claimed/running), warning (awaiting), ok (completed), danger (failure).
export type BadgeTone = "neutral" | "queue" | "warning" | "danger" | "info" | "ok";

// RunBadge is a card's primary status pill. kind "mr" is the completed-with-MR
// chip (rendered as a link to the merge request), carrying the derived MR-state
// variant; kind "badge" is a plain pill.
export type RunBadge =
  | { kind: "mr"; mrIid: number; mrState: MrChipState }
  | { kind: "badge"; label: string; tone: BadgeTone; pulse: boolean; title?: string };

// MrChipState is the MR chip's display variant (PRD #33 Decision 2, multica's
// derived-status pattern): the single enum every chip surface renders from, so raw
// forge state strings never scatter through components.
export type MrChipState = "open" | "merged" | "closed";

// mrChipState collapses a run's frozen mr_state hint into that variant. Display-only:
// only merged/closed get distinct rendering; opened / locked / unknown / NULL are
// "open" — the chip exactly as before this change (success criterion 2). mr_state is
// frozen per run and can be stale on a superseded run (only a stale "closed"
// misleads; "merged" is terminal), so callers scope any freshness claim to the board
// card and every surface's chip title says "as of last sync".
export function mrChipState(mrState: string | null | undefined): MrChipState {
  if (mrState === "merged") return "merged";
  if (mrState === "closed") return "closed";
  return "open";
}

// mrChipSuffix is the state word a chip appends after its "!N" — "" for open (so the
// chip is unchanged, SC2), " merged" / " closed" otherwise. Shared by all five chip
// surfaces so the wording is defined once.
export function mrChipSuffix(state: MrChipState): string {
  return state === "open" ? "" : ` ${state}`;
}

// mrChipTitle is the chip's hover title. The noun is per-forge (PRD #65 D2):
// "Merge request" on GitLab, "Pull request" on Forgejo. merged/closed scope the
// claim to "as of last sync" (Decision 1: mr_state is frozen per run and only
// best-effort fresh). The open title no longer names the platform — the persisted
// URL routes to the right forge and naming "GitLab" would be wrong on Forgejo.
export function mrChipTitle(state: MrChipState, forgeType: string): string {
  const noun = forgeNounSentence(forgeType);
  if (state === "merged") return `${noun} merged (as of last sync)`;
  if (state === "closed") return `${noun} closed unmerged (as of last sync)`;
  return `Open the ${forgeNounLower(forgeType)}`;
}

// isStoppedRun folds the cancelled nuance (PRD §1): a deliberate human stop is not
// breakage, so the board and RunsList style it calm/neutral, never rose. It reads
// the server-stamped stop_kind (PRD #33), so a live-poller plan reject carrying the
// user's VERBATIM reason is recognised too — the exact case the old exact-string
// heuristic could not. It covers status `cancelled` (which the server-side cancel
// path also stamps stop_kind for) and status `failed` carrying a stop_kind.
//
// The `failed` guard is load-bearing: on the live path stop_kind is stamped at
// verdict enqueue, while the run is still awaiting_approval/running, and a
// reject-then-approve race can even COMPLETE a run that carries a stop_kind. So a
// non-terminal or completed run is never styled "stopped" just because a verdict was
// once queued — only a `failed`/`cancelled` run is.
export function isStoppedRun(status: string, stopKind: StopKind | null | undefined): boolean {
  if (status === "cancelled") return true;
  if (status === "failed" && stopKind != null) return true;
  return false;
}

// runStatusTone maps a run status to a list-row badge tone (a simpler view than
// runBadge, which also carries label/pulse/MR-chip). Same taxonomy as StatusPill
// and the board badge so one status is one color everywhere: a deliberate stop is
// calm neutral, awaiting-approval is amber, a genuine failure is rose, completed
// is green, claimed/running are sky. Shared by the issue-view history rows.
export function runStatusTone(status: string, stopKind: StopKind | null | undefined): BadgeTone {
  if (status === "awaiting_approval") return "warning";
  if (isStoppedRun(status, stopKind)) return "neutral";
  if (status === "failed") return "danger";
  if (status === "completed") return "ok";
  if (status === "claimed" || status === "running") return "info";
  if (status === "queued") return "queue";
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

// HEALTH_FLAG_LABELS is the short word each run-health flag shows (PRD #47 Decision
// 10). "ok" is absent — a healthy run keeps its normal status badge.
const HEALTH_FLAG_LABELS: Record<Exclude<RunHealth, "ok">, string> = {
  stalled: "stalled",
  looping: "looping",
  slow: "slow",
  waiting_worker: "waiting for worker",
  approval_idle: "needs approval",
};

// HEALTH_FLAGGABLE_STATUSES mirrors the server's flaggable set: a health flag is
// only ever rendered while a run is in one of these (belt-and-braces with the
// server exit contract, Decision 3 — a terminal run must never show a stale ⚠).
const HEALTH_FLAGGABLE_STATUSES = new Set<string>(["queued", "running", "awaiting_approval"]);

// healthFlagLabel is the short word for a flag, or null for "ok". Shared by the
// board badge and the run-view header so the wording is defined once.
export function healthFlagLabel(health: RunHealth): string | null {
  return health === "ok" ? null : HEALTH_FLAG_LABELS[health];
}

// isHealthFlaggableStatus reports whether a status can carry a rendered health flag.
export function isHealthFlaggableStatus(status: string): boolean {
  return HEALTH_FLAGGABLE_STATUSES.has(status);
}

// shouldShowHealthFlag is the single gate both surfaces use: a non-ok flag on a
// flaggable status.
export function shouldShowHealthFlag(health: RunHealth, status: string): boolean {
  return health !== "ok" && isHealthFlaggableStatus(status);
}

// HealthFlaggable is the minimal run shape the health badge reads — satisfied by
// the board's LatestRun AND the owner-scoped Run / RunListItem, so one taxonomy
// drives every surface (board card, run view, dashboard, runs list).
export type HealthFlaggable = {
  health: RunHealth;
  health_since: string | null;
  health_reason: string | null;
  status: string;
};

// healthBadge is the warn-variant pill for a flagged run (PRD #47): `⚠ <label> ·
// <elapsed-since-flagged>` in the warn tone, keeping the pulse so it still reads as
// live. Returns null when the run is healthy or not in a flaggable status, so the
// caller falls through to the normal status badge. The elapsed counts from
// health_since ("stuck for Xm"), not created_at. title carries the owner-only
// reason when present (a non-owner's health_reason is null → no tooltip).
export function healthBadge(run: HealthFlaggable, nowMs: number): RunBadge | null {
  if (!shouldShowHealthFlag(run.health, run.status)) return null;
  const label = healthFlagLabel(run.health);
  const elapsed = run.health_since ? ` · ${formatElapsed(nowMs - Date.parse(run.health_since))}` : "";
  return {
    kind: "badge",
    label: `⚠ ${label}${elapsed}`,
    tone: "warning",
    pulse: true,
    title: run.health_reason ?? undefined,
  };
}

// runBadge maps a card's latest_run to its primary status pill. nowMs is passed in
// (not read from Date.now) so the running elapsed is deterministic under test.
export function runBadge(run: LatestRun, nowMs: number): RunBadge {
  // The stopped signal wins over the raw status so a cancel-shaped `failed`
  // never renders as breakage.
  if (isStoppedRun(run.status, run.stop_kind)) {
    return { kind: "badge", label: "stopped", tone: "neutral", pulse: false };
  }
  // A health flag overrides the normal status label while the run is alive but looks
  // slow/stuck/looping (PRD #47). Only ever fires for a flaggable status.
  const flagged = healthBadge(run, nowMs);
  if (flagged) return flagged;
  switch (run.status) {
    case "queued":
      return { kind: "badge", label: "queued", tone: "queue", pulse: false };
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
      // A completed run with an MR becomes a link chip (ok-accented), carrying the
      // derived MR-state variant so the board card can show merged/closed; without
      // an MR it must still be visible, so a plain ok "completed" badge stands in.
      if (run.mr_iid != null) return { kind: "mr", mrIid: run.mr_iid, mrState: mrChipState(run.mr_state) };
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
