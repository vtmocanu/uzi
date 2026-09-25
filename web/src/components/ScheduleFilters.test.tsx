// @vitest-environment jsdom
//
// ScheduleFilters (PRD #1645 D5): the source chips, the conditional Repo select and the
// removable Job chip. The page-level behaviour (filtering, counts, round-trips) is in
// Schedules.test.tsx; this file pins the bar's own contract.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { ScheduleFilters } from "./ScheduleFilters";

afterEach(cleanup);

const COUNTS = { all: 5, catalog: 2, mine: 3, paused: 1 };
const TWO_REPOS = [
  { id: "r1", path: "o/a" },
  { id: "r2", path: "o/b" },
];

function renderBar(over: Partial<Parameters<typeof ScheduleFilters>[0]> = {}) {
  const props = {
    source: "all" as const,
    counts: COUNTS,
    onSource: vi.fn(),
    repos: TWO_REPOS,
    repoId: null,
    onRepo: vi.fn(),
    jobName: null,
    onClearJob: vi.fn(),
    ...over,
  };
  render(<ScheduleFilters {...props} />);
  return props;
}

describe("ScheduleFilters", () => {
  it("renders the four chips as pressed-state buttons with their counts", () => {
    const p = renderBar({ source: "mine" });
    const mine = screen.getByRole("button", { name: /^Mine/ });
    expect(mine.getAttribute("aria-pressed")).toBe("true");
    expect(mine.textContent).toBe("Mine3");
    expect(screen.getByRole("button", { name: /^All/ }).getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: /^Paused/ }));
    expect(p.onSource).toHaveBeenCalledWith("paused");
  });

  it("offers the Repo select only for two or more repos; choosing 'All repos' clears it", () => {
    const p = renderBar({ repoId: "r2" });
    const select = screen.getByRole("combobox", { name: "Repo" }) as HTMLSelectElement;
    expect(select.value).toBe("r2");
    fireEvent.change(select, { target: { value: "" } });
    expect(p.onRepo).toHaveBeenCalledWith(null);
    cleanup();

    renderBar({ repos: [TWO_REPOS[0]] });
    expect(screen.getByRole("button", { name: /^All/ })).toBeTruthy(); // positive control
    expect(screen.queryByRole("combobox", { name: "Repo" })).toBeNull();
  });

  it("shows a removable Job chip only while a job filter is applied", () => {
    const p = renderBar({ jobName: "Bug triage sweep" });
    expect(screen.getByText(/Job: Bug triage sweep/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove the Job: Bug triage sweep filter" }));
    expect(p.onClearJob).toHaveBeenCalledTimes(1);
    cleanup();

    renderBar({ jobName: null });
    expect(screen.getByRole("button", { name: /^All/ })).toBeTruthy(); // positive control
    expect(screen.queryByText(/^Job:/)).toBeNull();
  });
});
