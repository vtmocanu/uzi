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
    const checks = screen.getByText("1/1 cross-check");
    expect(runs).not.toBe(checks);
    expect(runs.title).toBe("Running 1 of 1 run slots");
    expect(checks.title).toBe("Running 1 of 1 cross-check slots");
  });

  it.each([null, 0, undefined])("keeps legacy display for cross-check cap %s", (cap) => {
    render(<WorkerRunBadge worker={{ ...worker, max_cross_check_slots: cap as number | null }} />);
    expect(screen.getByText("busy")).toBeTruthy();
    expect(screen.getByText(`1/${cap ?? "?"} cross-check`)).toBeTruthy();
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
    expect(screen.getByText(`${activity.active_cross_checks}/1 ${activity.active_cross_checks === 1 ? "cross-check" : "cross-checks"}`)).toBeTruthy();
    expect(screen.queryByText("busy")).toBeNull();
  });

  it("shows idle capacity without labeling a cross-check-only worker busy", () => {
    render(<WorkerRunBadge worker={{ ...worker, active_runs: 0 }} />);
    expect(screen.getByText("0/1 runs")).toBeTruthy();
    expect(screen.getByText("1/1 cross-check")).toBeTruthy();
    expect(screen.queryByText("busy")).toBeNull();
  });
});

describe("WorkerRunBadge idle visibility", () => {
  it.each([undefined, false])("shows idle checks with hideIdleCrossChecks=%s", (hideIdleCrossChecks) => {
    render(<WorkerRunBadge worker={{ ...worker, busy: false, active_cross_checks: 0 }} hideIdleCrossChecks={hideIdleCrossChecks} />);
    const checks = screen.getByText("0/1 cross-checks");
    expect(checks.title).toBe("Running 0 of 1 cross-check slots");
    expect(checks.className).toContain("text-neutral-fg");
  });
  it("hides idle checks while retaining an active control", () => {
    render(<>
      <WorkerRunBadge worker={{ ...worker, active_cross_checks: 0 }} hideIdleCrossChecks />
      <WorkerRunBadge worker={worker} hideIdleCrossChecks />
    </>);
    expect(screen.getByTitle("Running 1 of 1 cross-check slots")).toBeTruthy();
    expect(screen.queryByText("0/1 cross-checks")).toBeNull();
  });
  it.each([1, 2])("shows %s active checks with idle hiding", (active) => {
    render(<WorkerRunBadge worker={{ ...worker, active_cross_checks: active, max_cross_check_slots: 2 }} hideIdleCrossChecks />);
    const checks = screen.getByText(active === 1 ? "1/2 cross-check" : "2/2 cross-checks");
    expect(checks.title).toBe(`Running ${active} of 2 cross-check slots`);
    expect(checks.className).toContain("text-warn");
  });
  it.each([null, undefined, 0])("keeps active checks visible with capacity %s and idle hiding", (cap) => {
    render(<WorkerRunBadge worker={{ ...worker, max_cross_check_slots: cap as number | null }} hideIdleCrossChecks />);
    const checks = screen.getByText(`1/${cap ?? "?"} cross-check`);
    expect(checks.className).toContain("text-warn");
    expect(checks.title).toBe(cap == null ? "Running 1 cross-checks; slot capacity is unadvertised" : "Running 1 of 0 cross-check slots");
    expect(screen.getByText("busy")).toBeTruthy();
  });
  it("retains run capacity and chat busy when hiding idle checks", () => {
    render(<WorkerRunBadge worker={{ ...worker, active_runs: 0, active_cross_checks: 0 }} hideIdleCrossChecks />);
    expect(screen.getByText("0/1 runs")).toBeTruthy();
    expect(screen.getByTitle("Chat active").textContent).toBe("busy");
    expect(screen.queryByText("0/1 cross-checks")).toBeNull();
  });
});
