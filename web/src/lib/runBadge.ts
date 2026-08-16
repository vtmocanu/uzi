// Pure, framework-free board-card run logic (PRD #12 M3): the badge taxonomy, the
// "cancelled is not failure" heuristic, the start-run gate input, and the run-view
// link gate. Kept out of Board.tsx so the mapping is unit-tested in isolation
// (see runBadge.test.ts), the same split the run stream uses (runStream.ts).

import {
  isTerminalRun,
  type LatestRun,
  type Milestone,
  type RunHealth,
  type StopKind,
} from "./api";
import { forgeNounSentence, forgeNounLower, forgePlatform } from "./forgeNoun";
import { stripUnsafeChars } from "./safeText";

// Tones mirror StatusPill's RUN_STATUS_TONES (ui.tsx) so one status renders one
// color everywhere: queue (queued), neutral (stopped/idle), info
// (claimed/running), warning (awaiting, limit_wait), ok (completed), danger
// (failure). Since PRD #35 that mirror is asserted by a test rather than only
// claimed here — see the RUN_STATUS_TONES agreement case in runBadge.test.ts.
export type BadgeTone =
  "neutral" | "queue" | "warning" | "danger" | "info" | "ok" | "plan";

// RunBadge is a card's primary status pill. kind "mr" is the completed-with-MR
// chip (rendered as a link to the merge request), carrying the derived MR-state
// variant; kind "badge" is a plain pill.
export type RunBadge =
  | { kind: "mr"; mrIid: number; mrState: MrChipState }
  | {
      kind: "badge";
      label: string;
      tone: BadgeTone;
      pulse: boolean;
      title?: string;
    };

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

// mrChipTitle is the chip's hover title. Both the noun and the destination platform
// are per-forge (PRD #65 D2): "Merge request … on GitLab" vs "Pull request … on
// Forgejo", so the open title keeps naming the platform (a GitLab card reads exactly
// as it did pre-#65) without ever saying "GitLab" on a Forgejo card. merged/closed
// scope the claim to "as of last sync" (Decision 1: mr_state is frozen per run and
// only best-effort fresh).
export function mrChipTitle(state: MrChipState, forgeType: string): string {
  const noun = forgeNounSentence(forgeType);
  if (state === "merged") return `${noun} merged (as of last sync)`;
  if (state === "closed") return `${noun} closed unmerged (as of last sync)`;
  return `Open the ${forgeNounLower(forgeType)} on ${forgePlatform(forgeType)}`;
}

// isStoppedRun folds the cancelled nuance (PRD §1): a deliberate human stop is not
// breakage, so the board and RunsList style it calm/neutral, never rose. It reads
// the server-stamped stop_kind (PRD #33), so a live-poller plan reject carrying the
// user's VERBATIM reason is recognised too — the exact case the old exact-string
// heuristic could not. It covers status `cancelled` (which the server-side cancel
// path also stamps stop_kind for) and status `failed` carrying a HUMAN stop_kind.
//
// "human" is load-bearing since PRD #108 M5 added `auto_stopped` — see
// HUMAN_STOP_KINDS below.
//
// The `failed` guard is load-bearing: on the live path stop_kind is stamped at
// verdict enqueue, while the run is still awaiting_approval/running, and a
// reject-then-approve race can even COMPLETE a run that carries a stop_kind. So a
// non-terminal or completed run is never styled "stopped" just because a verdict was
// once queued — only a `failed`/`cancelled` run is.
// HUMAN_STOP_KINDS is the set isStoppedRun's calm styling actually means (PRD #108
// M5). Enumerated rather than null-tested BECAUSE a null test silently absorbed
// "auto_stopped": that run is `failed` with a non-null stop_kind, so before this
// change it rendered as a calm neutral "stopped" pill everywhere and dropped out of
// favicon.ts's attention set — the exact opposite of what an auto-stop means.
//
// An auto-stop is uzi killing a run because it was broken. That is breakage and its
// owner needs to see it. Listing the members means the NEXT stop_kind added has to
// make this choice deliberately instead of inheriting it.
const HUMAN_STOP_KINDS: ReadonlySet<StopKind> = new Set<StopKind>([
  "cancelled",
  "plan_rejected",
]);

export function isStoppedRun(
  status: string,
  stopKind: StopKind | null | undefined,
): boolean {
  if (status === "cancelled") return true;
  if (status === "failed" && stopKind != null && HUMAN_STOP_KINDS.has(stopKind))
    return true;
  return false;
}

// runStatusTone maps a run status to a list-row badge tone (a simpler view than
// runBadge, which also carries label/pulse/MR-chip). Same taxonomy as StatusPill
// and the board badge so one status is one color everywhere: a deliberate stop is
// calm neutral, awaiting-approval is amber, a genuine failure is rose, completed
// is green, claimed/running are sky. Shared by the issue-view history rows.
export function runStatusTone(
  status: string,
  stopKind: StopKind | null | undefined,
): BadgeTone {
  if (status === "awaiting_approval" || status === "awaiting_input")
    return "warning";
  // PRD #35: a run parked on the owner's Anthropic usage limit. Warn, like the
  // other "blocked on something outside the run" state — never danger: it has not
  // failed and it resumes by itself.
  if (status === "limit_wait") return "warning";
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
//
// 🔴 `limit_wait` IS DELIBERATELY ABSENT AND MUST STAY ABSENT (PRD #35 §7.6). It
// looks like an omission because a parked run is non-terminal, which is exactly
// why this note is here. The server's ListActiveRunsForHealth is the same positive
// allowlist, so nothing ever re-examines a parked run; the fix for that is the park
// query CLEARING the health columns on entry, not making a park flaggable. The
// detector's three signals (stalled / looping / slow) all describe a *running*
// agent, and a run that is waiting on a clock by design would trip every one of
// them. Adding the status here would put "⚠ stalled" on a run that is behaving
// exactly as intended — the false alarm this design exists to avoid.
const HEALTH_FLAGGABLE_STATUSES = new Set<string>([
  "queued",
  "running",
  "awaiting_approval",
]);

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
export function shouldShowHealthFlag(
  health: RunHealth,
  status: string,
): boolean {
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
export function healthBadge(
  run: HealthFlaggable,
  nowMs: number,
): RunBadge | null {
  if (!shouldShowHealthFlag(run.health, run.status)) return null;
  const label = healthFlagLabel(run.health);
  const elapsed = run.health_since
    ? ` · ${formatElapsed(nowMs - Date.parse(run.health_since))}`
    : "";
  return {
    kind: "badge",
    label: `⚠ ${label}${elapsed}`,
    tone: "warning",
    pulse: true,
    title: badgeTitle(run.health_reason),
  };
}

/**
 * Issue #124 for the badge TOOLTIP, which is an ATTRIBUTE — and a whole-subtree
 * `container.textContent` assertion cannot see attribute values. That is why this class keeps
 * recurring: text is covered INCIDENTALLY by any subtree assertion, attributes only where
 * someone wrote an explicit check (`Judge.test.tsx` via getByLabelText,
 * `WorkerUpgradeBadge.test.tsx` via `span[title]` — so coverage exists, it is just never free).
 *
 * TWO renderers consume this descriptor, not five. The five `title={…}` badge sites share a
 * `{label, tone, title}` SHAPE, not a constructor:
 *
 *   Board.tsx          <- runBadge()        here            covered
 *   RunHealthBadge.tsx <- healthBadge()     here            covered
 *   JudgeRunBadge.tsx  <- judgeBadge()      judgeBadge.ts   not reached by this fix
 *   WorkerRunBadge.tsx <- workerRunBadge()  workerRuns.ts   not reached by this fix
 *   CIFixRunHeader.tsx <- fixVerdictChip()  fixVerdict.ts   not reached by this fix
 *
 * The three others need nothing TODAY, checked rather than assumed: `judgeBadge` is a closed
 * verdict lookup plus a count; `workerRunBadge` has TWO arms, a template over two numbers and
 * the literal "Holds an active run"; `fixVerdictChip` is four literals. But they are
 * where the next untrusted interpolation would land without a strip, and composing here
 * cannot reach them — so this placement prevents drift between the two constructors it owns,
 * NOT across all five. (An earlier version of this comment claimed the opposite; the drift it
 * said it was preventing is the drift that already exists.)
 *
 * Stripped at the descriptor rather than at the two renderers because this function's output
 * is display-only by construction — never posted back, never a key — so it is still a display
 * transform and not a boundary one.
 *
 * Covers BOTH title sources, which is why the fix does not depend on settling their
 * provenance separately:
 *   - `failure_reason` is worker-supplied and ingest does not strip it
 *     (workersvc `sanitizeFailureReason` is stripNUL + truncate). `service.go` writes
 *     `err.Error()` straight in, and a hostile repo can shape the error an agent fails
 *     with — attacker-influenceable, owner-only audience.
 *   - `health_reason` is server-composed as far as was traced, and NOT exhaustively so.
 *     The CLI already routes it through `sanitizeTTY` (cmd/uzi/run.go), so the web was the
 *     inconsistent side of a decision the CLI had already made.
 */
function badgeTitle(reason: string | null | undefined): string | undefined {
  const clean = reason ? stripUnsafeChars(reason) : "";
  return clean === "" ? undefined : clean;
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
    case "running":
      // The running elapsed moved OUT of the badge to the uniform per-card duration
      // token (issue #256 M4, Decision 4) — the board now renders `running <elapsed>`
      // via runDurationLabel, so a bare "running" badge here avoids doubling it.
      return { kind: "badge", label: "running", tone: "info", pulse: true };
    case "awaiting_approval":
      return {
        kind: "badge",
        label: "awaiting approval",
        tone: "warning",
        pulse: false,
      };
    // PRD #88. The default arm below would already produce a warning-toned "awaiting
    // input" via runStatusTone… except it does not: the default hardcodes `neutral`.
    // So this case is what stops a parked question rendering as a calm grey chip
    // indistinguishable from `cancelled`. The label says "answer" rather than echoing
    // the enum, because "awaiting input" reads as machine waiting for machine.
    case "awaiting_input":
      return {
        kind: "badge",
        label: "needs your answer",
        tone: "warning",
        pulse: false,
      };
    // PRD #35: parked on the owner's Anthropic usage limit. STATIC — no countdown
    // and no elapsed, unlike the running badge above. LatestRun is a deliberately
    // narrow board projection that carries neither retry_not_before nor
    // limit_resets_at, so the only honest thing this surface can say is THAT the run
    // is waiting; WHEN it resumes lives on the run view, which reads the full Run.
    // (Widening LatestRun to put a countdown on the card would also force a
    // Board.tsx prop change, which PRD #102 is rewriting in parallel.) The title is
    // therefore a fixed sentence, not an interpolation.
    case "limit_wait":
      return {
        kind: "badge",
        label: "limit wait",
        tone: "warning",
        pulse: false,
        title:
          "Paused on an Anthropic usage limit. It resumes on its own when the window reopens.",
      };
    case "failed":
      return {
        kind: "badge",
        label: "failed",
        tone: "danger",
        pulse: false,
        title: badgeTitle(run.failure_reason),
      };
    case "completed":
      // A completed run with an MR becomes a link chip (ok-accented), carrying the
      // derived MR-state variant so the board card can show merged/closed; without
      // an MR it must still be visible, so a plain ok "completed" badge stands in.
      if (run.mr_iid != null)
        return {
          kind: "mr",
          mrIid: run.mr_iid,
          mrState: mrChipState(run.mr_state),
        };
      return { kind: "badge", label: "completed", tone: "ok", pulse: false };
    default:
      return {
        kind: "badge",
        label: run.status.replace(/_/g, " "),
        tone: "neutral",
        pulse: false,
      };
  }
}

// MilestoneProgress is the compact done/total the milestone badge renders (PRD #122).
// PRD #265 M2: `reported` distinguishes "the lead reported no completion" (the
// milestones_completed column is null — nothing was reported mid-run and nothing was
// declared at completion) from a genuine "0 complete" (an empty-or-populated set was
// reported). A done run that simply never reported must not read as failure, so a null
// tracker renders as "not reported" (neutral), never a `0/N`.
export type MilestoneProgress = { done: number; total: number; reported: boolean };

// MilestoneCounted is the minimal run shape milestoneBadge reads: the FROZEN approved
// list (the denominator) and the ids reported complete. Satisfied by Run / RunListItem.
export type MilestoneCounted = {
  milestones?: Milestone[] | null;
  milestones_completed?: string[] | null;
};

// milestoneBadge folds a run into its `M{done}/{total}` badge input, or null when the
// run carries no frozen milestone list — the caller then falls back to the iteration
// badge, so a pre-#122 (or non-milestone) run renders exactly as before.
//
// `done` counts only completed ids that are MEMBERS of the frozen list. milestones_completed
// is a monotone union and can name an id no longer in the approved set (a milestone dropped
// after it was ticked); counting those would let the badge read e.g. M8/7. Clamping to
// frozen membership keeps the badge honest — done never exceeds total.
export function milestoneBadge(run: MilestoneCounted): MilestoneProgress | null {
  const frozen = run.milestones ?? [];
  if (frozen.length === 0) return null;
  // Count by iterating the FROZEN list and testing membership in the completed set,
  // NOT by reducing over milestones_completed. Both clamp to frozen membership, but
  // iterating frozen also makes `done` immune to a duplicate id in the completed set
  // (which would otherwise let the badge read M8/7): each frozen id is counted at most
  // once by construction, matching MilestoneChecklist's counter (its single source).
  // PRD #265 M2: null (or absent) ⇒ nothing was ever reported; an array (even []) ⇒ a
  // real reported count. `?? []` still drives the `done` tally, but `reported` keeps the
  // null-vs-[] distinction the wire already carries (apitypes nil → JSON null) instead of
  // collapsing them, so the caller can render "not reported" apart from "0 complete".
  const reported = run.milestones_completed != null;
  const completed = new Set(run.milestones_completed ?? []);
  const done = frozen.reduce((n, m) => (completed.has(m.id) ? n + 1 : n), 0);
  return { done, total: frozen.length, reported };
}

// milestoneBadgeText renders the compact pill's label + tooltip from a MilestoneProgress,
// so the board (Dashboard/RunsList) and the run header stay in lockstep (PRD #265 M2). A
// reported run shows `M{done}/{total}`; an unreported one shows an en-dash numerator and
// says so in the tooltip, never a `0/N` that reads as failure on a done run.
export function milestoneBadgeText(p: MilestoneProgress): { label: string; title: string } {
  return p.reported
    ? { label: `M${p.done}/${p.total}`, title: "Milestones reported complete of the approved plan" }
    : { label: `M–/${p.total}`, title: "No milestone completion reported for this run" };
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

// isAwaitingInput is its PRD #88 SIBLING, deliberately not a widening of it. The two
// human-in-the-loop reasons are a distinct run status precisely so a surface can tell
// "approve my plan" from "answer my question" (PRD Decision 3); folding them into one
// predicate would put back exactly what the status split removed, and the board strip
// would then tell a user to approve a plan that does not exist.
//
// What they SHARE is the treatment: same warn tone, same loud card ring, same strip
// (D-O #4). needsHumanAttention below is where that sharing is expressed, once.
export function isAwaitingInput(status: string): boolean {
  return status === "awaiting_input";
}

// needsHumanAttention is the union: a run parked on ANY human decision. Use it for
// presentation that is identical for both (the card ring, the favicon dot); use the two
// predicates above wherever the WORDING differs, which is anywhere a human reads it.
export function needsHumanAttention(status: string): boolean {
  return isAwaitingApproval(status) || isAwaitingInput(status);
}
