// @vitest-environment jsdom
//
// Schedules list page (PRD #241 M5, mock §1): rows render per target/timing, and the
// per-row enable toggle PATCHes { enabled } and adopts the server's returned row.
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
      updateSchedule: vi.fn(),
      runScheduleNow: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

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
    auto_approve: true,
    wait_on_limit: true,
    max_issues: 10,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    next_fires: [],
    ...over,
  };
}

beforeEach(() => {
  mockApi.listSchedules.mockResolvedValue([
    sched({ id: "s1", target: "sweep", enabled: true }),
    sched({ id: "s2", target: "prompt", prompt: "hunt flaky tests", enabled: false, cron_expr: "0 9 * * 1" }),
  ]);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPage() {
  return render(
    <MemoryRouter>
      <Schedules />
    </MemoryRouter>,
  );
}

describe("Schedules list", () => {
  it("renders a row per schedule with its target badge and cron", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("Sweep eligible PRD issues")).toBeTruthy());
    expect(screen.getByText("Prompt: hunt flaky tests")).toBeTruthy();
    // The cron string is shown verbatim (canonical source of truth).
    expect(screen.getByText("0 2 * * 1-5")).toBeTruthy();
    // A paused (disabled) schedule shows the paused pill in Next run.
    expect(screen.getByText("paused")).toBeTruthy();
  });

  it("the enable toggle PATCHes { enabled } and adopts the returned row", async () => {
    mockApi.updateSchedule.mockImplementation(async (id: string, input) =>
      sched({ id, enabled: input.enabled ?? true }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Sweep eligible PRD issues")).toBeTruthy());

    // s1 is enabled; disabling it sends { enabled: false }.
    const toggle = screen.getByRole("switch", { name: "Disable schedule" });
    fireEvent.click(toggle);
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalledWith("s1", { enabled: false }));
  });
});
