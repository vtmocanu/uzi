import { describe, expect, it } from "vitest";
import { workerRunBadge, workerCrossCheckBadge } from "./workerRuns";

type BadgeInput = Parameters<typeof workerRunBadge>[0];

function w(over: Partial<BadgeInput> = {}): BadgeInput {
  return { busy: false, active_runs: 0, max_concurrent_runs: null, max_cross_check_slots: null, ...over };
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

describe("dedicated cross-check capacity", () => {
  it("shows cap-one runs when the dedicated lane is enabled", () => {
    expect(workerRunBadge(w({ busy: true, active_runs: 1, max_concurrent_runs: 1, max_cross_check_slots: 1 }))?.label).toBe("1/1 runs");
  });

  it("uses live run count for an unknown run cap with an enabled lane", () => {
    expect(workerRunBadge(w({ active_runs: 1, max_cross_check_slots: 1 }))?.label).toBe("1/1 runs");
    expect(workerRunBadge(w({ busy: true, max_cross_check_slots: 1 }))).toBeNull();
  });

  it.each([null, 0, undefined])("hides disabled or unknown cross-check capacity %s", (cap) => {
    expect(workerCrossCheckBadge({ active_cross_checks: 0, max_cross_check_slots: cap as number | null })).toBeNull();
  });

  it.each([null, 0])("keeps running checks visible after cap changes to %s", (cap) => {
    expect(workerCrossCheckBadge({ active_cross_checks: 1, max_cross_check_slots: cap })?.label)
      .toBe(`1/${cap ?? "?"} cross-checks`);
  });

  it("shows idle dedicated capacity in a neutral tone", () => {
    expect(workerCrossCheckBadge({ active_cross_checks: 0, max_cross_check_slots: 1 })).toEqual({
      label: "0/1 cross-checks", tone: "neutral", title: "Running 0 of 1 cross-check slots",
    });
  });

  it("keeps dedicated load separate from run load", () => {
    expect(workerCrossCheckBadge({ active_cross_checks: 2, max_cross_check_slots: 3 })).toEqual({
      label: "2/3 cross-checks", tone: "warning", title: "Running 2 of 3 cross-check slots",
    });
  });
});
