import { describe, it, expect } from "vitest";
import {
  activeRunInHistory,
  canOpenRunView,
  effectiveRunStatus,
  formatElapsed,
  hasActiveRun,
  healthBadge,
  healthFlagLabel,
  isAwaitingApproval,
  isAwaitingFollowup,
  isAwaitingInput,
  isHealthFlaggableStatus,
  isPlanningRun,
  isRevisingRun,
  isStoppedRun,
  milestoneBadge,
  milestoneBadgeText,
  mrChipState,
  mrChipSuffix,
  mrChipTitle,
  needsHumanAttention,
  priorityBadge,
  retryHint,
  runBadge,
  runStatusTone,
  shouldShowHealthFlag,
} from "./runBadge";
import { RUN_STATUS_TONES } from "../components/ui";
import { isTerminalRun, TERMINAL_RUN_STATUSES } from "./runStatus";
import type { LatestRun, RunStatus, StopKind } from "./api";

// run builds a LatestRun with sane defaults, overridable per test.
function run(over: Partial<LatestRun> = {}): LatestRun {
  return {
    id: "run-1",
    status: "queued",
    mr_iid: null,
    mr_web_url: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    owner_name: "Robin Diaz",
    worker_name: null,
    is_mine: true,
    run_count: 1,
    created_at: "2026-07-04T12:00:00Z",
    updated_at: "2026-07-04T12:00:00Z",
    ...over,
  };
}

const NOW = Date.parse("2026-07-04T12:04:00Z"); // 4m after the default created_at

describe("runBadge taxonomy", () => {
  it("queued → queue 'queued'", () => {
    expect(runBadge(run({ status: "queued" }), NOW)).toEqual({
      kind: "badge",
      label: "queued",
      tone: "queue",
      pulse: false,
    });
  });

  it("claimed → info 'claimed'", () => {
    const b = runBadge(run({ status: "claimed" }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "claimed", tone: "info", pulse: false });
  });

  it("running → bare pulsing info badge (elapsed moved to the meta duration token, issue #256 M4)", () => {
    const b = runBadge(run({ status: "running", worker_name: "laptop" }), NOW);
    expect(b).toMatchObject({ kind: "badge", tone: "info", pulse: true });
    if (b.kind === "badge") expect(b.label).toBe("running");
  });

  it("awaiting_approval → amber (warning) badge", () => {
    const b = runBadge(run({ status: "awaiting_approval" }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "awaiting approval", tone: "warning" });
  });

  it("failed → rose (danger) badge carrying the failure reason as a tooltip", () => {
    const b = runBadge(run({ status: "failed", failure_reason: "boom" }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "failed", tone: "danger", title: "boom" });
  });

  it("cancelled → calm neutral 'stopped', never danger", () => {
    const b = runBadge(run({ status: "cancelled" }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "stopped", tone: "neutral" });
  });

  it("failed with a server-stamped stop_kind → 'stopped', never danger", () => {
    const b = runBadge(run({ status: "failed", stop_kind: "cancelled" }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "stopped", tone: "neutral" });
    // A live-poller plan reject carrying the user's VERBATIM reason: the string
    // heuristic could never catch this, stop_kind does (PRD #33 success criterion 1).
    const r = runBadge(
      run({ status: "failed", stop_kind: "plan_rejected", failure_reason: "this is the wrong approach entirely" }),
      NOW,
    );
    expect(r).toMatchObject({ kind: "badge", label: "stopped", tone: "neutral" });
  });

  it("completed with an MR → MR chip carrying the derived state (open by default)", () => {
    expect(runBadge(run({ status: "completed", mr_iid: 42 }), NOW)).toEqual({
      kind: "mr",
      mrIid: 42,
      mrState: "open",
    });
  });

  it("completed with a merged MR → MR chip carrying mrState 'merged'", () => {
    expect(runBadge(run({ status: "completed", mr_iid: 42, mr_state: "merged" }), NOW)).toEqual({
      kind: "mr",
      mrIid: 42,
      mrState: "merged",
    });
  });

  it("completed without an MR → plain ok 'completed' badge (never invisible)", () => {
    const b = runBadge(run({ status: "completed", mr_iid: null }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "completed", tone: "ok" });
  });

  // Decision 4 guards: a stop_kind stamped at verdict enqueue must not hijack a run
  // that is still running or that raced to completion.
  it("stamped stop_kind but still running → renders by status, not 'stopped'", () => {
    const b = runBadge(run({ status: "running", stop_kind: "cancelled" }), NOW);
    expect(b).toMatchObject({ kind: "badge", tone: "info", pulse: true });
    if (b.kind === "badge") expect(b.label).toBe("running");
  });

  it("stamped stop_kind but completed with an MR → MR chip (reject-then-approve race)", () => {
    expect(runBadge(run({ status: "completed", stop_kind: "plan_rejected", mr_iid: 42 }), NOW)).toEqual({
      kind: "mr",
      mrIid: 42,
      mrState: "open",
    });
  });

  // PRD #35. Note what this replaces: before the explicit arm, `limit_wait` fell to
  // runBadge's `default:` and rendered as a NEUTRAL GREY pill reading "limit wait" —
  // legible, and wrong twice over, since a parked run is neither idle nor
  // indistinguishable from a stopped one.
  it("limit_wait → warn 'limit wait', with a title that explains the wait", () => {
    const b = runBadge(run({ status: "limit_wait" }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "limit wait", tone: "warning", pulse: false });
    if (b.kind === "badge") expect(b.title).toMatch(/resumes on its own/i);
  });

  it("🔴 limit_wait's badge is STATIC — no countdown, no elapsed, on any input", () => {
    // The board's LatestRun projection carries neither retry_not_before nor
    // limit_resets_at, so there is nothing here to count down from. This is the test
    // that fails if someone widens LatestRun to put a countdown on the card — which
    // would also force a Board.tsx prop change, the one thing PRD #35's web work is
    // not allowed to do while PRD #102 rewrites that file.
    const early = runBadge(run({ status: "limit_wait", created_at: "2026-07-04T11:00:00Z" }), NOW);
    const late = runBadge(run({ status: "limit_wait", created_at: "2026-07-04T11:59:00Z" }), NOW);
    expect(early).toEqual(late);
    if (early.kind === "badge") expect(early.label).toBe("limit wait");
  });

  it("🔴 a parked run carrying a stale health flag renders 'limit wait', NOT '⚠ stalled'", () => {
    // The belt to the server's braces. The server clears health on park entry — it
    // has to, because its health detector's allowlist never revisits a parked run, so
    // a flag live at park time would freeze for days. This asserts the web does not
    // depend on that having worked: `limit_wait` is absent from
    // HEALTH_FLAGGABLE_STATUSES, so healthBadge returns null and the status wins.
    //
    // Getting this backwards is the concrete failure the design names: a run behaving
    // exactly as designed, sitting on the board under a red-flag "stalled" chip.
    const b = runBadge(
      run({ status: "limit_wait", health: "stalled", health_since: "2026-07-04T11:59:00Z" }),
      NOW,
    );
    expect(b).toMatchObject({ kind: "badge", label: "limit wait", tone: "warning" });
    expect(isHealthFlaggableStatus("limit_wait")).toBe(false);
    expect(shouldShowHealthFlag("stalled", "limit_wait")).toBe(false);
  });
});

// issue #321 M3. The pre-approval PLANNING phase is wired onto every status surface via
// one effective-status seam. The web TRUSTS the server's is_planning boolean and never
// re-derives it — these pin that trust, and that planning is meaningful only while running.
describe("planning phase (issue #321)", () => {
  it("isPlanningRun: running + is_planning true ⇒ planning", () => {
    expect(isPlanningRun({ status: "running", is_planning: true })).toBe(true);
  });

  it("isPlanningRun: is_planning true but no longer running ⇒ NOT planning", () => {
    // is_planning is only meaningful while running: a completed run carrying a stale
    // true must not read as planning.
    expect(isPlanningRun({ status: "completed", is_planning: true })).toBe(false);
  });

  it("isPlanningRun: running but is_planning false/absent ⇒ NOT planning", () => {
    expect(isPlanningRun({ status: "running", is_planning: false })).toBe(false);
    // Absent (rollout skew) reads as not-planning — the `=== true` guard.
    expect(isPlanningRun({ status: "running" })).toBe(false);
  });

  it("effectiveRunStatus: 'planning' while planning, else the raw status", () => {
    expect(effectiveRunStatus({ status: "running", is_planning: true })).toBe("planning");
    // is_planning true but completed → its raw status, never "planning".
    expect(effectiveRunStatus({ status: "completed", is_planning: true })).toBe("completed");
    expect(effectiveRunStatus({ status: "running", is_planning: false })).toBe("running");
    expect(effectiveRunStatus({ status: "running" })).toBe("running");
  });

  it("runBadge: a running run with is_planning true → pulsing indigo 'planning' badge", () => {
    expect(runBadge(run({ status: "running", is_planning: true }), NOW)).toEqual({
      kind: "badge",
      label: "planning",
      tone: "plan",
      pulse: true,
    });
  });

  it("runBadge: the SAME run without is_planning still reads as 'running' (unchanged)", () => {
    // The planning wiring must not disturb an ordinary running run — is_planning
    // false OR absent both fall through to the existing running badge.
    const off = runBadge(run({ status: "running", is_planning: false }), NOW);
    expect(off).toMatchObject({ kind: "badge", label: "running", tone: "info", pulse: true });
    const absent = runBadge(run({ status: "running" }), NOW);
    expect(absent).toMatchObject({ kind: "badge", label: "running", tone: "info", pulse: true });
  });

  it("runStatusTone maps the effective 'planning' status to the indigo plan tone", () => {
    expect(runStatusTone("planning", null)).toBe("plan");
  });
});

// issue #750. A run re-planning after a revise keeps status === "awaiting_approval"
// server-side, but its derived is_revising flag flips it to the "revising" effective
// status so it drops out of the human-attention grouping while the planner reworks the
// plan. The web TRUSTS the server's is_revising boolean but — because the server does NOT
// status-gate it — combines it with status (isRevisingRun requires awaiting_approval),
// exactly mirroring isPlanningRun's running-guard. These pin that combination.
describe("revising phase (issue #750)", () => {
  it("isRevisingRun: awaiting_approval + is_revising true ⇒ revising", () => {
    expect(isRevisingRun({ status: "awaiting_approval", is_revising: true })).toBe(true);
  });

  it("isRevisingRun: is_revising true but NOT awaiting_approval ⇒ NOT revising", () => {
    // The server flag is not status-gated, so it can linger on a run whose status has
    // moved. A terminal/other status must never read as revising on the strength of it.
    expect(isRevisingRun({ status: "completed", is_revising: true })).toBe(false);
    expect(isRevisingRun({ status: "running", is_revising: true })).toBe(false);
    expect(isRevisingRun({ status: "failed", is_revising: true })).toBe(false);
  });

  it("isRevisingRun: awaiting_approval but is_revising false/absent ⇒ NOT revising", () => {
    expect(isRevisingRun({ status: "awaiting_approval", is_revising: false })).toBe(false);
    // Absent (rollout skew) reads as not-revising — the `=== true` guard.
    expect(isRevisingRun({ status: "awaiting_approval" })).toBe(false);
  });

  it("effectiveRunStatus: 'revising' while revising, else the raw status", () => {
    expect(effectiveRunStatus({ status: "awaiting_approval", is_revising: true })).toBe("revising");
    // is_revising true but not awaiting_approval → its raw status, never "revising".
    expect(effectiveRunStatus({ status: "completed", is_revising: true })).toBe("completed");
    // A plain awaiting_approval run (no revise in flight) stays awaiting_approval.
    expect(effectiveRunStatus({ status: "awaiting_approval", is_revising: false })).toBe("awaiting_approval");
    expect(effectiveRunStatus({ status: "awaiting_approval" })).toBe("awaiting_approval");
  });

  it("planning and revising are mutually exclusive (distinct status guards)", () => {
    // is_planning is only meaningful while running; is_revising only while
    // awaiting_approval. A run cannot be both, so effectiveRunStatus never has to choose.
    expect(effectiveRunStatus({ status: "running", is_planning: true, is_revising: true })).toBe("planning");
    expect(effectiveRunStatus({ status: "awaiting_approval", is_planning: true, is_revising: true })).toBe("revising");
  });

  it("runBadge: a revising run → pulsing info 'revising' badge (not the awaiting warn)", () => {
    expect(runBadge(run({ status: "awaiting_approval", is_revising: true }), NOW)).toEqual({
      kind: "badge",
      label: "revising",
      tone: "info",
      pulse: true,
    });
  });

  it("runBadge: the SAME run without is_revising still reads as 'awaiting approval'", () => {
    // The revising wiring must not disturb an ordinary awaiting_approval run — is_revising
    // false OR absent both fall through to the existing warn-toned awaiting badge.
    const off = runBadge(run({ status: "awaiting_approval", is_revising: false }), NOW);
    expect(off).toMatchObject({ kind: "badge", label: "awaiting approval", tone: "warning" });
    const absent = runBadge(run({ status: "awaiting_approval" }), NOW);
    expect(absent).toMatchObject({ kind: "badge", label: "awaiting approval", tone: "warning" });
  });

  it("runStatusTone maps the effective 'revising' status to the calm info tone", () => {
    expect(runStatusTone("revising", null)).toBe("info");
  });

  it("a revising run is NOT in the human-attention grouping (via effective status)", () => {
    // The board strip, card ring and favicon dot all classify from effectiveRunStatus.
    // isAwaitingApproval / needsHumanAttention on the EFFECTIVE status is what drops a
    // revising run out — the raw status would still read awaiting_approval.
    const revising = { status: "awaiting_approval", is_revising: true };
    expect(isAwaitingApproval(effectiveRunStatus(revising))).toBe(false);
    expect(needsHumanAttention(effectiveRunStatus(revising))).toBe(false);
    // Once the next plan lands (is_revising false), it returns to the grouping.
    const approved = { status: "awaiting_approval", is_revising: false };
    expect(isAwaitingApproval(effectiveRunStatus(approved))).toBe(true);
    expect(needsHumanAttention(effectiveRunStatus(approved))).toBe(true);
  });
});

// PRD #35. runBadge.ts's header has always CLAIMED its tones mirror StatusPill's
// RUN_STATUS_TONES "so one status renders one color everywhere". Until this, nothing
// enforced it: the two maps live in different files, neither imports the other, and a
// status added to one renders in a different colour on the other surface with no
// build error and no failing test. The mirror was a comment.
describe("RUN_STATUS_TONES ↔ runStatusTone agreement", () => {
  it("gives every status in the pill map the same tone on the board/list surface", () => {
    for (const [status, cfg] of Object.entries(RUN_STATUS_TONES)) {
      // stop_kind null: the "stopped" nuance is a runBadge-only overlay that
      // deliberately has no StatusPill counterpart (RunView passes the literal
      // "stopped", which is not a status at all), so it is out of scope here.
      expect([status, runStatusTone(status, null)]).toEqual([status, cfg.tone]);
    }
  });

  it("covers limit_wait specifically, on both surfaces", () => {
    // Named separately from the loop because the loop would still pass if BOTH maps
    // were missing the status — it iterates the pill map, so an absent key is an
    // absent assertion. This is the case that would have caught a limit_wait added to
    // only one of the two.
    expect(RUN_STATUS_TONES["limit_wait"]).toEqual({ tone: "warning" });
    expect(runStatusTone("limit_wait", null)).toBe("warning");
  });

  it("leaves the unknown-status fallback alone", () => {
    // Adding a key must not change how a status NOBODY has heard of behaves — an old
    // web build against a newer server still has to degrade to a neutral pill rather
    // than crash or borrow a neighbour's colour.
    expect(RUN_STATUS_TONES["nine_hour_experimental"]).toBeUndefined();
    expect(runStatusTone("nine_hour_experimental", null)).toBe("neutral");
  });
});

describe("runBadge health warn variant (PRD #47)", () => {
  // health_since is 1m before NOW; created_at is 4m before. The badge must count
  // from health_since ("stuck for Xm"), not created_at.
  const flagged = (over: Partial<LatestRun> = {}) =>
    run({ status: "running", health: "stalled", health_since: "2026-07-04T12:03:00Z", ...over });

  it("a flagged running run renders ⚠ <label> · <elapsed> in the warn tone, keeping the pulse", () => {
    expect(runBadge(flagged({ health_reason: "the agent stopped sending updates" }), NOW)).toEqual({
      kind: "badge",
      label: "⚠ stalled · 1m",
      tone: "warning",
      pulse: true,
      title: "the agent stopped sending updates",
    });
  });

  it("elapsed counts from health_since, not created_at", () => {
    // created_at 4m ago, health_since 1m ago → 1m, proving the source.
    const b = runBadge(flagged(), NOW);
    expect(b).toMatchObject({ label: "⚠ stalled · 1m" });
  });

  it("a non-owner (health_reason null) gets no tooltip", () => {
    const b = healthBadge(flagged({ health_reason: null }), NOW);
    if (b?.kind !== "badge") throw new Error("expected a badge");
    expect(b.title).toBeUndefined();
  });

  it("labels every flag", () => {
    expect(healthFlagLabel("stalled")).toBe("stalled");
    expect(healthFlagLabel("looping")).toBe("looping");
    expect(healthFlagLabel("slow")).toBe("slow");
    expect(healthFlagLabel("waiting_worker")).toBe("waiting for worker");
    expect(healthFlagLabel("approval_idle")).toBe("needs approval");
    expect(healthFlagLabel("ok")).toBeNull();
  });

  it("a healthy run keeps its normal status badge (no ⚠)", () => {
    const b = runBadge(run({ status: "running", health: "ok" }), NOW);
    expect(b).toMatchObject({ label: "running", tone: "info" });
  });

  it("belt-and-braces: a terminal run never shows a stale flag", () => {
    // A completed run carrying a leftover flag must still render its completed/MR
    // badge, never ⚠ — the warn variant fires only for a flaggable status.
    expect(isHealthFlaggableStatus("completed")).toBe(false);
    expect(healthBadge(run({ status: "completed", health: "stalled", mr_iid: 9 }), NOW)).toBeNull();
    expect(runBadge(run({ status: "completed", health: "stalled", mr_iid: 9 }), NOW)).toEqual({
      kind: "mr",
      mrIid: 9,
      mrState: "open",
    });
  });

  it("shouldShowHealthFlag gates on both a non-ok flag and a flaggable status", () => {
    expect(shouldShowHealthFlag("stalled", "running")).toBe(true);
    expect(shouldShowHealthFlag("ok", "running")).toBe(false);
    expect(shouldShowHealthFlag("stalled", "completed")).toBe(false);
    expect(shouldShowHealthFlag("waiting_worker", "queued")).toBe(true);
    expect(shouldShowHealthFlag("approval_idle", "awaiting_approval")).toBe(true);
  });
});

describe("isStoppedRun (server-stamped stop_kind, terminal-guarded)", () => {
  it("true for cancelled status", () => {
    expect(isStoppedRun("cancelled", null)).toBe(true);
    expect(isStoppedRun("cancelled", "cancelled")).toBe(true);
  });
  it("true for failed carrying a stop_kind", () => {
    expect(isStoppedRun("failed", "cancelled")).toBe(true);
    expect(isStoppedRun("failed", "plan_rejected")).toBe(true);
  });
  it("false for a genuine failure (no stop_kind)", () => {
    expect(isStoppedRun("failed", null)).toBe(false);
  });
  // Decision 4 guard cases: on the live path stop_kind is stamped at verdict
  // enqueue, before the run is terminal — and a reject-then-approve race can even
  // complete a stamped run. The `failed`/`cancelled` guard keeps those honest.
  it("false for a NON-TERMINAL run even when a stop_kind was already stamped", () => {
    expect(isStoppedRun("awaiting_approval", "plan_rejected")).toBe(false);
    expect(isStoppedRun("running", "cancelled")).toBe(false);
  });
  it("false for a COMPLETED run that carries a stop_kind (reject-then-approve race)", () => {
    // Must NOT read as stopped — the run finished and should show its MR chip.
    expect(isStoppedRun("completed", "plan_rejected")).toBe(false);
  });

  // PRD #108 M5. The predicate used to be `stop_kind != null`, which silently
  // absorbed every future value — so an auto-stopped run (failed + a non-null
  // stop_kind) would have rendered as a calm neutral "stopped" pill across
  // RunsList / IssueView / RunView / the board AND dropped out of favicon.ts's
  // attention set. An auto-stop is uzi killing a run because it was BROKEN. It has
  // to look like breakage.
  //
  // TypeScript cannot catch this: stop_kind crosses a JSON decode boundary, so the
  // null test compiles perfectly against a value the union never listed. Only this
  // test stands between a new stop_kind and silently-calm breakage.
  it("false for auto_stopped — a server kill is breakage, not a deliberate stop", () => {
    expect(isStoppedRun("failed", "auto_stopped")).toBe(false);
  });
  it("still true for both HUMAN kinds, so the narrowing did not overreach", () => {
    // The positive control for the case above: if this pair ever goes false the
    // predicate has stopped recognising deliberate stops at all, and the
    // auto_stopped assertion would pass for entirely the wrong reason.
    expect(isStoppedRun("failed", "cancelled")).toBe(true);
    expect(isStoppedRun("failed", "plan_rejected")).toBe(true);
  });
});

describe("runStatusTone (list-row tone)", () => {
  it("awaiting_approval → warning", () => {
    expect(runStatusTone("awaiting_approval", null)).toBe("warning");
  });
  it("deliberate stops → neutral, never danger", () => {
    expect(runStatusTone("cancelled", null)).toBe("neutral");
    expect(runStatusTone("failed", "cancelled")).toBe("neutral");
    expect(runStatusTone("failed", "plan_rejected")).toBe("neutral");
  });
  it("a genuine failure (no stop_kind) → danger", () => {
    expect(runStatusTone("failed", null)).toBe("danger");
  });
  it("an auto-stopped run → danger, exactly like any other breakage (PRD #108 M5)", () => {
    // runStatusTone reads isStoppedRun, so this is the tone half of the same
    // narrowing: it must land on `danger` with the genuine failures, not on the
    // `neutral` the deliberate stops get.
    expect(runStatusTone("failed", "auto_stopped")).toBe("danger");
  });
  it("completed → ok, claimed/running → info, queued → queue (matches StatusPill)", () => {
    expect(runStatusTone("completed", null)).toBe("ok");
    expect(runStatusTone("claimed", null)).toBe("info");
    expect(runStatusTone("running", null)).toBe("info");
    expect(runStatusTone("queued", null)).toBe("queue");
  });
});

describe("mrChipState (derived MR-state variant, PRD #33)", () => {
  it("maps merged and closed to their own variant", () => {
    expect(mrChipState("merged")).toBe("merged");
    expect(mrChipState("closed")).toBe("closed");
  });
  it("treats opened / locked / unknown / null / undefined as 'open' (chip unchanged — SC2)", () => {
    expect(mrChipState("opened")).toBe("open");
    expect(mrChipState("locked")).toBe("open");
    expect(mrChipState("something-else")).toBe("open");
    expect(mrChipState(null)).toBe("open");
    expect(mrChipState(undefined)).toBe("open");
  });
});

describe("mrChipSuffix / mrChipTitle", () => {
  it("suffix is empty for open, the state word otherwise", () => {
    expect(mrChipSuffix("open")).toBe("");
    expect(mrChipSuffix("merged")).toBe(" merged");
    expect(mrChipSuffix("closed")).toBe(" closed");
  });
  it("title scopes merged/closed to 'as of last sync' with the per-forge noun + platform", () => {
    expect(mrChipTitle("merged", "gitlab")).toBe("Merge request merged (as of last sync)");
    expect(mrChipTitle("closed", "gitlab")).toBe("Merge request closed unmerged (as of last sync)");
    // GitLab open title is byte-identical to pre-#65 (dark-landing: names the platform).
    expect(mrChipTitle("open", "gitlab")).toBe("Open the merge request on GitLab");
    // Forgejo says "pull request" and names its own platform, never "GitLab".
    expect(mrChipTitle("merged", "forgejo")).toBe("Pull request merged (as of last sync)");
    expect(mrChipTitle("open", "forgejo")).toBe("Open the pull request on Forgejo");
  });
});

describe("retryHint (×N)", () => {
  it("is null for a single run", () => {
    expect(retryHint(1)).toBeNull();
    expect(retryHint(0)).toBeNull();
  });
  it("counts when the issue has run more than once", () => {
    expect(retryHint(2)).toBe("×2");
    expect(retryHint(5)).toBe("×5");
  });
});

describe("hasActiveRun (start-run gate input)", () => {
  it("is false when there is no run", () => {
    expect(hasActiveRun(null)).toBe(false);
    expect(hasActiveRun(undefined)).toBe(false);
  });
  it("is true for a non-terminal run (blocks a second start)", () => {
    for (const s of ["queued", "claimed", "running", "awaiting_approval"] as RunStatus[]) {
      expect(hasActiveRun(run({ status: s }))).toBe(true);
    }
  });
  it("is false for a terminal run (a new run may be started)", () => {
    for (const s of ["completed", "failed", "cancelled"] as RunStatus[]) {
      expect(hasActiveRun(run({ status: s }))).toBe(false);
    }
  });
});

describe("canOpenRunView (is_mine link gating)", () => {
  it("renders the run-view link only for the owner", () => {
    expect(canOpenRunView(run({ is_mine: true }))).toBe(true);
    expect(canOpenRunView(run({ is_mine: false }))).toBe(false);
  });
  it("is false when there is no run", () => {
    expect(canOpenRunView(null)).toBe(false);
  });
});

describe("formatElapsed", () => {
  it("seconds under a minute", () => {
    expect(formatElapsed(0)).toBe("0s");
    expect(formatElapsed(45_000)).toBe("45s");
  });
  it("minutes under an hour", () => {
    expect(formatElapsed(4 * 60_000)).toBe("4m");
    expect(formatElapsed(59 * 60_000)).toBe("59m");
  });
  it("hours and minutes", () => {
    expect(formatElapsed(90 * 60_000)).toBe("1h 30m");
  });
  it("never goes negative", () => {
    expect(formatElapsed(-5000)).toBe("0s");
  });
});

describe("isAwaitingApproval (attention strip filter)", () => {
  it("flags only awaiting_approval", () => {
    expect(isAwaitingApproval("awaiting_approval")).toBe(true);
    expect(isAwaitingApproval("running")).toBe(false);
  });

  // PRD #88. The two predicates must stay DISJOINT: the run status is split precisely
  // so a surface can tell "approve my plan" from "answer my question", and the board
  // strip names the action. Widening isAwaitingApproval to cover both would put back
  // exactly what the status split removed, and tell a user to approve a plan that does
  // not exist.
  it("does NOT absorb awaiting_input, and its sibling does not absorb approval", () => {
    expect(isAwaitingApproval("awaiting_input")).toBe(false);
    expect(isAwaitingInput("awaiting_input")).toBe(true);
    expect(isAwaitingInput("awaiting_approval")).toBe(false);
    expect(isAwaitingInput("running")).toBe(false);
  });
});

describe("milestoneBadge (M{done}/{total}, PRD #122)", () => {
  const ms = (n: number) => Array.from({ length: n }, (_, i) => ({ id: `m${i + 1}`, title: `Milestone ${i + 1}` }));

  it("returns null when there is no frozen milestone list (fall back to iteration badge)", () => {
    expect(milestoneBadge({ milestones: null, milestones_completed: null })).toBeNull();
    expect(milestoneBadge({ milestones: [], milestones_completed: ["m1"] })).toBeNull();
    // Missing keys entirely (a pre-#122 api pod omits them) reads as no milestones.
    expect(milestoneBadge({})).toBeNull();
  });

  it("a 3-of-7 run → { done: 3, total: 7, reported: true }", () => {
    expect(
      milestoneBadge({ milestones: ms(7), milestones_completed: ["m1", "m2", "m3"] }),
    ).toEqual({ done: 3, total: 7, reported: true });
  });

  it("completed ids not in the frozen list are CLAMPED, so done never exceeds total", () => {
    // milestones_completed is a monotone union: it can still name an id dropped from
    // the approved set. Counting it would read M8/7. Only frozen members count.
    expect(
      milestoneBadge({
        milestones: ms(7),
        milestones_completed: ["m1", "m2", "m3", "dropped-a", "dropped-b"],
      }),
    ).toEqual({ done: 3, total: 7, reported: true });
  });

  it("a null milestones_completed counts as zero done AND not reported (PRD #265 M2)", () => {
    // The whole point of #265: null (nothing reported) must be distinguishable from a
    // real 0-of-N so a done run that never reported does not read as failure.
    expect(milestoneBadge({ milestones: ms(4), milestones_completed: null })).toEqual({
      done: 0,
      total: 4,
      reported: false,
    });
  });

  it("an EMPTY reported set is 0 done but REPORTED — distinct from null (PRD #265 M2)", () => {
    expect(milestoneBadge({ milestones: ms(4), milestones_completed: [] })).toEqual({
      done: 0,
      total: 4,
      reported: true,
    });
  });

  it("a missing milestones_completed key reads as not reported", () => {
    expect(milestoneBadge({ milestones: ms(4) })).toEqual({ done: 0, total: 4, reported: false });
  });
});

describe("milestoneBadgeText (not-reported vs 0/N, PRD #265 M2)", () => {
  it("a reported run shows M{done}/{total} with the completion tooltip", () => {
    expect(milestoneBadgeText({ done: 3, total: 7, reported: true })).toEqual({
      label: "M3/7",
      title: "Milestones reported complete of the approved plan",
    });
  });

  it("a reported 0-of-N still shows M0/N (a genuine zero, not 'not reported')", () => {
    expect(milestoneBadgeText({ done: 0, total: 4, reported: true }).label).toBe("M0/4");
  });

  it("an unreported run shows M–/N and says 'not reported' in the tooltip, never a 0/N", () => {
    const t = milestoneBadgeText({ done: 0, total: 4, reported: false });
    expect(t.label).toBe("M–/4");
    expect(t.label).not.toContain("0");
    expect(t.title).toBe("No milestone completion reported for this run");
  });
});

// PRD #390 D5 / M4 guard. The two functions above are pinned in isolation; this threads
// milestoneBadge → milestoneBadgeText so the null-vs-[] distinction is asserted END TO END,
// on both the `reported` boolean AND the composed label. A regression that collapsed a null
// tracker into [] (a genuinely-unreported run reading as a failure-looking M0/N) — or the
// reverse — would keep each half locally plausible and only redden here. The tooltip wording
// must differ between the two states too: it is the only thing that tells them apart on hover.
describe("milestone progress null-vs-[] end to end (PRD #390 D5)", () => {
  const ms = (n: number) =>
    Array.from({ length: n }, (_, i) => ({ id: `m${i + 1}`, title: `Milestone ${i + 1}` }));

  it("a null tracker on a milestone-bearing run is NOT reported and composes to M–/N", () => {
    const p = milestoneBadge({ milestones: ms(5), milestones_completed: null });
    expect(p).not.toBeNull();
    expect(p!.reported).toBe(false);
    const text = milestoneBadgeText(p!);
    expect(text.label).toBe("M–/5");
    expect(text.label).not.toContain("0");
    expect(text.title).toBe("No milestone completion reported for this run");
  });

  it("an ABSENT milestones_completed key behaves exactly like null (M–/N, not reported)", () => {
    const p = milestoneBadge({ milestones: ms(5) });
    expect(p).not.toBeNull();
    expect(p!.reported).toBe(false);
    expect(milestoneBadgeText(p!).label).toBe("M–/5");
  });

  it("an empty reported set IS reported and composes to a genuine M0/N, distinct from neutral", () => {
    const reported = milestoneBadge({ milestones: ms(5), milestones_completed: [] });
    expect(reported).not.toBeNull();
    expect(reported!.reported).toBe(true);
    const text = milestoneBadgeText(reported!);
    expect(text.label).toBe("M0/5");
    expect(text.title).toBe("Milestones reported complete of the approved plan");
    // The neutral and the genuine-zero states must NOT share a tooltip — that is the whole
    // distinction D5 exists to preserve.
    const neutral = milestoneBadgeText(milestoneBadge({ milestones: ms(5), milestones_completed: null })!);
    expect(text.title).not.toBe(neutral.title);
    expect(text.label).not.toBe(neutral.label);
  });

  it("a single reported completion composes to M1/N and is reported", () => {
    const p = milestoneBadge({ milestones: ms(5), milestones_completed: ["m1"] });
    expect(p).not.toBeNull();
    expect(p!.reported).toBe(true);
    expect(milestoneBadgeText(p!).label).toBe("M1/5");
  });
});

describe("priorityBadge (queue priority → pill, PRD #320 D8)", () => {
  it("background → a neutral 'Deprioritized' pill explaining it yields", () => {
    const b = priorityBadge("background");
    expect(b).toMatchObject({ label: "Deprioritized", tone: "neutral" });
    expect(b?.title).toMatch(/yield/i);
  });

  it("expedited → an info 'Expedited' pill (bumped to the front)", () => {
    const b = priorityBadge("expedited");
    expect(b).toMatchObject({ label: "Expedited", tone: "info" });
    expect(b?.title).toMatch(/front of the queue/i);
  });

  it("restored → a warning-toned 'Restored' pill (past the grace)", () => {
    const b = priorityBadge("restored");
    expect(b).toMatchObject({ label: "Restored", tone: "warning" });
    expect(b?.title).toMatch(/no longer yielding/i);
  });

  // A normal run stays quiet — and, crucially, so does an ABSENT value: a pre-#320 api
  // omits the field, and api.ts pins "absent ⇒ normal", so both must map to no pill or a
  // rollout-skew row would sprout a spurious badge.
  it("normal → null (a normal row shows no pill)", () => {
    expect(priorityBadge("normal")).toBeNull();
  });

  it("undefined (absent ⇒ normal) → null", () => {
    expect(priorityBadge(undefined)).toBeNull();
  });
});

describe("needsHumanAttention (the shared treatment, PRD #88 D-O #4, PRD #517)", () => {
  it("is the union of the three human-blocked statuses and nothing else", () => {
    expect(needsHumanAttention("awaiting_approval")).toBe(true);
    expect(needsHumanAttention("awaiting_input")).toBe(true);
    // PRD #517: awaiting_followup is the third member. Mutation guard: dropping it
    // from needsHumanAttention makes this assert red (the card ring / favicon dot
    // would stop firing for a run parked on the user's next follow-up).
    expect(needsHumanAttention("awaiting_followup")).toBe(true);
    for (const s of ["queued", "claimed", "running", "completed", "failed", "cancelled", ""]) {
      expect(needsHumanAttention(s)).toBe(false);
    }
  });
});

describe("isAwaitingFollowup (PRD #517, the third human-in-the-loop sibling)", () => {
  it("matches only awaiting_followup, and the siblings do not absorb it", () => {
    expect(isAwaitingFollowup("awaiting_followup")).toBe(true);
    expect(isAwaitingFollowup("awaiting_input")).toBe(false);
    expect(isAwaitingFollowup("awaiting_approval")).toBe(false);
    expect(isAwaitingFollowup("running")).toBe(false);
    // The split is deliberate (mirrors isAwaitingInput): a follow-up park is not an
    // unanswered question, so neither existing predicate covers it.
    expect(isAwaitingApproval("awaiting_followup")).toBe(false);
    expect(isAwaitingInput("awaiting_followup")).toBe(false);
  });
});

describe("awaiting_followup presentation (PRD #517)", () => {
  it("gets the SAME warn tone as the other parks", () => {
    expect(runStatusTone("awaiting_followup", null)).toBe("warning");
    expect(runStatusTone("awaiting_followup", null)).toBe(
      runStatusTone("awaiting_approval", null),
    );
  });

  it("has its own badge case with distinct copy, not the neutral default", () => {
    // Mutation guard: removing the awaiting_followup arm from runBadge's switch drops
    // it to the default neutral chip (label "awaiting followup", tone neutral), which
    // this assert catches — it must be the warn "awaiting follow-up" pill, and its
    // label must NOT reuse awaiting_input's "needs your answer".
    const badge = runBadge(run({ status: "awaiting_followup" }), NOW);
    expect(badge).toEqual({
      kind: "badge",
      label: "awaiting follow-up",
      tone: "warning",
      pulse: false,
      title: "Waiting for your next follow-up. Send one to continue the run.",
    });
    if (badge.kind === "badge") {
      expect(badge.label).not.toBe("needs your answer");
    }
  });

  it("is NOT terminal (isTerminalRun false) — it is a non-terminal park", () => {
    expect(isTerminalRun("awaiting_followup")).toBe(false);
    expect(TERMINAL_RUN_STATUSES).not.toContain("awaiting_followup");
  });
});

describe("stop_kind='stopped' (PRD #517 M4, deliberate HUMAN_STOP_KINDS exclusion)", () => {
  it("type-checks as a StopKind and does NOT render as a calm 'stopped' pill", () => {
    // 'stopped' is a valid StopKind (compile-time: assigning it here fails to build
    // if the union omits it).
    const kind: StopKind = "stopped";
    // Mutation guard: adding 'stopped' to HUMAN_STOP_KINDS would make a `failed` run
    // carrying it style as a neutral "stopped" pill — this asserts it stays danger.
    expect(isStoppedRun("failed", kind)).toBe(false);
    expect(runStatusTone("failed", kind)).toBe("danger");
    // A gracefully-stopped run lands status=completed, which is the success it is.
    expect(runStatusTone("completed", kind)).toBe("ok");
  });
});

describe("awaiting_input presentation (PRD #88)", () => {
  it("gets the SAME warn tone as the plan gate", () => {
    expect(runStatusTone("awaiting_input", null)).toBe("warning");
    expect(runStatusTone("awaiting_input", null)).toBe(runStatusTone("awaiting_approval", null));
  });

  it("has its own badge case rather than falling to the neutral default", () => {
    // The default arm hardcodes `neutral`, so without an explicit case a parked question
    // would render as a calm grey chip indistinguishable from `cancelled` — despite
    // runStatusTone above saying warning. The two are separate code paths.
    const badge = runBadge(run({ status: "awaiting_input" }), NOW);
    expect(badge).toEqual({ kind: "badge", label: "needs your answer", tone: "warning", pulse: false });
  });
});

describe("activeRunInHistory (issue-view start-run gate)", () => {
  it("is true when any run in the history is still non-terminal", () => {
    expect(activeRunInHistory([{ status: "completed" }, { status: "running" }])).toBe(true);
    expect(activeRunInHistory([{ status: "awaiting_approval" }])).toBe(true);
    expect(activeRunInHistory([{ status: "queued" }])).toBe(true);
  });
  it("is false when every run is terminal", () => {
    expect(activeRunInHistory([{ status: "completed" }, { status: "failed" }, { status: "cancelled" }])).toBe(
      false,
    );
  });
  it("is false for an empty history (first run allowed)", () => {
    expect(activeRunInHistory([])).toBe(false);
  });
});

// Issue #124 for the badge TOOLTIP. `RunBadge.title` is rendered as a `title` ATTRIBUTE at
// five sites, and an attribute is a sink `container.textContent` cannot see — so every #124
// render test in this repo is structurally blind to it. Asserted on the descriptor, which is
// where the fix lives and where one assertion covers all five renderers.
describe("runBadge title — format characters (#124)", () => {
  /** `RunBadge` is a union and the `mr` variant carries no title, so narrow rather than
   *  cast — a cast here would hide a real regression if `failed` ever stopped being a badge. */
  const titleOf = (reason: string | null): string | undefined => {
    const b = runBadge({ status: "failed", failure_reason: reason } as never, Date.now());
    if (b.kind !== "badge") throw new Error(`expected a badge for a failed run, got ${b.kind}`);
    return b.title;
  };

  it("strips them from failure_reason, which is worker-supplied and unstripped at ingest", () => {
    const title = titleOf("the repo \u202Eapproved\u200B this");
    expect(title ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(title).toBe("the repo approved this");
  });

  it("leaves an ordinary reason byte-identical", () => {
    const reason = "exit status 1: go test ./... failed";
    expect(titleOf(reason)).toBe(reason);
  });

  it("covers the OTHER badgeTitle call site — healthBadge's arm (#124)", () => {
    // `badgeTitle` has TWO call sites: `runBadge(run.failure_reason)` and
    // `healthBadge(run.health_reason)`. Every other case in this describe builds through
    // `runBadge`, so reverting healthBadge's call alone left them all green — a call site
    // with no assertion, which is the same shape as a channel with no assertion.
    const b = healthBadge(
      { health: "stalled", status: "running", health_since: null, health_reason: "no output for \u202E20m\u200B" } as never,
      Date.now(),
    );
    // Narrowed rather than cast, like titleOf above: `RunBadge` is a union whose `mr`
    // variant has no title, and a cast would hide a real regression here.
    if (b === null || b.kind !== "badge") throw new Error(`expected a badge for a stalled run, got ${b?.kind}`);
    expect(b.title ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(b.title).toBe("no output for 20m");
  });

  it("keeps an absent reason absent rather than turning it into an empty tooltip", () => {
    // `title=""` renders a real (empty) tooltip box in some engines, so a reason that is
    // nothing but format characters must collapse to undefined, not to "".
    expect(titleOf(null)).toBeUndefined();
    expect(titleOf("\u202E\u200B")).toBeUndefined();
  });
});
