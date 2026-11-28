// @vitest-environment jsdom
//
// #4373 end to end in jsdom: open the command menu inside AppShell, search, press Enter, and
// let the REAL Schedules / WorkersSettings / Skills pages (taken from the app's APP_ROUTES)
// load and reveal the chosen item. The per-page reveal tests drive the pages with a hand-built
// URL; this proves the menu's own targets are the ones the pages understand.
import type { ReactElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AppShell } from "./AppShell";
import { APP_ROUTES } from "../App";
import { api, type Schedule, type Skill, type Worker } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    MOCK_MODE: false,
    api: {
      // AppShell and the menu.
      listRepos: vi.fn(),
      listConnections: vi.fn(),
      listConnectionsBackground: vi.fn().mockResolvedValue({ connections: [] }),
      workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
      getJudgeStats: vi
        .fn()
        .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
      runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
      listAgentTemplates: vi.fn().mockResolvedValue({ templates: [] }),
      getFindingsStats: vi
        .fn()
        .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
      listRuns: vi.fn().mockResolvedValue({ runs: [] }),
      getRecoveryHolds: vi.fn(),
      getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
      getMyCodexRateLimits: vi.fn().mockResolvedValue({ accounts: [] }),
      getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
      version: vi.fn().mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" }),
      getAdminHealth: vi.fn().mockRejectedValue(new Error("no health")),
      // Shared by the menu and the pages.
      listSchedules: vi.fn(),
      listWorkers: vi.fn(),
      listSkills: vi.fn(),
      // Schedules page.
      listScheduleCatalog: vi.fn(),
      getSchedulePause: vi.fn(),
      // WorkersSettings page.
      listSecrets: vi.fn(),
      hostedConfig: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const holds = {
  aggregate: { open_holds: 0, admission_counted_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
  holds: [],
};

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

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    hibernation_state: "none",
    hibernation_reason: null,
    hibernated_at: null,
    wake_requested_at: null,
    wake_error: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    kind: "external",
    hosted_size: null,
    active_runs: 0,
    max_concurrent_runs: null,
    active_cross_checks: 0,
    max_cross_check_slots: null,
    template_declared: null,
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: "2026-07-14T00:00:00Z",
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    stats_disk_dind_bytes: null,
    stats_disk_dind_total_bytes: null,
    stats_disk_dind_inodes: null,
    stats_disk_dind_total_inodes: null,
    draining_since: null,
    ...over,
  };
}

function aSkill(over: Partial<Skill> & Pick<Skill, "id" | "name" | "scope">): Skill {
  return {
    description: `desc for ${over.name}`,
    body: `# ${over.name}\n\nbody`,
    user_id: null,
    updated_by: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

// The real page elements from the app's route table, so a route rename cannot go unnoticed.
function pageFor(path: string): ReactElement {
  const route = APP_ROUTES.find((r) => r.path === path);
  if (!route) throw new Error(`no APP_ROUTES entry for ${path}`);
  return route.element;
}

function renderApp(start: string) {
  return render(
    <MemoryRouter initialEntries={[start]}>
      <AppShell>
        <Routes>
          <Route path="/start" element={<p>start page</p>} />
          <Route path="/schedules" element={pageFor("/schedules")} />
          <Route path="/workers" element={pageFor("/workers")} />
          <Route path="/skills" element={pageFor("/skills")} />
        </Routes>
      </AppShell>
    </MemoryRouter>,
  );
}

// Scoped to the dialog: a page behind it can have <option>s of its own.
const optionLabels = () =>
  within(screen.getByRole("dialog", { name: "Command menu" })).queryAllByRole("option").map((o) => o.querySelector("span.truncate")?.textContent ?? "");

async function searchAndEnter(query: string, expectedLabel: string) {
  await screen.findByRole("link", { name: "Workers" });
  act(() => {
    document.body.dispatchEvent(
      new KeyboardEvent("keydown", { key: "k", code: "KeyK", ctrlKey: true, bubbles: true, cancelable: true }),
    );
  });
  // Scoped to the dialog: a page behind it (Add a worker) has comboboxes of its own.
  const dialog = await screen.findByRole("dialog", { name: "Command menu" });
  const input = within(dialog).getByRole("combobox");
  fireEvent.change(input, { target: { value: query } });
  await waitFor(() => expect(optionLabels()).toContain(expectedLabel));
  // The match must be the active (first) row, or Enter would navigate elsewhere.
  expect(optionLabels()[0]).toBe(expectedLabel);
  fireEvent.keyDown(input, { key: "Enter" });
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Command menu" })).toBeNull());
}

let scrollSpy: ReturnType<typeof vi.fn>;

beforeEach(() => {
  scrollSpy = vi.fn();
  HTMLElement.prototype.scrollIntoView = scrollSpy as unknown as typeof HTMLElement.prototype.scrollIntoView;
  vi.mocked(useAuth).mockReturnValue({
    user: {
      id: "u1",
      email: "member@uzi.local",
      display_name: "Member",
      is_admin: false,
      is_active: true,
      created_at: "2026-01-01T00:00:00Z",
      last_login: null,
    },
    loading: false,
    logout: vi.fn(),
    refresh: vi.fn(),
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
  mockApi.getRecoveryHolds.mockResolvedValue(holds as Awaited<ReturnType<typeof api.getRecoveryHolds>>);
  mockApi.listScheduleCatalog.mockResolvedValue({ entries: [], enablements: [] });
  mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: null });
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listSchedules.mockResolvedValue([]);
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.listSkills.mockResolvedValue({ skills: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("command menu to page reveal (#4373)", () => {
  it("Enter on a schedule scrolls to, focuses and highlights its name block once Schedules loads", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "s1", labels: ["quokkasched"] }),
      sched({ id: "s2", repo_path: "vtmocanu/other" }),
    ]);
    renderApp("/start");
    await searchAndEnter("quokkasched", "Sweep · label quokkasched · vtmocanu/uzi");

    await waitFor(() => expect(document.activeElement?.id).toBe("schedule-name-s1"));
    const el = document.getElementById("schedule-name-s1")!;
    expect(el.getAttribute("data-revealed")).toBe("true");
    expect(scrollSpy.mock.contexts).toContain(el);
  });

  it("Enter on a worker from the Add tab shows the workers panel and focuses the row", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "w1", name: "quokkaworker" }), aWorker({ id: "w2", name: "other-box" })],
    });
    renderApp("/workers?tab=add");
    await screen.findByRole("tab", { name: /Add a worker/ });
    await waitFor(() => expect(document.getElementById("worker-row-w1")).not.toBeNull());
    expect(document.getElementById("worker-row-w1")?.closest("[hidden]")).not.toBeNull();

    await searchAndEnter("quokkaworker", "quokkaworker");

    await waitFor(() => expect(document.activeElement?.id).toBe("worker-row-w1"));
    const row = document.getElementById("worker-row-w1")!;
    expect(row.closest("[hidden]")).toBeNull();
    expect(row.getAttribute("data-revealed")).toBe("true");
    expect(scrollSpy.mock.contexts).toContain(row);
  });

  it("Enter on a skill opens that skill's view", async () => {
    mockApi.listSkills.mockResolvedValue({
      skills: [aSkill({ id: "sk1", name: "quokkaskill", scope: "global" }), aSkill({ id: "sk2", name: "other-skill", scope: "global" })],
    });
    renderApp("/start");
    await searchAndEnter("quokkaskill", "quokkaskill");

    expect(await screen.findByRole("heading", { name: "quokkaskill" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Back" })).toBeTruthy();
  });
});
