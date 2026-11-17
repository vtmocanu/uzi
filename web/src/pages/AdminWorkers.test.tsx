// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminWorkers } from "./AdminWorkers";
import { healthySilentDoc } from "../mocks/data/health";
import { api } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getSettings: vi.fn(),
      getAdminHealth: vi.fn(),
      updateSettings: vi.fn(),
      adminListDockerAllowlistRepos: vi.fn(),
      adminListWorkers: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ refresh: vi.fn().mockResolvedValue(undefined) }) }));

const mockApi = vi.mocked(api);

// A full AppSettings fixture; tests override the fields they exercise.
const settings = (over: Partial<import("../lib/api").AppSettings> = {}) => ({
  autopilot_label: "autopilot",
  uzi_label: "uzi",
  default_theme: "ember",
  // PRD #1167 "Lights on" m2: instance appearance defaults (required AppSettings keys).
  default_appearance_mode: "dark",
  default_light_theme: "hall",
  default_dark_theme: "ember",
  default_typeface: "system",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  // The server reports the built-in as the effective value of an unset key (source "default").
  codex_judge_model: "gpt-6.1-sol",
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
  // PRD #685 branding config keys (owned by the Branding tab; unbranded defaults).
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_keep_name: "true",
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: "false",
  ...over,
});

type Src = "env" | "db" | "default";

const response = (
  over: Partial<import("../lib/api").AppSettings> = {},
  sources: Record<string, Src> = {},
) => ({
  settings: settings(over),
  secrets: {},
  sources: sources as Record<string, Src>,
  defaults: {},
  slack_status: "disabled",
});

beforeEach(() => {
  mockApi.getSettings.mockResolvedValue(response());
  mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
  mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
  mockApi.adminListDockerAllowlistRepos.mockResolvedValue({
    repos: [
      { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi", enabled: true, owner_email: "admin@example.com", connection_id: "conn-1", forge_type: "gitlab", base_url: "https://gitlab.example.com" },
      { id: "repo-two", path_with_namespace: "vtmocanu/two", enabled: true, owner_email: "dana@example.com", connection_id: "conn-2", forge_type: "github", base_url: "https://github.com" },
    ] as unknown as import("../lib/api").AdminDockerAllowlistRepo[],
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/admin/workers"]}>
      <AdminWorkers />
    </MemoryRouter>,
  );
}

const IDS = [
  "docker-allowlist",
  "hosted-worker-quota",
  "custody-hold-limit",
  "ephemeral-workers",
  "worker-hibernation",
  "capability-scheduling",
];

describe("AdminWorkers", () => {
  it("renders the six worker cards and an on-page index listing all of them", async () => {
    renderPage();
    const nav = await screen.findByRole("navigation", { name: "Sections on this page" });
    await waitFor(() => {
      for (const id of IDS) expect(document.getElementById(id), id).not.toBeNull();
    });
    expect([...nav.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual(IDS.map((id) => `#${id}`));
    expect(screen.getByRole("link", { name: "Hosted worker quota" }).getAttribute("href")).toBe("#hosted-worker-quota");
    expect(document.getElementById("review-bots")).toBeNull();
  });

  it("marks Workers as the active admin tab and links the admin settings guide", async () => {
    renderPage();
    const tab = await screen.findByRole("link", { name: "Workers" });
    expect(tab.getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("link", { name: "admin settings" }).getAttribute("href")).toBe("/docs/admin-settings");
  });

  it("shows the load error and no cards when settings fail to load", async () => {
    mockApi.getSettings.mockRejectedValue(new Error("boom"));
    renderPage();
    await screen.findByText(/boom|Failed to load settings/);
    for (const id of IDS) expect(document.getElementById(id), id).toBeNull();
    expect(screen.queryByRole("navigation", { name: "Sections on this page" })).toBeNull();
  });

  it("renders the current hosted worker quota", async () => {
    mockApi.getSettings.mockResolvedValue(response({ hosted_worker_quota: "3" }));
    renderPage();
    const quota = await screen.findByRole("spinbutton", { name: "Hosted workers per user" });
    expect((quota as HTMLInputElement).value).toBe("3");
    expect(screen.getByRole("button", { name: "Save hosted worker quota" })).toHaveProperty("disabled", true);
  });

  it("saves the flipped ephemeral-workers kill-switch as a string bool", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ ephemeral_workers_enabled: "true" }));
    renderPage();
    const toggle = (await screen.findByLabelText(/Auto-provision workers on demand for this instance/i)) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    const btn = screen.getByRole("button", { name: /save ephemeral workers settings/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    fireEvent.click(toggle);
    fireEvent.click(btn);
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith(expect.objectContaining({ ephemeral_workers_enabled: "true" })),
    );
  });

  it("saves the Docker allowlist, preserving unresolved ids", async () => {
    mockApi.getSettings.mockResolvedValue(response({ docker_repo_allowlist: "repo-uzi,repo-other" }));
    mockApi.updateSettings.mockResolvedValue(response({ docker_repo_allowlist: "repo-other,repo-two,repo-uzi" }));
    renderPage();
    expect(await screen.findByText(/unresolved\/deleted repositories \(preserved\)/i)).toBeTruthy();
    fireEvent.click(await screen.findByLabelText(/vtmocanu\/two/));
    fireEvent.click(screen.getByRole("button", { name: /save repo allowlist/i }));
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ docker_repo_allowlist: "repo-other,repo-two,repo-uzi" }),
    );
  });

  it("saves capability scheduling OFF, and disables it when fixed by the environment", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ capability_aware_scheduling: "false" }));
    renderPage();
    await screen.findByText("Capability-aware scheduling");
    const toggle = screen.getByLabelText(/enable capability-aware scheduling/i) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
    fireEvent.click(toggle);
    fireEvent.click(screen.getByRole("button", { name: /save capability scheduling/i }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ capability_aware_scheduling: "false" }));

    cleanup();
    mockApi.getSettings.mockResolvedValue(response({ capability_aware_scheduling: "true" }, { capability_aware_scheduling: "env" }));
    renderPage();
    await screen.findByText("Capability-aware scheduling");
    expect((screen.getByLabelText(/enable capability-aware scheduling/i) as HTMLInputElement).disabled).toBe(true);
  });
});

describe("AdminWorkers recreated cases", () => {
  it("loads the current ephemeral-workers kill-switch value (true)", async () => {
    mockApi.getSettings.mockResolvedValue(response({ ephemeral_workers_enabled: "true" }));
    renderPage();
    const toggle = (await screen.findByLabelText(/Auto-provision workers on demand for this instance/i)) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
  });

  it("keeps the docker allowlist Save disabled until the selection changes (dirty-check)", async () => {
    mockApi.getSettings.mockResolvedValue(response({ docker_repo_allowlist: "repo-uzi" }));
    renderPage();
    const saveBtn = () => screen.getByRole("button", { name: /save repo allowlist/i }) as HTMLButtonElement;
    await screen.findByLabelText(/vtmocanu\/two/);
    expect(saveBtn().disabled).toBe(true);
    fireEvent.click(screen.getByLabelText(/vtmocanu\/two/));
    expect(saveBtn().disabled).toBe(false);
  });

  it("does not show the unresolved indicator when the repos list fails to load", async () => {
    mockApi.getSettings.mockResolvedValue(response({ docker_repo_allowlist: "repo-uzi,repo-other" }));
    mockApi.adminListDockerAllowlistRepos.mockRejectedValue(new Error("network"));
    renderPage();
    expect(await screen.findByText(/could not load repositories/i)).toBeTruthy();
    expect(screen.queryByText(/unresolved\/deleted repositories/i)).toBeNull();
  });

  it("renders the capability toggle checked (default ON) with Save disabled until changed", async () => {
    renderPage();
    await screen.findByText("Capability-aware scheduling");
    const toggle = screen.getByLabelText(/enable capability-aware scheduling/i) as HTMLInputElement;
    const saveBtn = screen.getByRole("button", { name: /save capability scheduling/i }) as HTMLButtonElement;
    expect(toggle.checked).toBe(true);
    expect(saveBtn.disabled).toBe(true);
    fireEvent.click(toggle);
    expect(saveBtn.disabled).toBe(false);
  });
});
