// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminWorkers } from "./AdminWorkers";
import { healthySilentDoc } from "../mocks/data/health";
import { api } from "../lib/api";
import { BUILTIN_WORKER_SIZES } from "../lib/workerSizes";

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
      getAdminWorkerSizes: vi.fn(),
      updateAdminWorkerSize: vi.fn(),
      resetAdminWorkerSize: vi.fn(),
      createAdminWorkerSize: vi.fn(),
      deleteAdminWorkerSize: vi.fn(),
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

// GET /admin/worker-sizes over the builtin seed (PRD #4213); `over` patches rows by name.
const adminSizes = (over: Record<string, Partial<import("../lib/api").AdminWorkerSize>> = {}): import("../lib/api").AdminWorkerSizes => ({
  sizes: BUILTIN_WORKER_SIZES.map((s, i) => ({
    ...s,
    display_order: i,
    builtin: true,
    customized: false,
    shipped: { ...s },
    updated_at: "2026-11-01T09:00:00Z",
    ...over[s.name],
  })),
  ceilings: { cpu_limit_millicores: null, memory_limit_mib: null, data_gib: null, dind_data_gib: null, dind_overridden: false, enabled_tiers: null },
  editable: true,
  read_only_reason: null,
  read_only_code: null,
  advisories: [],
  max_sizes: 16,
});

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
  mockApi.getAdminWorkerSizes.mockResolvedValue(adminSizes());
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
  "worker-sizes",
  "hosted-worker-quota",
  "custody-hold-limit",
  "ephemeral-workers",
  "worker-hibernation",
  "capability-scheduling",
];

describe("AdminWorkers", () => {
  it("renders the seven worker cards and an on-page index listing all of them", async () => {
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

  it("passes the settings sources to the worker size, cap and lease controls (PRD #4212)", async () => {
    mockApi.getSettings.mockResolvedValue(
      response(
        { ephemeral_lease: "30m", ephemeral_default_size: "m", hosted_default_size: "s" },
        { ephemeral_lease: "env", hosted_default_size: "db" },
      ),
    );
    renderPage();
    const lease = (await screen.findByLabelText("Idle lease")) as HTMLInputElement;
    expect(lease.value).toBe("30m");
    expect(lease.disabled).toBe(true);
    expect((screen.getByLabelText("Default size for new workers") as HTMLSelectElement).value).toBe("s");
    expect(screen.getByRole("button", { name: "Reset to default: Default size for new workers" })).toBeTruthy();
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

describe("sibling-card save responses (#4544)", () => {
  // Loose builder for keys the AppSettings fixture type does not enumerate.
  const resp = (over: Record<string, string>, sources: Record<string, Src> = {}) =>
    response(over as Partial<import("../lib/api").AppSettings>, sources);
  const HIB_SAVE = "Save worker hibernation settings";
  const hibBox = () => screen.getByRole("checkbox", { name: "Allow hibernating persistent hosted workers" }) as HTMLInputElement;
  const hibSave = () => screen.getByRole("button", { name: HIB_SAVE }) as HTMLButtonElement;
  const hibKeys = { worker_hibernation_auto: "false", worker_hibernation_idle_after: "336h", worker_hibernation_surplus: "false", worker_hibernation_surplus_idle_after: "2h" };

  it("keeps a sibling card's confirmed value when a slower, older response lands", async () => {
    mockApi.getSettings.mockResolvedValue(resp({ docker_repo_allowlist: "repo-uzi", worker_hibernation_enabled: "false", ...hibKeys }));
    let release!: (r: ReturnType<typeof response>) => void;
    const held = new Promise<ReturnType<typeof response>>((res) => {
      release = res;
    });
    mockApi.updateSettings.mockReturnValueOnce(held as never);
    mockApi.updateSettings.mockResolvedValueOnce(resp({ docker_repo_allowlist: "repo-uzi", worker_hibernation_enabled: "true", ...hibKeys }) as never);
    renderPage();
    fireEvent.click(await screen.findByLabelText(/vtmocanu\/two/));
    fireEvent.click(screen.getByRole("button", { name: /save repo allowlist/i }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledTimes(1));

    fireEvent.click(hibBox());
    fireEvent.click(hibSave());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByText("Worker hibernation settings saved.")).toBeTruthy());
    expect(hibSave().disabled).toBe(true);

    // The older allowlist response still carries the pre-save hibernation value.
    release(resp({ docker_repo_allowlist: "repo-two,repo-uzi", worker_hibernation_enabled: "false", ...hibKeys }));
    await screen.findByText(/repo allowlist saved/i);

    expect(hibBox().checked).toBe(true);
    // Save stays disabled only if the page still holds "true" (the card's checkbox is local state).
    expect(hibSave().disabled).toBe(true);
    fireEvent.click(hibBox());
    expect(hibSave().disabled).toBe(false);
  });

  it("seeds every key on load and keeps another card's source state when a card saves", async () => {
    // Every key carries a value that differs from what the card renders for a missing key,
    // so a key dropped from the initial seeding shows up as a changed rendered value.
    mockApi.getSettings.mockResolvedValue({
      ...resp(
        {
          docker_repo_allowlist: "repo-two",
          hosted_worker_quota: "5",
          hosted_default_size: "m",
          custody_hold_limit: "13",
          ephemeral_workers_enabled: "true",
          ephemeral_max_per_user: "6",
          ephemeral_default_size: "l",
          ephemeral_lease: "30m",
          worker_hibernation_enabled: "true",
          worker_hibernation_auto: "true",
          worker_hibernation_idle_after: "72h",
          worker_hibernation_surplus: "true",
          worker_hibernation_surplus_idle_after: "30m",
          capability_aware_scheduling: "true",
        },
        { capability_aware_scheduling: "env", ephemeral_lease: "env", hosted_default_size: "db" },
      ),
      defaults: { hosted_default_size: "s", ephemeral_max_per_user: "3", ephemeral_default_size: "s", ephemeral_lease: "2h" },
    });
    mockApi.updateSettings.mockResolvedValue(
      resp({ docker_repo_allowlist: "repo-two,repo-uzi" }, { capability_aware_scheduling: "db", docker_repo_allowlist: "db" }) as never,
    );
    renderPage();
    const toggle = () => screen.getByLabelText(/enable capability-aware scheduling/i) as HTMLInputElement;
    const quota = () => screen.getByRole("spinbutton", { name: "Hosted workers per user" }) as HTMLInputElement;
    const quotaSave = () => screen.getByRole("button", { name: "Save hosted worker quota" }) as HTMLButtonElement;
    const custody = () => screen.getByRole("spinbutton", { name: "Recovery holds per owner" }) as HTMLInputElement;
    const custodySave = () => screen.getByRole("button", { name: "Save custody hold limit" }) as HTMLButtonElement;
    await screen.findByText("Capability-aware scheduling");

    // Docker allowlist: only the seeded id is ticked.
    expect((await screen.findByLabelText(/vtmocanu\/two/) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText(/vtmocanu\/uzi/) as HTMLInputElement).checked).toBe(false);
    // Hosted quota, default size (value, source, defaults-driven hint).
    expect(quota().value).toBe("5");
    expect(quotaSave().disabled).toBe(true);
    expect((screen.getByLabelText("Default size for new workers") as HTMLSelectElement).value).toBe("m");
    expect(screen.getByText("Saved · built-in default s")).toBeTruthy();
    // Custody hold limit.
    expect(custody().value).toBe("13");
    expect(custodySave().disabled).toBe(true);
    // Ephemeral workers: switch, cap, default size, lease.
    expect((screen.getByLabelText(/Auto-provision workers on demand for this instance/i) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText("Per-user cap") as HTMLInputElement).value).toBe("6");
    expect(screen.getByText("Default · built-in default 3")).toBeTruthy();
    expect((screen.getByLabelText("Ephemeral default size") as HTMLSelectElement).value).toBe("l");
    expect(screen.getByText("Default · built-in default s")).toBeTruthy();
    const lease = screen.getByLabelText("Idle lease") as HTMLInputElement;
    expect(lease.value).toBe("30m");
    expect(lease.disabled).toBe(true);
    expect(screen.getByText("Set by environment · built-in default 2h")).toBeTruthy();
    // Worker hibernation: five keys, plus the readout derived from the idle threshold.
    expect(hibBox().checked).toBe(true);
    expect((screen.getByRole("checkbox", { name: "Automatically hibernate idle workers" }) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText("Idle threshold") as HTMLInputElement).value).toBe("72h");
    expect(screen.getByText("3 days without run activity")).toBeTruthy();
    const surplus = screen.getByRole("checkbox", { name: "Keep one warm worker per pool, hibernate the surplus" }) as HTMLInputElement;
    expect(surplus.checked).toBe(true);
    expect(surplus.disabled).toBe(false);
    expect((screen.getByLabelText("Surplus idle threshold") as HTMLInputElement).value).toBe("30m");
    expect(hibSave().disabled).toBe(true);
    // Capability scheduling: on (a missing key renders off) and locked by the environment.
    expect(toggle().checked).toBe(true);
    expect(toggle().disabled).toBe(true);
    expect(screen.getByRole("button", { name: "Reset to default: Default size for new workers" })).toBeTruthy();

    fireEvent.click(screen.getByLabelText(/vtmocanu\/uzi/));
    fireEvent.click(screen.getByRole("button", { name: /save repo allowlist/i }));
    await screen.findByText(/repo allowlist saved/i);

    // Unsent keys keep their seeded values and sources (the response carries fixture defaults).
    expect(toggle().disabled).toBe(true);
    expect((screen.getByLabelText("Idle lease") as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByRole("button", { name: "Reset to default: Default size for new workers" })).toBeTruthy();
    // Save stays disabled only while the page still holds the seeded quota, custody and hibernation values.
    expect(quota().value).toBe("5");
    expect(quotaSave().disabled).toBe(true);
    expect(custody().value).toBe("13");
    expect(custodySave().disabled).toBe(true);
    expect(hibSave().disabled).toBe(true);
  });

  it("merges the response values for the sent keys only, and sends exactly the payload", async () => {
    mockApi.getSettings.mockResolvedValue(
      resp(
        { docker_repo_allowlist: "repo-uzi", capability_aware_scheduling: "true", ephemeral_lease: "30m", hosted_default_size: "s", hosted_worker_quota: "2" },
        { hosted_default_size: "db" },
      ),
    );
    renderPage();
    const quota = () => screen.getByRole("spinbutton", { name: "Hosted workers per user" }) as HTMLInputElement;
    await screen.findByText("Capability-aware scheduling");

    // Allowlist: the response also carries unsent keys with other values; they must not land.
    mockApi.updateSettings.mockResolvedValueOnce(
      resp({ docker_repo_allowlist: "repo-uzi", capability_aware_scheduling: "false", hosted_worker_quota: "9" }) as never,
    );
    fireEvent.click(await screen.findByLabelText(/vtmocanu\/two/));
    fireEvent.click(screen.getByRole("button", { name: /save repo allowlist/i }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenLastCalledWith({ docker_repo_allowlist: "repo-two,repo-uzi" }));
    await screen.findByText(/repo allowlist saved/i);
    // Unsent keys keep their values and sources (the allowlist card's own checkbox is local
    // state, so the sent-key merge is asserted on the SettingControl step below instead).
    expect(quota().value).toBe("2");
    expect((screen.getByLabelText(/enable capability-aware scheduling/i) as HTMLInputElement).checked).toBe(true);

    // Capability: its own key lands, the unsent quota does not.
    mockApi.updateSettings.mockResolvedValueOnce(resp({ capability_aware_scheduling: "false", hosted_worker_quota: "7" }) as never);
    fireEvent.click(screen.getByLabelText(/enable capability-aware scheduling/i));
    fireEvent.click(screen.getByRole("button", { name: /save capability scheduling/i }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenLastCalledWith({ capability_aware_scheduling: "false" }));
    await screen.findByText("Capability-aware scheduling setting saved.");
    expect(quota().value).toBe("2");

    // A SettingControl-based control: the default-size selector. The server answers with a
    // value and a source that differ from what was sent ("m" sent, "l" confirmed, now the
    // default). SettingControl re-seeds its draft from its `value` prop, which the page
    // derives from applySaved, so the select showing "l" proves the page took the response
    // value (not the sent one) and the vanished Reset button proves it took the response source.
    const resetBtn = () => screen.queryByRole("button", { name: "Reset to default: Default size for new workers" });
    expect(resetBtn()).toBeTruthy();
    mockApi.updateSettings.mockResolvedValueOnce(
      resp({ hosted_default_size: "l", ephemeral_lease: "99m" }, { hosted_default_size: "default" }) as never,
    );
    fireEvent.change(screen.getByLabelText("Default size for new workers"), { target: { value: "m" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Default size for new workers" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenLastCalledWith({ hosted_default_size: "m" }));
    await waitFor(() => expect(resetBtn()).toBeNull());
    expect((screen.getByLabelText("Default size for new workers") as HTMLSelectElement).value).toBe("l");
    expect((screen.getByLabelText("Idle lease") as HTMLInputElement).value).toBe("30m");

    // Hosted quota: the response confirms a value that differs from the one sent. Save going
    // disabled needs the page's own copy to equal the response value (the card's input alone
    // would also show it), and the unsent custody limit in the response must not land.
    const quotaSave = () => screen.getByRole("button", { name: "Save hosted worker quota" }) as HTMLButtonElement;
    const custody = () => screen.getByRole("spinbutton", { name: "Recovery holds per owner" }) as HTMLInputElement;
    const custodySave = () => screen.getByRole("button", { name: "Save custody hold limit" }) as HTMLButtonElement;
    expect(quotaSave().disabled).toBe(true);
    mockApi.updateSettings.mockResolvedValueOnce(resp({ hosted_worker_quota: "8", custody_hold_limit: "99" }) as never);
    fireEvent.change(quota(), { target: { value: "6" } });
    expect(quotaSave().disabled).toBe(false);
    fireEvent.click(quotaSave());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenLastCalledWith({ hosted_worker_quota: "6" }));
    await screen.findByText("Hosted worker quota saved.");
    expect(quota().value).toBe("8");
    expect(quotaSave().disabled).toBe(true);
    // Custody was not sent: still the fixture's 8, so its Save is disabled against the page's 8, not 99.
    expect(custody().value).toBe("8");
    expect(custodySave().disabled).toBe(true);

    // Custody hold limit: same shape, with the unsent quota differing in the response.
    mockApi.updateSettings.mockResolvedValueOnce(resp({ custody_hold_limit: "15", hosted_worker_quota: "19" }) as never);
    fireEvent.change(custody(), { target: { value: "12" } });
    expect(custodySave().disabled).toBe(false);
    fireEvent.click(custodySave());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenLastCalledWith({ custody_hold_limit: "12" }));
    await screen.findByText("Custody hold limit saved.");
    expect(custody().value).toBe("15");
    expect(custodySave().disabled).toBe(true);
    expect(quota().value).toBe("8");
    expect(quotaSave().disabled).toBe(true);
    expect((screen.getByLabelText("Idle lease") as HTMLInputElement).value).toBe("30m");
  });
});

describe("AdminWorkers worker sizes (PRD #4213)", () => {
  const optionTexts = (label: string) =>
    [...(screen.getByLabelText(label) as HTMLSelectElement).options].filter((o) => !o.disabled).map((o) => o.textContent);

  it("feeds the default-size selectors from the sizes list and refreshes them after a save", async () => {
    mockApi.getAdminWorkerSizes
      .mockResolvedValueOnce(adminSizes())
      .mockResolvedValueOnce(adminSizes({ l: { memory_limit_mib: 24576, customized: true } }));
    mockApi.updateAdminWorkerSize.mockResolvedValue({
      size: adminSizes({ l: { memory_limit_mib: 24576, customized: true } }).sizes[2],
      warnings: [],
      advisories: [],
    });
    renderPage();
    await waitFor(() => expect(optionTexts("Default size for new workers")).toContain("L — up to 4 CPU / 20Gi RAM / 50Gi disk"));
    expect(optionTexts("Ephemeral default size")).toContain("L — up to 4 CPU / 20Gi RAM / 50Gi disk");

    fireEvent.click(screen.getByRole("button", { name: "Edit size L" }));
    fireEvent.change(screen.getByLabelText("L Memory limit (GiB)"), { target: { value: "24" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size L" }));

    await waitFor(() => expect(mockApi.updateAdminWorkerSize).toHaveBeenCalledWith("l", expect.objectContaining({ memory_limit_mib: 24576 })));
    // The page re-read the list and lifted it: both selectors on this page show 24Gi at once.
    await waitFor(() => expect(optionTexts("Default size for new workers")).toContain("L — up to 4 CPU / 24Gi RAM / 50Gi disk"));
    expect(optionTexts("Ephemeral default size")).toContain("L — up to 4 CPU / 24Gi RAM / 50Gi disk");
    expect(optionTexts("Default size for new workers")).not.toContain("L — up to 4 CPU / 20Gi RAM / 50Gi disk");
    expect(mockApi.getAdminWorkerSizes).toHaveBeenCalledTimes(2);
  });

  it("lists an added size in both default-size selectors at once, in display order, and saves it as a default", async () => {
    const xs = {
      name: "xs", cpu_request_millicores: 250, cpu_limit_millicores: 1000, memory_request_mib: 1024, memory_limit_mib: 2048,
      data_gib: 5, dind_data_gib: 10, display_order: 4, builtin: false, customized: false, shipped: null, updated_at: "2026-11-19T09:00:00Z",
    };
    const withXs = { ...adminSizes(), sizes: [...adminSizes().sizes, xs] };
    mockApi.getAdminWorkerSizes.mockResolvedValueOnce(adminSizes()).mockResolvedValueOnce(withXs);
    mockApi.createAdminWorkerSize.mockResolvedValue({ size: xs, warnings: [], advisories: [] });
    mockApi.updateSettings.mockResolvedValue(response({ hosted_default_size: "xs" }, { hosted_default_size: "db" }) as never);
    renderPage();
    await waitFor(() => expect(optionTexts("Default size for new workers")).toHaveLength(4));
    expect(optionTexts("Default size for new workers")).not.toContain("XS — up to 1 CPU / 2Gi RAM / 5Gi disk");

    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fireEvent.change(screen.getByLabelText("New size name"), { target: { value: "xs" } });
    for (const [label, value] of [["CPU request (cores)", "0.25"], ["CPU limit (cores)", "1"], ["Memory request (GiB)", "1"], ["Memory limit (GiB)", "2"], ["Data disk (GiB)", "5"], ["DinD data disk (GiB)", "10"]]) {
      fireEvent.change(screen.getByLabelText(`New size ${label}`), { target: { value } });
    }
    fireEvent.click(screen.getByRole("button", { name: "Add size XS" }));
    await waitFor(() => expect(mockApi.createAdminWorkerSize).toHaveBeenCalledWith("xs", {
      cpu_request_millicores: 250, cpu_limit_millicores: 1000, memory_request_mib: 1024, memory_limit_mib: 2048, data_gib: 5, dind_data_gib: 10,
    }));
    // The page re-read the list: both selectors offer XS last, after the builtins.
    await waitFor(() =>
      expect(optionTexts("Default size for new workers")).toEqual([
        "S — up to 1 CPU / 4Gi RAM / 5Gi disk",
        "M — up to 4 CPU / 12Gi RAM / 25Gi disk",
        "L — up to 4 CPU / 20Gi RAM / 50Gi disk",
        "L-HIGHMEM — up to 4 CPU / 32Gi RAM / 50Gi disk",
        "XS — up to 1 CPU / 2Gi RAM / 5Gi disk",
      ]),
    );
    expect(optionTexts("Ephemeral default size")).toContain("XS — up to 1 CPU / 2Gi RAM / 5Gi disk");

    fireEvent.change(screen.getByLabelText("Default size for new workers"), { target: { value: "xs" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Default size for new workers" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ hosted_default_size: "xs" }));
  });

  it("keeps the other cards usable when the sizes read fails", async () => {
    mockApi.getAdminWorkerSizes.mockRejectedValue(new (await import("../lib/apiError")).ApiError(500, "internal error"));
    renderPage();
    expect(await screen.findByRole("button", { name: "Retry loading worker sizes" })).toBeTruthy();
    // The selectors fall back to the builtin names, bare: no quantity they cannot vouch for.
    expect(optionTexts("Default size for new workers")).toEqual(["S", "M", "L", "L-HIGHMEM"]);
  });
});
