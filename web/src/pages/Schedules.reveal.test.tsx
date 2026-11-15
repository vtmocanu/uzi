// @vitest-environment jsdom
//
// Issue #4373: `/schedules?tab=schedules#schedule-name-<encodeURIComponent(id)>` reveals the
// schedule's row on direct entry and on in-app navigation, once per location.key.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";
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
      getSchedulePause: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);
const EMPTY_CATALOG = { entries: [], enablements: [] };
const TRICKY_ID = "a b/c#d";

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
    remove_label_on_dispatch: false,
    capacity_limit: null,
    capacity_room_needed: null,
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

const linkFor = (id: string) => `/schedules?tab=schedules#schedule-name-${encodeURIComponent(id)}`;
const nameEl = (id: string) => document.getElementById(`schedule-name-${id}`);

// A test-only control that performs an in-app navigation to `to`.
function Nav({ to }: { to: string }) {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(to)}>
      go
    </button>
  );
}

function renderAt(entry: string, navTo?: string) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      {navTo && <Nav to={navTo} />}
      <Schedules />
    </MemoryRouter>,
  );
}

let scrollSpy: ReturnType<typeof vi.fn>;

beforeEach(() => {
  scrollSpy = vi.fn();
  HTMLElement.prototype.scrollIntoView = scrollSpy as unknown as typeof HTMLElement.prototype.scrollIntoView;
  mockApi.listScheduleCatalog.mockResolvedValue(EMPTY_CATALOG);
  mockApi.listRepos.mockResolvedValue({ repos: [] } as Awaited<ReturnType<typeof api.listRepos>>);
  mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: null });
  mockApi.listSchedules.mockResolvedValue([
    sched({ id: "s1" }),
    sched({ id: TRICKY_ID, repo_path: "vtmocanu/other" }),
  ]);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Schedules deep-link reveal (#4373)", () => {
  it("direct entry scrolls to, focuses and highlights the row after load", async () => {
    renderAt(linkFor("s1"));
    await waitFor(() => expect(document.activeElement).toBe(nameEl("s1")));
    expect(nameEl("s1")?.getAttribute("data-revealed")).toBe("true");
    expect(scrollSpy.mock.contexts).toContain(nameEl("s1"));
  });

  it("in-app navigation to the link reveals the row on the mounted page", async () => {
    renderAt("/schedules?tab=schedules", linkFor("s1"));
    await screen.findByText("vtmocanu/other");
    expect(document.activeElement).not.toBe(nameEl("s1"));
    fireEvent.click(screen.getByRole("button", { name: "go" }));
    await waitFor(() => expect(document.activeElement).toBe(nameEl("s1")));
    expect(nameEl("s1")?.getAttribute("data-revealed")).toBe("true");
  });

  it("navigating in from the Job catalog tab switches to the list and reveals", async () => {
    renderAt("/schedules?tab=catalog", linkFor("s1"));
    await screen.findByRole("tab", { name: /Job catalog/ });
    fireEvent.click(screen.getByRole("button", { name: "go" }));
    await waitFor(() => expect(document.activeElement).toBe(nameEl("s1")));
  });

  it("round-trips an id with reserved characters", async () => {
    renderAt(linkFor(TRICKY_ID));
    await waitFor(() => expect(document.activeElement).toBe(nameEl(TRICKY_ID)));
    expect(nameEl(TRICKY_ID)?.getAttribute("data-revealed")).toBe("true");
  });

  it("an unknown id is a no-op: no scroll and focus unchanged", async () => {
    const before = document.activeElement;
    renderAt(linkFor("nope"));
    await screen.findByText("vtmocanu/other");
    await act(async () => {});
    expect(scrollSpy).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(before);
    expect(document.querySelector("[data-revealed]")).toBeNull();
    expect(document.activeElement).not.toBe(screen.getByRole("tab", { name: /Schedules/ }));
  });

  it("a malformed escape is a no-op", async () => {
    const before = document.activeElement;
    renderAt("/schedules?tab=schedules#schedule-name-%E0%A4%A");
    await screen.findByText("vtmocanu/other");
    await act(async () => {});
    expect(scrollSpy).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(before);
  });

  it("blur clears the highlight", async () => {
    renderAt(linkFor("s1"));
    await waitFor(() => expect(nameEl("s1")?.getAttribute("data-revealed")).toBe("true"));
    act(() => nameEl("s1")?.blur());
    await waitFor(() => expect(nameEl("s1")?.hasAttribute("data-revealed")).toBe(false));
  });

  it("a data reload does not re-reveal", async () => {
    mockApi.updateSchedule.mockImplementation(async (id) => sched({ id }));
    renderAt(linkFor("s1"));
    await waitFor(() => expect(document.activeElement).toBe(nameEl("s1")));
    expect(scrollSpy).toHaveBeenCalledTimes(1);
    act(() => nameEl("s1")?.blur());
    const loads = mockApi.listSchedules.mock.calls.length;
    // Toggling a row reloads the list (the same re-render path a data refresh takes).
    fireEvent.click(screen.getByRole("switch", { name: /Pause Sweep eligible issues on vtmocanu\/other/ }));
    await waitFor(() => expect(mockApi.listSchedules.mock.calls.length).toBeGreaterThan(loads));
    await act(async () => {});
    expect(scrollSpy).toHaveBeenCalledTimes(1);
    expect(document.activeElement).not.toBe(nameEl("s1"));
    expect(nameEl("s1")?.hasAttribute("data-revealed")).toBe(false);
  });
});
