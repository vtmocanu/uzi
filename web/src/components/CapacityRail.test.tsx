// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CapacityRail } from "./CapacityRail";
import { aWorker } from "../pages/overviewFixtures";

afterEach(cleanup);

const withStats = { stats_cpu_pct: 34, stats_mem_bytes: 2147483648, stats_mem_limit_bytes: 4294967296, stats_source: "cgroup" };

function mount(workers: ReturnType<typeof aWorker>[]) {
  return render(
    <MemoryRouter>
      <CapacityRail workers={workers} />
    </MemoryRouter>,
  );
}

const fleet = () => [
  aWorker({ id: "a", name: "alpha", active_runs: 2, max_concurrent_runs: 2, ...withStats }),
  aWorker({ id: "b", name: "bravo", active_runs: 1, max_concurrent_runs: 2, draining_since: "2026-07-05T12:00:00Z", kind: "hosted", upgrade_status: "upgrading", upgrade_target: "v0.86.1" }),
  aWorker({ id: "c", name: "charlie", status: "offline", max_concurrent_runs: 2 }),
];

describe("CapacityRail", () => {
  it("states the headline, the legend and the exclusions", () => {
    const { container } = mount(fleet());
    expect(screen.getByRole("heading", { name: /Capacity/ })).toBeTruthy();
    expect(container.querySelector("strong")?.textContent).toBe("3 of 4");
    expect(container.textContent).toContain("run slots in use");
    expect(container.textContent).toContain("In use 3");
    expect(container.textContent).toContain("Closed 1");
    expect(container.textContent).toContain("Open 0");
    expect(container.textContent).toContain("Not counted: 1 offline");
    expect(screen.getByRole("link", { name: "Workers →" }).getAttribute("href")).toBe("/workers");
  });

  it("carries the advertised-capacity note and never promises a run", () => {
    const { container } = mount(fleet());
    expect(screen.getByText(/Open means advertised spare slots/)).toBeTruthy();
    expect(container.textContent).not.toMatch(/available|guaranteed|will run/i);
  });

  it("lists a worker with no stats, an offline worker and every other worker", () => {
    mount(fleet());
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
    expect(screen.getByText("bravo")).toBeTruthy();
    expect(screen.getByText("charlie")).toBeTruthy();
    expect(screen.getAllByText("No stats reported").length).toBe(2);
    expect(screen.getByText(/cpu 34%/)).toBeTruthy();
  });

  it("keeps a draining pill neutral and says it takes no new runs, with the roll note", () => {
    const { container } = mount(fleet());
    const pill = screen.getByText("draining");
    expect(pill.className).toContain("bg-neutral-surface");
    expect(container.textContent).toContain("Takes no new runs.");
    expect(container.textContent).toContain("Rolls to v0.86.1 once fully idle");
  });

  it("shows no roll note for a manual cordon alone, but still says it takes no new runs", () => {
    const { container } = mount([aWorker({ manual_cordoned_at: "2026-07-05T12:00:00Z", effective_cordoned: true, max_concurrent_runs: 1 })]);
    expect(container.textContent).toContain("Takes no new runs.");
    expect(container.textContent).not.toContain("Rolls to");
  });

  it("notes no-cap workers and over-cap runs", () => {
    const { container } = mount([aWorker({ id: "x", max_concurrent_runs: null }), aWorker({ id: "y", active_runs: 3, max_concurrent_runs: 2 })]);
    expect(container.textContent).toContain("Not counted: 1 with no advertised slot cap");
    expect(container.textContent).toContain("1 run over advertised caps");
  });

  it("draws proportional segments past twelve slots", () => {
    const { container } = mount([aWorker({ active_runs: 5, max_concurrent_runs: 20 })]);
    expect(container.querySelectorAll('[aria-hidden="true"][style], [aria-hidden="true"] > [style]').length).toBeGreaterThan(0);
  });

  it("sanitizes a hostile worker name", () => {
    mount([aWorker({ name: "evil‮​name" })]);
    expect(screen.getByText("evilname")).toBeTruthy();
  });

  it("shows an empty state with a link when there are no workers", () => {
    mount([]);
    expect(screen.getByText(/No workers yet\./)).toBeTruthy();
    expect(screen.getAllByRole("link").some((l) => l.getAttribute("href") === "/workers")).toBe(true);
  });
});
