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
} from "./runBadge";
import type { LatestRun, RunStatus } from "./api";

// run builds a LatestRun with sane defaults, overridable per test.
function run(over: Partial<LatestRun> = {}): LatestRun {
  return {
    id: "run-1",
    status: "queued",
    mr_iid: null,
    failure_reason: null,
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
  it("queued → neutral 'queued'", () => {
    expect(runBadge(run({ status: "queued" }), NOW)).toEqual({
      kind: "badge",
      label: "queued",
      tone: "neutral",
      pulse: false,
    });
  });

  it("claimed → neutral 'claimed'", () => {
    const b = runBadge(run({ status: "claimed" }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "claimed", tone: "neutral", pulse: false });
  });

  it("running → pulsing badge with elapsed since created_at", () => {
    const b = runBadge(run({ status: "running", worker_name: "laptop" }), NOW);
    expect(b).toMatchObject({ kind: "badge", tone: "neutral", pulse: true });
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

  it("failed with a known server stop reason → 'stopped', never danger", () => {
    const b = runBadge(run({ status: "failed", failure_reason: "run cancelled" }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "stopped", tone: "neutral" });
    const r = runBadge(run({ status: "failed", failure_reason: "plan rejected" }), NOW);
    expect(r).toMatchObject({ kind: "badge", label: "stopped", tone: "neutral" });
  });

  it("completed with an MR → MR chip", () => {
    expect(runBadge(run({ status: "completed", mr_iid: 42 }), NOW)).toEqual({
      kind: "mr",
      mrIid: 42,
    });
  });

  it("completed without an MR → plain 'completed' badge (never invisible)", () => {
    const b = runBadge(run({ status: "completed", mr_iid: null }), NOW);
    expect(b).toMatchObject({ kind: "badge", label: "completed", tone: "neutral" });
  });
});

describe("isStoppedRun (exact server stop reasons)", () => {
  it("true for cancelled status", () => {
    expect(isStoppedRun("cancelled", null)).toBe(true);
  });
  it("true for failed with an exact server stop reason", () => {
    expect(isStoppedRun("failed", "run cancelled")).toBe(true);
    expect(isStoppedRun("failed", "plan rejected")).toBe(true);
  });
  it("false for a failed reason that merely contains 'cancel' (no false stop)", () => {
    // An arbitrary agent error containing the word must NOT masquerade as a
    // deliberate stop — only the exact server literals do.
    expect(isStoppedRun("failed", "Cancelled by poller")).toBe(false);
    expect(isStoppedRun("failed", "operation cancel failed")).toBe(false);
    expect(isStoppedRun("failed", "run cancelled by user")).toBe(false);
  });
  it("false for a genuine failure", () => {
    expect(isStoppedRun("failed", "compile error")).toBe(false);
    expect(isStoppedRun("failed", null)).toBe(false);
  });
  it("false for non-terminal / completed", () => {
    expect(isStoppedRun("completed", null)).toBe(false);
    expect(isStoppedRun("running", null)).toBe(false);
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
