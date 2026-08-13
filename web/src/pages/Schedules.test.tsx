// @vitest-environment jsdom
//
// Schedules list page (PRD #241 M5, mock §1): rows render per target/timing, and the
// per-row enable toggle PATCHes { enabled } and adopts the server's returned row.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Schedules } from "./Schedules";
import { api, type LastFire, type Schedule } from "../lib/api";

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
    last_fire: null,
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

// ── PRD #308 M4: the enriched "Last run" cell + "Last fire" detail row ─────────
const NOW = new Date().toISOString();

function fire(over: Partial<LastFire>): LastFire {
  return { fired_at: NOW, matched: 0, capped: false, started: [], skips: [], ...over };
}

// only1 renders a single schedule so the "Last fire" disclosure and its panel are
// unambiguous (one button, one detail panel).
function only1(over: Partial<Schedule>) {
  mockApi.listSchedules.mockResolvedValue([sched({ id: "s1", ...over })]);
}

describe("Schedules — fire outcomes (PRD #308 M4)", () => {
  it("started: a green badge, expandable to the runs it started with their run ids", async () => {
    only1({
      target: "issue",
      issue_iid: 142,
      last_fire: fire({
        matched: 1,
        started: [{ issue_iid: 142, run_id: "3f1a2b7c-dead-beef-0000-000000000000", title: "Fix the thing" }],
      }),
    });
    renderPage();
    // The list cell shows the green outcome badge before expansion.
    await waitFor(() => expect(screen.getByText("1 started")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Last fire" }));
    // Expanded: the started run is listed with its issue ref and a run chip (truncated id).
    expect(screen.getByText("Fix the thing")).toBeTruthy();
    const chip = screen.getByText(/run 3f1a/);
    expect(chip.getAttribute("href")).toBe("/runs/3f1a2b7c-dead-beef-0000-000000000000");
  });

  it("started-nothing: an amber '0 started · 1 skipped' badge, expandable to the skip + its reason", async () => {
    only1({
      target: "sweep",
      max_issues: 5,
      last_fire: fire({
        matched: 1,
        capped: false,
        skips: [{ issue_iid: 96, title: "Worker restart drops commits", reason: "no_prd_link" }],
      }),
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("0 started · 1 skipped")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Last fire" }));
    expect(screen.getByText("Worker restart drops commits")).toBeTruthy();
    // The typed reason renders as its human label, never the raw sentinel.
    expect(screen.getByText("no PRD link")).toBeTruthy();
    expect(screen.queryByText("no_prd_link")).toBeNull();
  });

  it("empty-label: a matched-0 sweep reads as a neutral 'matched 0', not an error", async () => {
    only1({ target: "sweep", last_fire: fire({ matched: 0 }) });
    renderPage();
    await waitFor(() => expect(screen.getByText("matched 0")).toBeTruthy());
  });

  it("never-fired: shows the dash and offers no disclosure", async () => {
    only1({ last_fired_at: null, last_fire: null });
    renderPage();
    await waitFor(() => expect(screen.getByText("— never fired")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Last fire" })).toBeNull();
  });

  it("parked: an error schedule with a prior last_fire still shows that fire", async () => {
    only1({
      status: "error",
      last_fire: fire({
        matched: 1,
        started: [{ issue_iid: 173, run_id: "aaaa1111-0000-0000-0000-000000000000", title: "prior run" }],
      }),
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("1 started")).toBeTruthy());
  });

  it("parked: an error schedule with no fire at all shows the dash", async () => {
    only1({ status: "error", last_fired_at: null, last_fire: null });
    renderPage();
    await waitFor(() => expect(screen.getByText("— never fired")).toBeTruthy());
  });
});

describe("Schedules — the cap hint (PRD #308 M4, Goal 2)", () => {
  const hintText = "Nothing newer was reached.";

  it("renders when capped && skipped>0 && started===0", async () => {
    only1({
      target: "sweep",
      max_issues: 1,
      last_fire: fire({
        matched: 1,
        capped: true,
        skips: [{ issue_iid: 96, title: "candidate", reason: "no_prd_link" }],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    expect(screen.getByText(hintText)).toBeTruthy();
  });

  it("does NOT render when capped but something started", async () => {
    only1({
      target: "sweep",
      max_issues: 1,
      last_fire: fire({
        matched: 2,
        capped: true,
        started: [{ issue_iid: 90, run_id: "bbbb2222-0000-0000-0000-000000000000", title: "started one" }],
        skips: [{ issue_iid: 96, title: "candidate", reason: "no_prd_link" }],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    expect(screen.queryByText(hintText)).toBeNull();
  });

  it("does NOT render when skips exist but the fire was not capped", async () => {
    only1({
      target: "sweep",
      max_issues: 10,
      last_fire: fire({
        matched: 1,
        capped: false,
        skips: [{ issue_iid: 96, title: "candidate", reason: "no_prd_link" }],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    expect(screen.queryByText(hintText)).toBeNull();
  });
});
