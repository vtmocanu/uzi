// @vitest-environment jsdom
//
// Issue #961 (m2) characterization tests for two Schedules fixes that the main
// Schedules.test.tsx harness cannot cover cleanly:
//   - item 4c: a mutation error (a failed toggle) must clear on `onSaved → reload()`,
//     the ONE reload path that does not clear the error itself. Driving it needs
//     ScheduleModal's onSaved, so this file mocks ScheduleModal down to a button that
//     invokes it (isolating that mock from the main suite).
//   - item 12: on a FIRST-load failure both schedules and catalog stay null, so the
//     render hits the skeleton branch; the error Alert must still be reachable there.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Schedules } from "./Schedules";
import { api, type Schedule } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSchedules: vi.fn(),
      listScheduleCatalog: vi.fn(),
      listRepos: vi.fn(),
      updateSchedule: vi.fn(),
      runScheduleNow: vi.fn(),
      cloneSchedule: vi.fn(),
      resetSchedule: vi.fn(),
      enableCatalogSchedule: vi.fn(),
      deleteSchedule: vi.fn(),
      addScheduleRepo: vi.fn(),
      // PRD #1093: the page fetches the pause state alongside the list.
      getSchedulePause: vi.fn(),
    },
  };
});

// Reduce ScheduleModal to a button that invokes onSaved, so item 4c can trigger the
// `onSaved → reload()` path without driving the real modal form. importOriginal keeps
// relativeFromNow (Schedules imports it from the same module) real.
vi.mock("../components/ScheduleModal", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../components/ScheduleModal")>();
  return {
    ...actual,
    ScheduleModal: ({ onSaved }: { onSaved: () => void }) => (
      <button type="button" onClick={onSaved}>
        modal-saved
      </button>
    ),
  };
});

const mockApi = vi.mocked(api);
const EMPTY_CATALOG = { entries: [], enablements: [] };

function sched(over: Partial<Schedule>): Schedule {
  return {
    id: "s1",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    target: "sweep",
    issue_iid: null,
    labels: null,
    prompt: "",
    timing: "recurring",
    cron_expr: "0 2 * * 1-5",
    run_at: null,
    timezone: "Europe/Bucharest",
    next_fire_at: new Date(Date.now() + 3_600_000).toISOString(),
    last_fired_at: null,
    last_fire: null,
    auto_approve: true,
    wait_on_limit: true,
    max_issues: 10,
    guidance: null,
    baked_guidance: null,
    model: null,
    output_mode: null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "user",
    catalog_slug: null,
    customized: false,
    sibling_group_id: null,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    next_fires: [],
    ...over,
  };
}

beforeEach(() => {
  mockApi.listScheduleCatalog.mockResolvedValue(EMPTY_CATALOG);
  mockApi.listRepos.mockResolvedValue({ repos: [] } as Awaited<ReturnType<typeof api.listRepos>>);
  mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: null });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// item 4c. A failed toggle sets the mutation `error` slot. onSaved → reload() is the only
// reload path that does NOT independently call setError(""), so it relies on the fetcher's
// onFetchStart to clear the stale error. Mutation-checked: removing the onFetchStart opt
// leaves "Could not update the schedule" shown after the reload.
describe("Schedules — a mutation error clears on onSaved → reload() (item 4c)", () => {
  it("wipes the failed-toggle error after the modal saves", async () => {
    mockApi.listSchedules.mockResolvedValue([sched({ id: "s1", target: "sweep", enabled: true })]);
    mockApi.updateSchedule.mockRejectedValue(new Error("nope"));

    render(
      <MemoryRouter>
        <Schedules />
      </MemoryRouter>,
    );
    fireEvent.click(screen.getByRole("tab", { name: /My schedules/ }));
    await waitFor(() => expect(screen.getByText("Sweep eligible issues")).toBeTruthy());

    // Fail the toggle → the mutation error is shown.
    fireEvent.click(screen.getByRole("switch", { name: "Disable schedule" }));
    await waitFor(() => expect(screen.getByText("Could not update the schedule")).toBeTruthy());

    // Open the (mocked) New-schedule modal and save: onSaved → reload() → onFetchStart clears.
    fireEvent.click(screen.getByRole("button", { name: /New schedule/ }));
    fireEvent.click(screen.getByRole("button", { name: "modal-saved" }));
    await waitFor(() => expect(screen.queryByText("Could not update the schedule")).toBeNull());
  });
});

// item 12. On a first-load failure both schedules and catalog stay null, so the render
// hits the null/skeleton branch and the two loaded branches' error Alert is unreachable.
// The fix renders the error Alert ABOVE the ListSkeleton in that branch. Mutation-checked:
// reverting the branch back to a bare <ListSkeleton> leaves the error unreachable (the
// "Could not load schedules" text is never found).
describe("Schedules — first-load error is reachable in the skeleton branch (item 12)", () => {
  it("shows the load error above the skeleton when the first load fails", async () => {
    mockApi.listSchedules.mockRejectedValue(new Error("down"));

    render(
      <MemoryRouter>
        <Schedules />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Could not load schedules")).toBeTruthy());
  });
});
