/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import "@fontsource/ibm-plex-sans/latin-400.css";
import "@fontsource/ibm-plex-sans/latin-500.css";
import "@fontsource/ibm-plex-sans/latin-600.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { AppShell } from "./AppShell";
import { APP_ROUTES } from "../App";
import { api } from "../lib/api";
import { healthySilentDoc } from "../mocks/data/health";
import { isApplePlatform } from "../lib/platform";
import { useAuth } from "../auth/AuthContext";

// #4373 in a real engine: choosing a menu item must land the page at the target. Scroll
// position, focus and "within the viewport" need real layout, which jsdom cannot give.
vi.mock("../lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api")>()),
  MOCK_MODE: false,
  api: {
    // AppShell and the menu.
    listRepos: vi.fn(),
    listConnections: vi.fn(),
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
    getJudgeStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    listSchedules: vi.fn().mockResolvedValue([]),
    listAgentTemplates: vi.fn().mockResolvedValue({ templates: [] }),
    listSkills: vi.fn().mockResolvedValue({ skills: [] }),
    getFindingsStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getRecoveryHolds: vi.fn(),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    getMyCodexRateLimits: vi.fn().mockResolvedValue({ accounts: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" }),
    getAdminHealth: vi.fn(),
    // WorkersSettings.
    listWorkers: vi.fn(),
    listSecrets: vi.fn(),
    hostedConfig: vi.fn(),
    // AdminSettings.
    getSettings: vi.fn(),
    adminListWorkers: vi.fn(),
    updateSettings: vi.fn(),
    vaultMigration: vi.fn(),
    adminListDockerAllowlistRepos: vi.fn(),
    getAgentSource: vi.fn(),
    getReleaseCheck: vi.fn(),
    checkReleaseNow: vi.fn(),
    getAdminMaintenance: vi.fn(),
    setMaintenance: vi.fn(),
    getSlackStatus: vi.fn(),
  },
}));
// The route table imports guards and pages that use other AuthContext exports; keep them real.
vi.mock("../auth/AuthContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../auth/AuthContext")>()),
  useAuth: vi.fn(),
}));

const mockApi = vi.mocked(api);

const settings = {
  autopilot_label: "autopilot",
  uzi_label: "uzi",
  default_theme: "ember",
  default_appearance_mode: "dark",
  default_light_theme: "hall",
  default_dark_theme: "ember",
  default_typeface: "system",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  codex_judge_model: "gpt-6.2-sol",
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
  ephemeral_workers_enabled: "false",
  hosted_worker_quota: "2",
  custody_hold_limit: "8",
  ci_autofix_enabled: "true",
  now_summary_enabled: "true",
  release_check_enabled: "true",
  release_check_banner_enabled: "true",
  summary_model: "haiku",
  codex_summary_model: "gpt-6-luna",
  health_enabled: "true",
  health_stall_seconds: "300",
  health_tool_call_seconds: "1200",
  health_near_timeout_pct: "85",
  health_queued_seconds: "600",
  health_approval_seconds: "3600",
  health_nudge_cooldown_seconds: "1800",
  run_extension_cap_seconds: "57600",
  docker_repo_allowlist: "",
  mr_review_trusted_bots: "",
  capability_aware_scheduling: "true",
  completion_interlock_rollout: "true",
  github_project_sync_enabled: "false",
  fetch_max_file_bytes: "26214400",
  fetch_max_run_bytes: "209715200",
  fetch_max_run_files: "100",
  fetch_max_concurrent_per_run: "4",
  fetch_max_run_attempts: "500",
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_keep_name: "true",
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: "false",
} as unknown as import("../lib/api").AppSettings;

const loadedResponse = () => ({
  settings,
  secrets: { slack_bot_token: false, slack_app_token: false },
  sources: {} as Record<string, "env" | "db" | "default">,
  defaults: {
    judge_model: "opus",
    codex_judge_model: "gpt-6.2-sol",
    summary_model: "haiku",
    codex_summary_model: "gpt-6-luna",
  },
  slack_status: "disabled",
});

function aWorker(id: string, name: string) {
  return {
    hibernation_state: "none",
    hibernation_reason: null,
    hibernated_at: null,
    wake_requested_at: null,
    wake_error: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    id,
    name,
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
  } as unknown as import("../lib/api").Worker;
}

beforeEach(async () => {
  await page.viewport(1200, 700);
  window.scrollTo(0, 0);
  vi.mocked(useAuth).mockReturnValue({
    user: {
      id: "u1",
      email: "admin@uzi.local",
      display_name: "Admin",
      is_admin: true,
      is_active: true,
      created_at: "2026-01-01T00:00:00Z",
      last_login: null,
    },
    loading: false,
    logout: vi.fn(),
    refresh: vi.fn().mockResolvedValue(undefined),
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
  mockApi.getRecoveryHolds.mockResolvedValue({
    aggregate: { open_holds: 0, admission_counted_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  } as Awaited<ReturnType<typeof api.getRecoveryHolds>>);
  mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listWorkers.mockResolvedValue({ workers: [aWorker("w1", "quokkaworker"), aWorker("w2", "other-box")] });
  mockApi.getSettings.mockResolvedValue(loadedResponse());
  mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
  mockApi.vaultMigration.mockResolvedValue({ master_sealed: 0 });
  mockApi.getSlackStatus.mockResolvedValue({ slack_status: "disabled" });
  mockApi.getReleaseCheck.mockRejectedValue(new Error("not under test"));
  mockApi.getAgentSource.mockRejectedValue(new Error("not under test"));
  mockApi.adminListDockerAllowlistRepos.mockResolvedValue({ repos: [] });
  const none = { count: 0, items: [] };
  mockApi.getAdminMaintenance.mockResolvedValue({
    state: { active: false, since: null, reason: null, by_label: null },
    readout: { as_of: "2026-10-25T12:00:00Z", clear: true, active: none, held: none, custody: none, queued: none, idle_bound_workers: none },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function pageFor(path: string) {
  const route = APP_ROUTES.find((r) => r.path === path);
  if (!route) throw new Error(`no APP_ROUTES entry for ${path}`);
  return route.element;
}

// Mirrors the router's hash as text so a test can assert what the menu navigated to.
function Where() {
  const loc = useLocation();
  return <output data-testid="where">{loc.pathname + loc.search + loc.hash}</output>;
}

function mountApp(start: string) {
  render(
    <MemoryRouter initialEntries={[start]}>
      <AppShell>
        <Where />
        <Routes>
          <Route path="/start" element={<p>start page</p>} />
          <Route path="/tall" element={<div data-testid="tall" style={{ height: 4000 }}>tall start page</div>} />
          <Route path="/workers"element={pageFor("/workers")} />
          <Route path="/admin/settings" element={pageFor("/admin/settings")} />
          <Route path="/docs/:slug" element={pageFor("/docs/:slug")} />
        </Routes>
      </AppShell>
    </MemoryRouter>,
  );
}

const dialogOptions = () =>
  within(screen.getByRole("dialog", { name: "Command menu" })).queryAllByRole("option");
const labelOf = (o: HTMLElement) => o.querySelector("span.truncate")?.textContent ?? "";

// Opens the menu with the platform shortcut, types `query`, asserts the wanted row is the
// active (first) one, and presses Enter.
async function chooseFromMenu(query: string, label: string) {
  await vi.waitFor(() => expect(screen.queryByRole("link", { name: "Workers" })).not.toBeNull());
  await userEvent.keyboard(isApplePlatform() ? "{Meta>}k{/Meta}" : "{Control>}k{/Control}");
  const dialog = await screen.findByRole("dialog", { name: "Command menu" });
  // Scoped to the dialog: the Add a worker form behind it has comboboxes of its own.
  const input = within(dialog).getByRole("combobox");
  await vi.waitFor(() => expect(document.activeElement).toBe(input));
  await userEvent.keyboard(query);
  await vi.waitFor(() => expect(dialogOptions().map(labelOf)).toContain(label));
  expect(labelOf(dialogOptions()[0])).toBe(label);
  await userEvent.keyboard("{Enter}");
  await vi.waitFor(() => expect(screen.queryByRole("dialog", { name: "Command menu" })).toBeNull());
}

const inViewport = (el: Element) => {
  const r = el.getBoundingClientRect();
  return r.top >= 0 && r.top < window.innerHeight;
};

describe("command menu reveals in a real browser (#4373)", () => {
  it("Admin > Settings > Slack lands with #slack on screen after settings load", async () => {
    mountApp("/start");
    await chooseFromMenu("slack", "Admin › Settings › Slack");
    await vi.waitFor(() => expect(document.getElementById("slack")).not.toBeNull());
    const slack = document.getElementById("slack")!;
    await vi.waitFor(() => expect(inViewport(slack), `slack top=${slack.getBoundingClientRect().top}`).toBe(true));
    // Non-vacuous: the section was below the fold, so the page really scrolled.
    expect(window.scrollY).toBeGreaterThan(0);
  });

  it("a failed settings load leaves the page at the top with the error shown", async () => {
    mockApi.getSettings.mockRejectedValue(new Error("settings exploded"));
    mountApp("/start");
    await chooseFromMenu("maintenance mode", "Admin › Settings › Maintenance mode");
    expect(await screen.findByText(/settings exploded|Failed to load settings/)).toBeTruthy();
    // Let any (wrongly) scheduled scroll run before asserting it did not.
    await new Promise((r) => setTimeout(r, 250));
    // Non-vacuous: the page is tall enough that a scroll to the anchor would have moved it.
    expect(document.getElementById("maintenance")).not.toBeNull();
    expect(document.documentElement.scrollHeight).toBeGreaterThan(window.innerHeight);
    expect(window.scrollY).toBe(0);
    expect(screen.getByTestId("where").textContent).toBe("/admin/settings#maintenance");
  });

  it("a failed settings load resets a scrolled page to the top", async () => {
    mockApi.getSettings.mockRejectedValue(new Error("settings exploded"));
    mountApp("/tall");
    await vi.waitFor(() => expect(screen.queryByTestId("tall")).not.toBeNull());
    window.scrollTo(0, 400);
    await vi.waitFor(() => expect(window.scrollY).toBeGreaterThan(0));
    await chooseFromMenu("slack", "Admin › Settings › Slack");
    expect(await screen.findByText(/settings exploded|Failed to load settings/)).toBeTruthy();
    await vi.waitFor(() => expect(window.scrollY).toBe(0));
  });

  it("Enter on a worker leaves its row focused", async () => {
    mountApp("/workers?tab=add");
    await screen.findByRole("tab", { name: /Add a worker/ });
    await chooseFromMenu("quokkaworker", "quokkaworker");
    await vi.waitFor(() => expect(document.activeElement?.id).toBe("worker-row-w1"));
    const row = document.activeElement as HTMLElement;
    expect(row.closest("[hidden]")).toBeNull();
    expect(inViewport(row)).toBe(true);
    // Focus stays on the row once the page has settled (nothing steals it back).
    await new Promise((r) => setTimeout(r, 250));
    expect(document.activeElement).toBe(row);
  });

  it("Enter on a heading of the bundled getting-started doc scrolls that heading into view", async () => {
    await page.viewport(1200, 500);
    mountApp("/start");
    await chooseFromMenu("run an agent", "Getting started › 5. Run an agent");
    const heading = await screen.findByRole("heading", { level: 2, name: "5. Run an agent" });
    // The hash the menu navigated to names this very element.
    const hash = screen.getByTestId("where").textContent!.split("#")[1];
    expect(decodeURIComponent(hash)).toBe(heading.id);
    expect(heading.id).not.toBe("");
    await vi.waitFor(() => expect(inViewport(heading), `top=${heading.getBoundingClientRect().top}`).toBe(true));
    expect(window.scrollY).toBeGreaterThan(0);
  });
});
