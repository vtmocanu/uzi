import { describe, expect, it } from "vitest";
import { workerRunBadge } from "./workerRuns";

type BadgeInput = Parameters<typeof workerRunBadge>[0];

function w(over: Partial<BadgeInput> = {}): BadgeInput {
  return { busy: false, active_runs: 0, max_concurrent_runs: null, ...over };
}

describe("workerRunBadge", () => {
  it("renders nothing for an idle, un-capped worker", () => {
    expect(workerRunBadge(w())).toBeNull();
  });

  it("keeps the legacy 'busy' pill for a single run and no cap above 1", () => {
    const badge = workerRunBadge(w({ busy: true, active_runs: 1 }));
    expect(badge).toEqual({ label: "busy", tone: "warning", title: "Holds an active run" });
  });

  it("keeps the legacy 'busy' pill for a single run against a cap of 1", () => {
    const badge = workerRunBadge(w({ busy: true, active_runs: 1, max_concurrent_runs: 1 }));
    expect(badge?.label).toBe("busy");
  });

  it("shows 'N/M runs' for two active runs against a cap of two (the PRD validation case)", () => {
    const badge = workerRunBadge(w({ busy: true, active_runs: 2, max_concurrent_runs: 2 }));
    expect(badge).toEqual({
      label: "2/2 runs",
      tone: "warning",
      title: "Running 2 of 2 run slots",
    });
  });

  it("shows 'N/M runs' for an idle worker that advertises a cap above 1, in a calm tone", () => {
    const badge = workerRunBadge(w({ busy: false, active_runs: 0, max_concurrent_runs: 2 }));
    expect(badge).toEqual({
      label: "0/2 runs",
      tone: "neutral",
      title: "Running 0 of 2 run slots",
    });
  });

  it("shows one slot used of an advertised cap", () => {
    const badge = workerRunBadge(w({ busy: true, active_runs: 1, max_concurrent_runs: 3 }));
    expect(badge?.label).toBe("1/3 runs");
    expect(badge?.tone).toBe("warning");
  });

  it("falls back to the active count when a multi-run worker advertises no cap", () => {
    // Defensive: an un-capped worker should be serial, but if it ever reports >1
    // active runs the badge must not print "N/null".
    const badge = workerRunBadge(w({ busy: true, active_runs: 2, max_concurrent_runs: null }));
    expect(badge).toEqual({
      label: "2/2 runs",
      tone: "warning",
      title: "Running 2 concurrent runs",
    });
  });
});
