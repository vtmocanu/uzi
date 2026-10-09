// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { WorkerRunBadge } from "./WorkerRunBadge";

afterEach(cleanup);

const worker = {
  busy: true, active_runs: 1, max_concurrent_runs: 1,
  active_cross_checks: 1, max_cross_check_slots: 1,
};

describe("WorkerRunBadge cross-check lane", () => {
  it("renders independent cap-one run and cross-check badges", () => {
    render(<WorkerRunBadge worker={worker} />);
    const runs = screen.getByText("1/1 runs");
    const checks = screen.getByText("1/1 cross-checks");
    expect(runs).not.toBe(checks);
    expect(runs.title).toBe("Running 1 of 1 run slots");
    expect(checks.title).toBe("Running 1 of 1 cross-check slots");
  });

  it.each([null, 0, undefined])("keeps legacy display for cross-check cap %s", (cap) => {
    render(<WorkerRunBadge worker={{ ...worker, max_cross_check_slots: cap as number | null }} />);
    expect(screen.getByText("busy")).toBeTruthy();
    expect(screen.getByText(`1/${cap ?? "?"} cross-checks`)).toBeTruthy();
  });

  it("shows busy alongside both capacity badges for uncounted chat activity", () => {
    render(<WorkerRunBadge worker={{ ...worker, active_runs: 0, active_cross_checks: 0 }} />);
    expect(screen.getByText("0/1 runs")).toBeTruthy();
    expect(screen.getByText("0/1 cross-checks")).toBeTruthy();
    expect(screen.getByText("busy")).toBeTruthy();
  });

  it.each([
    { busy: true, active_runs: 1, active_cross_checks: 0 },
    { busy: true, active_runs: 0, active_cross_checks: 1 },
    { busy: false, active_runs: 0, active_cross_checks: 0 },
  ])("does not add busy for counted lane activity or idle workers: %s", (activity) => {
    render(<WorkerRunBadge worker={{ ...worker, ...activity }} />);
    expect(screen.getByText(`${activity.active_runs}/1 runs`)).toBeTruthy();
    expect(screen.getByText(`${activity.active_cross_checks}/1 cross-checks`)).toBeTruthy();
    expect(screen.queryByText("busy")).toBeNull();
  });

  it("shows idle capacity without labeling a cross-check-only worker busy", () => {
    render(<WorkerRunBadge worker={{ ...worker, active_runs: 0 }} />);
    expect(screen.getByText("0/1 runs")).toBeTruthy();
    expect(screen.getByText("1/1 cross-checks")).toBeTruthy();
    expect(screen.queryByText("busy")).toBeNull();
  });
});
