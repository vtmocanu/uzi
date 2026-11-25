// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminSettings } from "./AdminSettings";
import { healthySilentDoc } from "../mocks/data/health";
import { api } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getSettings: vi.fn(),
      getAdminHealth: vi.fn(),
      adminListWorkers: vi.fn(),
      updateSettings: vi.fn(),
      vaultMigration: vi.fn(),
      adminListDockerAllowlistRepos: vi.fn(),
      listRepos: vi.fn(),
      getAgentSource: vi.fn(),
      getReleaseCheck: vi.fn(),
      checkReleaseNow: vi.fn(),
      getAdminMaintenance: vi.fn(),
      setMaintenance: vi.fn(),
      getSlackStatus: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ refresh: vi.fn().mockResolvedValue(undefined) }) }));

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

const scroll = vi.fn();
const originalScroll = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollIntoView");

beforeEach(() => {
  scroll.mockClear();
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", { configurable: true, value: scroll });
  mockApi.getSettings.mockResolvedValue(loadedResponse());
  mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
  mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
  mockApi.vaultMigration.mockResolvedValue({ master_sealed: 0 });
  mockApi.getSlackStatus.mockResolvedValue({ slack_status: "disabled" });
  mockApi.getReleaseCheck.mockRejectedValue(new Error("not under test"));
  mockApi.getAgentSource.mockRejectedValue(new Error("not under test"));
  mockApi.listRepos.mockResolvedValue({ repos: [] });
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
  if (originalScroll) Object.defineProperty(HTMLElement.prototype, "scrollIntoView", originalScroll);
  else Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
});

function renderAt() {
  return render(
    <MemoryRouter initialEntries={["/admin/settings"]}>
      <AdminSettings />
    </MemoryRouter>,
  );
}

describe("AdminSettings section pills", () => {
  const pills = async () => {
    const nav = await screen.findByRole("navigation", { name: "Sections on this page" });
    return Array.from(nav.querySelectorAll("a"));
  };
  const current = (list: HTMLAnchorElement[]) => list.filter((a) => a.getAttribute("aria-current") === "true");

  it("marks exactly the first pill current after load", async () => {
    renderAt();
    const list = await pills();
    expect(list.length).toBeGreaterThan(1);
    await waitFor(() => expect(current(list)).toEqual([list[0]]));
  });

  it("moves the current mark to the clicked pill only", async () => {
    renderAt();
    const list = await pills();
    const target = list[2];
    fireEvent.click(target);
    await waitFor(() => expect(current(list)).toEqual([target]));
  });
});
