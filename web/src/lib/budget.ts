// PRD #1189 (extend a run's wall-clock budget): the pure half of every web surface that
// reads a run's budget — the elapsed-vs-budget text in the header, the near-timeout panel,
// the paused panel's "remains when you resume" line, and the Extend chooser's facts. Kept
// framework-free and unit-tested in isolation, the same split limitWait.ts / runBadge.ts use
// and for the same reason: RunView needs routing, a live stream and a dozen API mocks to
// mount, so logic left inside it is only ever reachable through the component.
//
// 🔴 ONE CLOCK. The header's "used", the chooser's facts and #1170's deadline_at are all the
// SAME paused-aware active-time clock and MUST agree: on a run that sat 32m at its plan gate,
// raw wall elapsed (RunEvent.formatDuration(now − started_at)) would disagree with the
// deadline in the same header. For a RUNNING run we therefore derive "used" from the server's
// budget_total_seconds MINUS the live time-left to deadline_at (used = total − (deadline −
// now)), which is exactly now − started_at − paused and ages naturally on the client clock,
// never from raw elapsed. deadline_at is null while paused (it moves), so the paused view
// computes "remaining when resumed" from the frozen budget_used_seconds instead.

// A run shape carrying just the budget wire fields — satisfied by the full Run DTO and by a
// hand-built test double. Every field is optional/nullable to match the rollout-skew contract
// (an older api omits them); the functions below return null when the run carries no budget to
// show, so a caller falls back to plain elapsed with no branch of its own.
export type BudgetRun = {
  status: string;
  started_at?: string | null;
  deadline_at?: string | null;
  budget_wall_seconds?: number | null;
  budget_total_seconds?: number | null;
  budget_used_seconds?: number | null;
  budget_extension_seconds?: number | null;
  budget_extension_cap_seconds?: number | null;
};

// BudgetView is the normalized set of seconds every budget surface reads, so the header text,
// the panels and the chooser can never derive the same number two different ways.
export type BudgetView = {
  // Active time used so far (paused-aware). Aged client-side for a running run.
  usedSec: number;
  // The FROZEN budget, excluding any extension — the "/ 8h" number.
  wallSec: number;
  // The owner-granted extension on top of wallSec (0 when never extended).
  extSec: number;
  // The effective admin cap (0 = extending disabled).
  capSec: number;
  // How much extension an owner may still grant: cap − extension, clamped ≥ 0.
  allowanceLeftSec: number;
  // Time left before the run is stopped (running) / would be stopped after resume (paused).
  remainingSec: number;
  // The wall-clock deadline as an ISO string for a running run; null while paused (it moves).
  deadlineIso: string | null;
  // True for a running run (has a live deadline), false for a paused one.
  running: boolean;
};

/**
 * formatBudgetDuration renders a budget span compactly, two units at most, dropping a zero
 * lower unit: `8h`, `3h 48m`, `16h`, `45m`, `30s`, `2d 4h`. This is the BUDGET vocabulary
 * (`/ 8h`, not `8h 00m 00s`) — deliberately NOT RunEvent.formatDuration (which always shows
 * seconds) — so the header reads `3h 48m / 8h` and an extended budget reads `8h+2h`.
 */
export function formatBudgetDuration(seconds: number): string {
  const s = Number.isFinite(seconds) ? Math.max(0, Math.round(seconds)) : 0;
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return `${m}m`;
  return `${sec}s`;
}

/**
 * parseDurationToSeconds parses the Extend chooser's custom input into whole seconds, or null.
 *
 * Accepts one or more `<number><unit>` pairs (unit ∈ d/h/m/s), whitespace-insensitive, so
 * `2h`, `90m`, `1h30m`, `1h 30m` and `1d` all parse; a bare number (no unit), 0/negative, and
 * anything with stray characters return null. Returns null rather than throwing so the caller
 * simply disables Confirm and shows the "enter a duration like 2h" hint.
 */
export function parseDurationToSeconds(input: string): number | null {
  const t = input.trim().toLowerCase().replace(/\s+/g, "");
  if (t === "") return null;
  // The WHOLE string must be number+unit pairs — a trailing/leading stray char fails here.
  if (!/^(\d+[dhms])+$/.test(t)) return null;
  let total = 0;
  for (const m of t.matchAll(/(\d+)([dhms])/g)) {
    const n = Number(m[1]);
    const unit = m[2];
    total += unit === "d" ? n * 86400 : unit === "h" ? n * 3600 : unit === "m" ? n * 60 : n;
  }
  return total > 0 ? total : null;
}

/**
 * extendBudgetView derives the shared budget numbers for a run, or null when there is no
 * budget to show (rollout skew, a null-budget kind, or a status that never times out).
 *
 * A RUNNING run needs budget_total_seconds (server-computed, so RUN_TIMEOUT is already folded
 * in for a default-budget run); "used" is aged off deadline_at so it agrees with the header's
 * own deadline. A PAUSED run has no deadline_at (it moves), so it needs budget_wall_seconds
 * to state the frozen budget and derives "remaining when resumed" from the frozen used figure.
 * Every other status returns null.
 */
export function extendBudgetView(run: BudgetRun, nowMs: number): BudgetView | null {
  const ext = run.budget_extension_seconds ?? 0;
  const cap = run.budget_extension_cap_seconds ?? 0;
  const allowanceLeft = Math.max(0, cap - ext);

  if (run.status === "running") {
    const total = run.budget_total_seconds;
    if (total == null) return null;
    const wall = run.budget_wall_seconds ?? total - ext;
    let remaining: number;
    let used: number;
    const dl = run.deadline_at ? Date.parse(run.deadline_at) : NaN;
    if (Number.isFinite(dl)) {
      remaining = Math.max(0, (dl - nowMs) / 1000);
      used = Math.max(0, total - remaining);
    } else {
      // No deadline on the wire (rollout skew): fall back to the server's used snapshot.
      used = Math.max(0, run.budget_used_seconds ?? 0);
      remaining = Math.max(0, total - used);
    }
    return {
      usedSec: used,
      wallSec: Math.max(0, wall),
      extSec: ext,
      capSec: cap,
      allowanceLeftSec: allowanceLeft,
      remainingSec: remaining,
      deadlineIso: run.deadline_at ?? null,
      running: true,
    };
  }

  if (run.status === "paused") {
    // budget_total_seconds is null while paused (the deadline moves), so the frozen wall is
    // the only budget number available; without it (a default-budget run) there is nothing to
    // show and the caller keeps today's plain elapsed.
    const wall = run.budget_wall_seconds;
    if (wall == null) return null;
    // Rollout skew: an older payload can carry budget_wall_seconds WITHOUT budget_used_seconds.
    // Coercing the missing value to 0 would fabricate "0s used" and a full remaining budget, so
    // return null instead — the caller falls back to the legacy plain-elapsed display and omits
    // the remaining-budget sentence, rather than showing an invented number.
    if (run.budget_used_seconds == null) return null;
    const used = Math.max(0, run.budget_used_seconds);
    const remaining = Math.max(0, wall + ext - used);
    return {
      usedSec: used,
      wallSec: wall,
      extSec: ext,
      capSec: cap,
      allowanceLeftSec: allowanceLeft,
      remainingSec: remaining,
      deadlineIso: null,
      running: false,
    };
  }

  return null;
}

/**
 * budgetRightLabel renders the "/ 8h" right-hand side: the frozen budget, plus `+<extension>`
 * when the run has been extended, so an extended 8h reads `8h+2h`, NEVER `10h` (the extension
 * is an additive column the owner granted, distinct from the immutable frozen budget).
 */
export function budgetRightLabel(view: BudgetView): string {
  const base = formatBudgetDuration(view.wallSec);
  return view.extSec > 0 ? `${base}+${formatBudgetDuration(view.extSec)}` : base;
}

// formatLocalTime renders an ISO instant as the viewer's local wall-clock time ("15:20"),
// matching the PausedPanel heading. Null-safe: an absent/unparseable instant yields null so a
// caller drops the "stops at …" clause rather than rendering "stops at Invalid Date".
export function formatLocalTime(iso: string | null | undefined): string | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return null;
  return new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

// extendEnabled reports whether the Extend affordance should render at all: the effective
// admin cap is a real, positive number. undefined ⇒ "unknown" (rollout skew) ⇒ hidden, and a
// real 0 ⇒ extending turned off ⇒ hidden — the two are deliberately treated the same for
// VISIBILITY (the button is absent either way), but never conflated as VALUES elsewhere.
export function extendEnabled(run: Pick<BudgetRun, "budget_extension_cap_seconds">): boolean {
  const cap = run.budget_extension_cap_seconds;
  return typeof cap === "number" && cap > 0;
}
