import { describe, it, expect } from "vitest";
import {
  activeRunInHistory,
  canOpenRunView,
  formatElapsed,
  hasActiveRun,
  isAwaitingApproval,
  isStoppedRun,
  retryHint,
  runBadge,
  runStatusTone,
} from "./runBadge";
import type { LatestRun, RunStatus } from "./api";

// run builds a LatestRun with sane defaults, overridable per test.
function run(over: Partial<LatestRun> = {}): LatestRun {
  return {
    id: "run-1",
    status: "queued",
    mr_iid: null,
    failure_reason: null,
    stop_kind: null,
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

  it("completed with an MR → MR chip", () => {
    expect(runBadge(run({ status: "completed", mr_iid: 42 }), NOW)).toEqual({
      kind: "mr",
      mrIid: 42,
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
    });
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
