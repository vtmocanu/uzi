import { describe, it, expect } from "vitest";
import {
  activeRunInHistory,
  canOpenRunView,
  formatElapsed,
  hasActiveRun,
  healthBadge,
  healthFlagLabel,
  isAwaitingApproval,
  isHealthFlaggableStatus,
  isStoppedRun,
  mrChipState,
  mrChipSuffix,
  mrChipTitle,
  retryHint,
  runBadge,
  runStatusTone,
  shouldShowHealthFlag,
} from "./runBadge";
import type { LatestRun, RunStatus } from "./api";

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
    owner_name: "Vlad",
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

  it("running → pulsing info badge with elapsed since created_at", () => {
    const b = runBadge(run({ status: "running", worker_name: "laptop" }), NOW);
    expect(b).toMatchObject({ kind: "badge", tone: "info", pulse: true });
    if (b.kind === "badge") expect(b.label).toBe("running 4m");
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
    if (b.kind === "badge") expect(b.label).toBe("running 4m");
  });

  it("stamped stop_kind but completed with an MR → MR chip (reject-then-approve race)", () => {
    expect(runBadge(run({ status: "completed", stop_kind: "plan_rejected", mr_iid: 42 }), NOW)).toEqual({
      kind: "mr",
      mrIid: 42,
      mrState: "open",
    });
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
    expect(b).toMatchObject({ label: "running 4m", tone: "info" });
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
  it("title scopes merged/closed to 'as of last sync' with the per-forge noun", () => {
    expect(mrChipTitle("merged", "gitlab")).toBe("Merge request merged (as of last sync)");
    expect(mrChipTitle("closed", "gitlab")).toBe("Merge request closed unmerged (as of last sync)");
    expect(mrChipTitle("open", "gitlab")).toBe("Open the merge request");
    // Forgejo says "pull request", and the open title no longer names a platform.
    expect(mrChipTitle("merged", "forgejo")).toBe("Pull request merged (as of last sync)");
    expect(mrChipTitle("open", "forgejo")).toBe("Open the pull request");
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
