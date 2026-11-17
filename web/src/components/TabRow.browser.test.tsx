/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";
import { AppShell } from "./AppShell";
import { AdminShell } from "./AdminShell";
import { SettingsShell } from "./SettingsShell";
import { RunsLayout, RunsList } from "../pages/RunsList";
import { Schedules } from "../pages/Schedules";
import { WorkersSettings } from "../pages/WorkersSettings";
import { Findings } from "../pages/Findings";
import { Board } from "../pages/Board";
import {
  api,
  type Board as BoardData,
  type Card,
  type IncidentalFinding,
  type Schedule,
} from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { setupAppShellMocks } from "../test-appshell-mocks";
import { aRun, aWorker } from "../pages/overviewFixtures";
import { degradedDoc } from "../mocks/data";

// #4419: the seven tab rows share one TabRow, and the page-level ones pin below the app
// bar. Layout, sticky and scroll-padding need a real engine, so this runs in Chromium.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  const { appShellApiMocks } = await import("../test-appshell-mocks");
  return {
    ...actual,
    api: {
      ...appShellApiMocks(),
      adminListWorkers: vi.fn(),
      listScheduleCatalog: vi.fn(),
      getSchedulePause: vi.fn(),
      previewSchedule: vi.fn(),
      checkRepoLabels: vi.fn(),
      hostedConfig: vi.fn(),
      listFindings: vi.fn(),
      getBoard: vi.fn(),
      getBoardPrefs: vi.fn(),
      setBoardPrefs: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../auth/AuthContext")>()),
  useAuth: vi.fn(),
}));

const mockApi = vi.mocked(api);

const aSchedule = (i: number): Schedule =>
  ({
    id: `s${i}`,
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    target: "prompt",
    issue_iid: null,
    labels: null,
    prompt: `scheduled prompt number ${i}`,
    timing: "recurring",
    cron_expr: "0 2 * * 1-5",
    run_at: null,
    timezone: "Europe/Bucharest",
    next_fire_at: new Date(Date.now() + 3_600_000 + i * 60_000).toISOString(),
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
  }) as Schedule;

const aFinding = (i: number): IncidentalFinding => ({
  disposition_id: `disp-${i}`,
  finding_id: `find-${i}`,
  location: `api/internal/file${i}.go#fn${i}`,
  repo_id: "repo-uzi",
  repo_path: "vtmocanu/uzi",
  status: "open",
  last_title: `Finding number ${i}`,
  seen_in_runs: 2,
});

const aCard = (iid: number): Card =>
  ({
    iid,
    title: `Issue number ${iid} with a title long enough to wrap onto a second line`,
    state: "opened",
    labels: ["uzi", "Planned"],
    assignee_ids: [],
    web_url: `https://forge.example.com/g/r/-/issues/${iid}`,
    forge_type: "gitlab",
    author: "someone",
    has_prd_link: true,
    column: "Planned",
    closed: false,
    conflict: false,
    latest_run: null,
    pipeline: null,
    forge_updated_at: "2026-01-01T00:00:00Z",
  }) as Card;

const aBoard = (): BoardData => ({
  repo_id: "repo-1",
  path_with_namespace: "grp/proj",
  web_url: "https://forge.example.com/grp/proj",
  forge_type: "gitlab",
  columns: [{ label_name: "Planned" }, { label_name: "Human Review" }] as BoardData["columns"],
  cards: Array.from({ length: 30 }, (_, i) => aCard(i + 1)),
  pipeline: null,
  bot_forge_user_id: 0,
});

const range = (n: number) => Array.from({ length: n }, (_, i) => i + 1);

beforeEach(() => {
  localStorage.clear();
  setupAppShellMocks(api, useAuth);
  mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
  mockApi.listRuns.mockResolvedValue({
    runs: range(40).map((i) => aRun({ id: `run-${i}`, status: "running", issue_title: `Run number ${i}` })),
  });
  mockApi.listWorkers.mockResolvedValue({
    workers: range(30).map((i) => aWorker({ id: `w${i}`, name: `worker-${i}` })),
  });
  mockApi.listSchedules.mockResolvedValue(range(30).map(aSchedule));
  mockApi.listScheduleCatalog.mockResolvedValue({ entries: [], enablements: [] });
  mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: null });
  mockApi.previewSchedule.mockResolvedValue({ fires: [] });
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
  mockApi.listFindings.mockResolvedValue({
    bucket: "to_file",
    repo: "",
    run: "",
    open_count: 60,
    findings: range(60).map(aFinding),
  });
  mockApi.getFindingsStats.mockResolvedValue({ total: 60, todo: 60, filed: 0, done: 0, dismissed: 0, false_positives: 0 });
  mockApi.getBoard.mockResolvedValue({ board: aBoard() });
  mockApi.getBoardPrefs.mockResolvedValue({ extra_labels: null, show_all: false });
  mockApi.setBoardPrefs.mockImplementation(async (_repoId, prefs) => prefs);
});

afterEach(() => {
  cleanup();
  document.documentElement.style.removeProperty("overflow-anchor");
  window.scrollTo(0, 0);
  localStorage.clear();
  vi.clearAllMocks();
});

const Tall = ({ children }: { children?: ReactNode }) => (
  <div style={{ height: 4000 }} data-tall="">
    {children}
  </div>
);

interface Scenario {
  name: string;
  path: string;
  /** The row's accessible hook once mounted. */
  ready: () => Promise<unknown>;
  routes: ReactNode;
}

const scenarios: Scenario[] = [
  {
    name: "Settings",
    path: "/settings",
    ready: () => screen.findByRole("link", { name: "Appearance" }),
    routes: (
      <Route
        path="/settings/*"
        element={
          <SettingsShell description="Tall child">
            <Tall />
          </SettingsShell>
        }
      />
    ),
  },
  {
    name: "Admin",
    path: "/admin/users",
    ready: () => screen.findByRole("link", { name: /^Health/ }),
    routes: (
      <Route
        path="/admin/*"
        element={
          <AdminShell description="Tall child">
            <Tall />
          </AdminShell>
        }
      />
    ),
  },
  {
    name: "Runs",
    path: "/runs",
    ready: () => screen.findByText("Run number 3"),
    routes: (
      <Route element={<RunsLayout />}>
        <Route path="/runs" element={<RunsList />} />
      </Route>
    ),
  },
  {
    name: "Schedules",
    path: "/schedules",
    ready: () => screen.findByRole("button", { name: /^Pause all/ }),
    routes: <Route path="/schedules" element={<Schedules />} />,
  },
  {
    name: "Workers",
    path: "/workers",
    ready: () => screen.findByText("worker-3"),
    routes: <Route path="/workers" element={<WorkersSettings />} />,
  },
  {
    name: "Findings",
    path: "/findings",
    ready: () => screen.findByText("Finding number 3"),
    routes: <Route path="/findings" element={<Findings />} />,
  },
];

const nextFrame = () => new Promise((r) => requestAnimationFrame(() => r(null)));

async function mount(s: Scenario, width: number, height = 700, after?: ReactNode) {
  await page.viewport(width, height);
  render(
    <MemoryRouter initialEntries={[s.path]}>
      <AppShell>
        <Routes>{s.routes}</Routes>
        {after}
      </AppShell>
    </MemoryRouter>,
  );
  await s.ready();
  await nextFrame();
}

const blockEl = () => document.querySelector<HTMLElement>("[data-pinned-block-root]")!;
// The mobile bar is the only `frame sticky` element; it is display:none from lg up.
const barEl = () => document.querySelector<HTMLElement>("div.frame.sticky")!;
const rowOf = (block: HTMLElement) => block.firstElementChild as HTMLElement;
// Content that follows the block in the page flow; its document top must not move when the
// block sticks.
const docTop = (el: Element) => el.getBoundingClientRect().top + window.scrollY;

async function scrollDeep(y = 900) {
  window.scrollTo(0, y);
  await waitFor(() => expect(window.scrollY).toBeGreaterThan(y - 5));
  await nextFrame();
  await nextFrame();
}

const expectedTop = (width: number) => (width >= 1024 ? 0 : barEl().getBoundingClientRect().bottom);

describe("tab rows do not grow a vertical scrollbar (#4419)", () => {
  for (const s of scenarios.filter((x) => ["Settings", "Admin", "Runs", "Schedules", "Workers"].includes(x.name))) {
    for (const width of [400, 1440]) {
      it(`${s.name} at ${width}px: the row fits its box and the active tab sits on the line`, async () => {
        await mount(s, width);
        const row = rowOf(blockEl());
        // Make the row's one non-tab control (Schedules' Pause all) taller than the tabs, as it
        // is at some font sizes; the tabs must stretch to the row so the underline stays on the line.
        const extra = screen.queryByRole("button", { name: /^Pause all/ });
        if (extra) extra.style.minHeight = "48px";
        await nextFrame();
        expect(row.scrollHeight, "scrollHeight").toBe(row.clientHeight);
        const active = row.querySelector<HTMLElement>('[aria-current="page"], [aria-selected="true"]')!;
        expect(active, "active tab").toBeTruthy();
        // Exact: a taller control in the row (Schedules' Pause all) must not lift the underline.
        expect(Math.abs(active.getBoundingClientRect().bottom - row.getBoundingClientRect().bottom)).toBeLessThanOrEqual(0.01);
      });
    }
  }
});

describe("page tab rows pin below the app bar (#4419)", () => {
  for (const s of scenarios) {
    for (const width of [1440, 400]) {
      it(`${s.name} at ${width}px pins, without moving content or overflowing sideways`, async () => {
        await mount(s, width);
        const block = blockEl();
        expect(block.dataset.stuck).toBe("false");
        const follower = block.nextElementSibling!;
        const before = docTop(follower);
        await scrollDeep();
        await waitFor(() => expect(block.dataset.stuck).toBe("true"));
        expect(Math.abs(block.getBoundingClientRect().top - expectedTop(width))).toBeLessThanOrEqual(0.5);
        expect(Math.abs(docTop(follower) - before)).toBeLessThanOrEqual(1);
        expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
        if (s.name === "Runs") {
          const r = block.getBoundingClientRect();
          const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
          expect(block.contains(hit), "block is painted above the run cards").toBe(true);
        }
        window.scrollTo(0, 0);
        await waitFor(() => expect(block.dataset.stuck).toBe("false"));
      });
    }
  }

  for (const width of [320, 640, 1024]) {
    it(`Settings at ${width}px has no horizontal overflow${width === 320 ? " and pins at the bar bottom" : ""}`, async () => {
      await mount(scenarios[0], width);
      await scrollDeep();
      expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
      if (width === 320) {
        expect(Math.abs(blockEl().getBoundingClientRect().top - barEl().getBoundingClientRect().bottom)).toBeLessThanOrEqual(0.5);
      }
    });
  }
});

describe("stuck detection and the app-bar offset (#4419)", () => {
  it("an instant jump from a partly visible block still reads as stuck (1440x420)", async () => {
    await page.viewport(1440, 420);
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <AppShell>
          <Routes>
            <Route
              path="/settings/*"
              element={
                <>
                  <div data-spacer="" />
                  <SettingsShell description="Tall child">
                    <Tall />
                  </SettingsShell>
                </>
              }
            />
          </Routes>
        </AppShell>
      </MemoryRouter>,
    );
    await screen.findByRole("link", { name: "Appearance" });
    const block = blockEl();
    const spacer = document.querySelector<HTMLElement>("[data-spacer]")!;
    // Straddle the viewport bottom: the block's top edge 20px above it.
    spacer.style.height = "0px";
    spacer.style.height = `${Math.max(0, 420 - 20 - block.getBoundingClientRect().top)}px`;
    await nextFrame();
    await nextFrame();
    const top = block.getBoundingClientRect().top;
    expect(top).toBeLessThan(420);
    expect(top + block.getBoundingClientRect().height).toBeGreaterThan(420);
    expect(block.dataset.stuck).toBe("false");
    window.scrollTo({ top: 2000, behavior: "instant" });
    await waitFor(() => expect(block.dataset.stuck).toBe("true"));
    window.scrollTo({ top: 0, behavior: "instant" });
    await waitFor(() => expect(block.dataset.stuck).toBe("false"));
  });

  it("follows the bar's real height when the bar mounts after sign-in (400px)", async () => {
    // The shell renders the public layout while signed out, so the bar (and its observer)
    // appear only after the auth value flips. Make the bar taller than the 53px default.
    const style = document.createElement("style");
    style.textContent = "div.frame.sticky { padding-top: 30px !important; }";
    document.head.appendChild(style);
    try {
      await page.viewport(400, 700);
      const user = vi.mocked(useAuth)();
      vi.mocked(useAuth).mockReturnValue({ ...user, user: null });
      const tree = (
        <MemoryRouter initialEntries={["/settings"]}>
          <AppShell>
            <Routes>{scenarios[0].routes}</Routes>
          </AppShell>
        </MemoryRouter>
      );
      const view = render(tree);
      await scenarios[0].ready();
      expect(document.querySelector("div.frame.sticky")).toBeNull();
      vi.mocked(useAuth).mockReturnValue(user);
      view.rerender(tree);
      await waitFor(() => expect(barEl()).toBeTruthy());
      const bar = barEl();
      expect(bar.getBoundingClientRect().height).toBeGreaterThan(60);
      await waitFor(() =>
        expect(document.documentElement.style.getPropertyValue("--app-bar-h")).toBe(`${bar.getBoundingClientRect().height}px`),
      );
      const block = blockEl();
      await scrollDeep();
      await waitFor(() => expect(block.dataset.stuck).toBe("true"));
      expect(Math.abs(block.getBoundingClientRect().top - bar.getBoundingClientRect().bottom)).toBeLessThanOrEqual(0.5);
    } finally {
      style.remove();
      document.documentElement.style.removeProperty("--app-bar-h");
    }
  });
});

describe("Board toolbar offset follows the app bar (#4419)", () => {
  it("pins flush under the bar at 400px in the real shell", async () => {
    localStorage.setItem("uzi.board.repo-1.hideEmpty", "false");
    await page.viewport(400, 700);
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/board"]}>
        <AppShell>
          <Routes>
            <Route path="/repos/:id/board" element={<Board />} />
          </Routes>
        </AppShell>
      </MemoryRouter>,
    );
    await screen.findByText("Human Review");
    const toolbar = screen.getByPlaceholderText("Search issues…").closest(".sticky") as HTMLElement;
    await scrollDeep(600);
    expect(Math.abs(toolbar.getBoundingClientRect().top - barEl().getBoundingClientRect().bottom)).toBeLessThanOrEqual(0.5);
  });
});

describe("focus and anchors clear the pinned block (#4419)", () => {
  for (const width of [1440, 400]) {
    it(`shift-tab at ${width}px lands a control hidden under the block below it`, async () => {
      await page.viewport(width, 700);
      render(
        <MemoryRouter initialEntries={["/settings"]}>
          <AppShell>
            <Routes>
              <Route
                path="/settings/*"
                element={
                  <SettingsShell description="Buttons">
                    <div>
                      {range(40).map((i) => (
                        <button key={i} type="button" data-n={i} style={{ display: "block", height: 100 }}>
                          Control {i}
                        </button>
                      ))}
                    </div>
                  </SettingsShell>
                }
              />
            </Routes>
          </AppShell>
        </MemoryRouter>,
      );
      await screen.findByRole("button", { name: "Control 1" });
      const btn = (n: number) => document.querySelector<HTMLElement>(`[data-n="${n}"]`)!;
      await scrollDeep(1200);
      const block = blockEl();
      await waitFor(() => expect(block.dataset.stuck).toBe("true"));
      const blockBottom = block.getBoundingClientRect().bottom;
      // Park control 11 so its top is 30px under the block's bottom edge: partly hidden.
      window.scrollTo(0, window.scrollY + btn(11).getBoundingClientRect().top - (blockBottom - 30));
      await nextFrame();
      expect(btn(11).getBoundingClientRect().top).toBeLessThan(blockBottom);
      btn(12).focus({ preventScroll: true });
      await userEvent.keyboard("{Shift>}{Tab}{/Shift}");
      await nextFrame();
      expect(document.activeElement).toBe(btn(11));
      expect(btn(11).getBoundingClientRect().top).toBeGreaterThanOrEqual(blockBottom);
    });
  }
});

describe("pinned tablists keep their keyboard contract (#4419)", () => {
  for (const [name, idx, tabs] of [
    ["Schedules", 3, [/^Schedules/, /^Job catalog/]],
    ["Workers", 4, [/^Your workers/, /^Add a worker/]],
  ] as const) {
    it(`${name}: arrows, Home and End move the selection while pinned`, async () => {
      // Filler after the page keeps the document tall on every tab, so a scrollY change can
      // only come from focus moving the window, not from a shorter panel clamping it.
      // Chromium's scroll anchoring would also shift scrollY when a taller panel mounts above
      // the filler, so switch it off for this test.
      document.documentElement.style.overflowAnchor = "none";
      await mount(scenarios[idx], 1440, 700, <div style={{ height: 6000 }} />);
      await scrollDeep();
      const tab = (re: RegExp) => screen.getByRole("tab", { name: re });
      tab(tabs[0]).focus({ preventScroll: true });
      const y = window.scrollY;
      await userEvent.keyboard("{ArrowRight}");
      await waitFor(() => expect(tab(tabs[1]).getAttribute("aria-selected")).toBe("true"));
      expect(document.activeElement).toBe(tab(tabs[1]));
      expect(window.scrollY, "ArrowRight leaves the window put").toBe(y);
      await userEvent.keyboard("{Home}");
      await waitFor(() => expect(tab(tabs[0]).getAttribute("aria-selected")).toBe("true"));
      expect(window.scrollY, "Home leaves the window put").toBe(y);
      await userEvent.keyboard("{End}");
      await waitFor(() => expect(tab(tabs[1]).getAttribute("aria-selected")).toBe("true"));
      expect(window.scrollY, "End leaves the window put").toBe(y);
    });
  }

  for (const width of [1440, 400]) {
    it(`Schedules at ${width}px: Pause all, from deep in the page, shows the picker with focus in it`, async () => {
      await mount(scenarios[3], width);
      await scrollDeep();
      const pause = screen.getByRole("button", { name: /^Pause all/ });
      await act(async () => {
        pause.click();
      });
      const panel = await screen.findByRole("heading", { name: "Pause all schedules" });
      const box = panel.parentElement!;
      await waitFor(() => expect(box.contains(document.activeElement)).toBe(true));
      await nextFrame();
      await nextFrame();
      const r = box.getBoundingClientRect();
      const limit = width >= 1024 ? 0 : barEl().getBoundingClientRect().bottom;
      expect(r.top).toBeGreaterThanOrEqual(limit - 0.5);
      expect(r.bottom).toBeLessThanOrEqual(window.innerHeight);
    });
  }
});

describe("Admin tabs do not move the page when the health pip re-renders (#4419)", () => {
  it("keeps scrollY when the attention count changes while scrolled deep", async () => {
    await mount(scenarios[1], 400);
    await scrollDeep(1200);
    const before = window.scrollY;
    const healthTab = () => screen.getByRole("link", { name: /^Health/ });
    expect(healthTab().textContent).not.toMatch(/\d/);
    mockApi.getAdminHealth.mockResolvedValue(degradedDoc());
    // The shared health provider polls; becoming visible fires an immediate catch-up read.
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    // The pip re-rendered with a count, which re-runs AdminShell's keep-in-view effect.
    await waitFor(() => expect(healthTab().textContent).toMatch(/\d/));
    await nextFrame();
    expect(window.scrollY).toBe(before);
  });
});
