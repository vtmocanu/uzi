// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminSettings } from "./AdminSettings";
import { api, ApiError } from "../lib/api";

// Only the api object is swapped; ApiError and types stay real so the page's
// `instanceof ApiError` checks match what the mocked methods throw.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getSettings: vi.fn(),
      updateSettings: vi.fn(),
      vaultMigration: vi.fn(),
      listRepos: vi.fn(),
      getAgentSource: vi.fn(),
      syncAgentSource: vi.fn(),
      applyAgentSource: vi.fn(),
      resolveAgentSourceLatest: vi.fn(),
      updateCheckAgentSource: vi.fn(),
      getReleaseCheck: vi.fn(),
      checkReleaseNow: vi.fn(),
    },
  };
});
// The page re-resolves the admin's own theme after a save via useAuth().refresh.
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ refresh: vi.fn().mockResolvedValue(undefined) }) }));

const mockApi = vi.mocked(api);

// A full AppSettings fixture; tests override the fields they exercise.
const settings = (over: Partial<import("../lib/api").AppSettings> = {}) => ({
  autopilot_label: "autopilot",
  uzi_label: "uzi",
  default_theme: "ember",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
  ephemeral_workers_enabled: "false",
  release_check_enabled: "true",
  release_check_banner_enabled: "true",
  summary_model: "haiku",
  health_enabled: "true",
  health_stall_seconds: "300",
  health_slow_seconds: "2700",
  health_queued_seconds: "600",
  health_approval_seconds: "3600",
  health_nudge_cooldown_seconds: "1800",
  docker_repo_allowlist: "",
  capability_aware_scheduling: "true",
  github_project_sync_enabled: "false",
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

// A full SettingsResponse fixture. Secrets default to not-configured and every
// key to source "default"; tests override just what they exercise.
const response = (
  over: Partial<import("../lib/api").AppSettings> = {},
  secrets: Record<string, boolean> = {},
  sources: Record<string, Src> = {},
  slack_status = "disabled",
) => ({
  settings: settings(over),
  secrets: { slack_bot_token: false, slack_app_token: false, ...secrets },
  sources: {
    uzi_label: "db",
    autopilot_label: "default",
    default_theme: "default",
    slack_enabled: "default",
    public_base_url: "default",
    slack_bot_token: "default",
    slack_app_token: "default",
    ...sources,
  } as Record<string, Src>,
  slack_status,
});

// A fresh-install agent-source view: empty URL, disabled, nothing staged. Used as
// the default so the card renders cleanly in the other cards' tests.
const emptyAgentSource = (): import("../lib/api").AgentSourceView => ({
  config: { url: "", ref: "", folder: ".claude/agents", enabled: false, interval: "1h", credential_configured: false },
  status: {},
  staged: null,
});

// A configured view with a PENDING staged snapshot to review.
const pendingAgentSource = (): import("../lib/api").AgentSourceView => ({
  config: {
    url: "https://github.com/uzi-dev/agents.git",
    ref: "v1.2.0",
    folder: ".claude/agents",
    enabled: true,
    interval: "1h",
    credential_configured: true,
  },
  status: {
    last_sync_at: "2026-08-23T09:00:00Z",
    last_sync_sha: "9f2c17a4e0b3d6c8a1f5",
    last_sync_status: "ok",
    last_applied_at: "2026-08-21T09:00:00Z",
    last_applied_sha: "3b7d0e51c9a2f4681db0",
    counts: { staged: 2, changed: 2, failed: 1 },
  },
  staged: {
    fetched_at: "2026-08-23T09:00:00Z",
    fetched_sha: "9f2c17a4e0b3d6c8a1f5",
    source_url: "https://github.com/uzi-dev/agents.git",
    source_ref: "v1.2.0",
    roles: [
      {
        name: "planner",
        ok: true,
        description: "Plans work.",
        prompt_body: "You are the planner.",
        // The raw body carried hidden control/bidi chars stripped for this preview.
        body_sanitized: true,
      },
      { name: "broken-role", ok: false, reason: "invalid" },
    ],
    diff: [
      { name: "planner", action: "add", detail: "new synced-only role" },
      { name: "release-notes", action: "conflict", detail: "collides with an admin global template" },
      { name: "migration-writer", action: "remove", detail: "no longer in the source" },
    ],
    counts: { staged: 2, changed: 2, failed: 1 },
    pending: true,
  },
});

// A release-check status fixture (PRD #836 M5). The default is the "behind" happy
// path: a check has run, a newer release exists, both toggles on. Tests override the
// fields they exercise.
const releaseCheck = (
  over: Partial<import("../lib/api").ReleaseCheckStatus> = {},
): import("../lib/api").ReleaseCheckStatus => ({
  release_check_enabled: true,
  release_check_banner_enabled: true,
  interval: "6h",
  running_version: "0.4.2",
  latest_tag: "v0.5.0",
  latest_name: "Hosted worker drain controls",
  body: "### Added\n- Worker drain deadline controls (#812)\n- Per-run cost roll-up (#799)\n",
  notes_url: "https://github.com/vtmocanu/uzi/releases/tag/v0.5.0",
  published_at: "2026-08-27T00:00:00Z",
  checked_at: "2026-08-30T00:00:00Z",
  update_available: true,
  far_behind: false,
  security: false,
  status: "ok",
  ...over,
});

beforeEach(() => {
  mockApi.getSettings.mockResolvedValue(response());
  mockApi.getReleaseCheck.mockResolvedValue({ release_check: releaseCheck() });
  mockApi.checkReleaseNow.mockResolvedValue({ release_check: releaseCheck() });
  mockApi.vaultMigration.mockResolvedValue({ master_sealed: 0 });
  mockApi.listRepos.mockResolvedValue({
    repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }] as unknown as import("../lib/api").Repo[],
  });
  mockApi.getAgentSource.mockResolvedValue({ agent_source: emptyAgentSource() });
  mockApi.syncAgentSource.mockResolvedValue({ agent_source: emptyAgentSource() });
  mockApi.resolveAgentSourceLatest.mockResolvedValue({ latest_ref: "v1.10.0" });
  mockApi.updateCheckAgentSource.mockResolvedValue({ agent_source: emptyAgentSource() });
  mockApi.applyAgentSource.mockResolvedValue({
    result: {
      sha: "9f2c17a4e0b3d6c8a1f5",
      applied: 1,
      unchanged: 0,
      conflicts: 1,
      deprovisioned: 1,
      skipped_parse: 1,
      already_applied: false,
      message: "applied",
    },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPage() {
  return render(
    <MemoryRouter>
      <AdminSettings />
    </MemoryRouter>,
  );
}

const input = (name: string) => screen.getByLabelText(name) as HTMLInputElement;
const saveButton = () => screen.getByRole("button", { name: /save settings/i }) as HTMLButtonElement;

describe("AdminSettings — vault migration (PRD #32)", () => {
  it("hides the migration notice when nothing is master-sealed", async () => {
    mockApi.vaultMigration.mockResolvedValue({ master_sealed: 0 });
    renderPage();
    await screen.findByLabelText("uzi label");
    expect(screen.queryByText(/pre-vault encryption/i)).toBeNull();
  });

  it("shows the migration notice with the count when secrets are still master-sealed", async () => {
    mockApi.vaultMigration.mockResolvedValue({ master_sealed: 3 });
    renderPage();
    await waitFor(() => expect(screen.getByText(/pre-vault encryption/i)).toBeTruthy());
    expect(screen.getByText(/3 stored secrets/i)).toBeTruthy();
  });
});

describe("AdminSettings", () => {
  it("renders the always-visible admin-settings guide link", async () => {
    renderPage();
    const link = await screen.findByRole("link", { name: "admin settings" });
    expect(link.getAttribute("href")).toBe("/docs/admin-settings");
  });

  it("loads the current labels into the fields", async () => {
    renderPage();
    const uzi = (await screen.findByLabelText("uzi label")) as HTMLInputElement;
    expect(uzi.value).toBe("uzi");
    expect(input("Autopilot label").value).toBe("autopilot");
    // Save is disabled until a field changes.
    expect(saveButton().disabled).toBe(true);
  });

  it("saves the edited uzi label and shows a success notice", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ uzi_label: "runnable" }));
    renderPage();
    await screen.findByLabelText("uzi label");
    fireEvent.change(input("uzi label"), { target: { value: "runnable" } });

    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        uzi_label: "runnable",
        autopilot_label: "autopilot",
        default_theme: "ember",
      }),
    );
    // A changed label mentions the next-sync propagation (N1).
    expect(await screen.findByText(/Boards reflect the label change after the next sync/i)).toBeTruthy();
    // Back to a clean (non-dirty) state after a successful save.
    expect(saveButton().disabled).toBe(true);
  });

  it("saves the default theme selection (PRD #21)", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ default_theme: "mission" }));
    renderPage();
    await screen.findByLabelText("uzi label");
    const theme = screen.getByLabelText("Default theme") as HTMLSelectElement;
    // Loads at the current instance default.
    expect(theme.value).toBe("ember");
    fireEvent.change(theme, { target: { value: "mission" } });

    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        uzi_label: "uzi",
        autopilot_label: "autopilot",
        default_theme: "mission",
      }),
    );
    // A theme-only change is presentation-only: the notice must NOT claim a resync (N1).
    expect(await screen.findByText("Settings saved.")).toBeTruthy();
    expect(screen.queryByText(/next sync/i)).toBeNull();
    expect(saveButton().disabled).toBe(true);
  });

  it("surfaces a server validation error", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "uzi_label already in use"));
    renderPage();
    await screen.findByLabelText("uzi label");
    fireEvent.change(input("uzi label"), { target: { value: "Backlog" } });
    fireEvent.click(saveButton());

    expect(await screen.findByText("uzi_label already in use")).toBeTruthy();
  });

  it("blocks equal labels client-side without calling the API", async () => {
    renderPage();
    await screen.findByLabelText("Autopilot label");
    fireEvent.change(input("Autopilot label"), { target: { value: "uzi" } });
    fireEvent.click(saveButton());

    // The danger Alert carries role="alert"; assert on it so the match is not
    // ambiguous with the section's help copy (which also mentions differing).
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/must differ/i);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  // ── Slack card (PRD #25 M1) ──────────────────────────────────────────────
  it("greys out Slack fields fixed by the environment", async () => {
    mockApi.getSettings.mockResolvedValue(
      response({}, { slack_bot_token: true }, { public_base_url: "env", slack_bot_token: "env" }),
    );
    renderPage();
    const baseUrl = (await screen.findByLabelText("Public base URL")) as HTMLInputElement;
    expect(baseUrl.disabled).toBe(true);
    expect((screen.getByLabelText("Bot token") as HTMLInputElement).disabled).toBe(true);
    // The "set from environment" hint appears for the greyed fields.
    expect(screen.getAllByText("Set from environment.").length).toBeGreaterThan(0);
  });

  it("marks a stored token configured without revealing it", async () => {
    mockApi.getSettings.mockResolvedValue(response({}, { slack_bot_token: true }));
    renderPage();
    const bot = (await screen.findByLabelText("Bot token")) as HTMLInputElement;
    // Write-only: the stored token is never pre-filled, only signalled.
    expect(bot.value).toBe("");
    expect(bot.placeholder).toMatch(/configured/i);
  });

  it("renders the live Slack connection status chip (PRD #25 M2)", async () => {
    mockApi.getSettings.mockResolvedValue(response({}, {}, {}, "connected"));
    renderPage();
    // The chip reflects the DTO's slack_status, not a hardcoded stub.
    expect(await screen.findByText("connected")).toBeTruthy();
    expect(screen.queryByText("disabled")).toBeNull();
  });

  it("saves the Slack card, sending only the entered token", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ slack_enabled: "true" }));
    renderPage();
    await screen.findByLabelText("Bot token");
    fireEvent.click(screen.getByLabelText(/Enable Slack notifications/i));
    fireEvent.change(screen.getByLabelText("Bot token"), { target: { value: "xoxb-new" } });

    const btn = screen.getByRole("button", { name: /save slack settings/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        slack_enabled: "true",
        public_base_url: "http://127.0.0.1:8080",
        slack_bot_token: "xoxb-new",
      }),
    );
    // The app token was left blank, so it is NOT sent.
    expect(mockApi.updateSettings.mock.calls[0][0]).not.toHaveProperty("slack_app_token");
  });

  it("saves the Run health card, sending only changed fields (PRD #47)", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ health_stall_seconds: "120" }));
    renderPage();
    const stall = (await screen.findByLabelText(/Stalled after/i)) as HTMLInputElement;
    fireEvent.change(stall, { target: { value: "120" } });

    const btn = screen.getByRole("button", { name: /save run health/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ health_stall_seconds: "120" }),
    );
    // Untouched thresholds are not sent.
    expect(mockApi.updateSettings.mock.calls[0][0]).not.toHaveProperty("health_slow_seconds");
  });

  it("rejects an out-of-range health threshold client-side and disables save (PRD #47)", async () => {
    renderPage();
    const stall = (await screen.findByLabelText(/Stalled after/i)) as HTMLInputElement;
    fireEvent.change(stall, { target: { value: "30" } }); // 1–59 is rejected

    expect(screen.getByText(/between 60 and 86400/i)).toBeTruthy();
    const btn = screen.getByRole("button", { name: /save run health/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it("rejects a non-digit health threshold for server parity (1e3, PRD #47)", async () => {
    // Number("1e3") === 1000 would pass a naive isInteger check, but the server's
    // strconv.Atoi rejects it — the digit-only client rule keeps them in lockstep.
    renderPage();
    const stall = (await screen.findByLabelText(/Stalled after/i)) as HTMLInputElement;
    fireEvent.change(stall, { target: { value: "1e3" } });

    expect(screen.getByText(/whole number of seconds/i)).toBeTruthy();
    const btn = screen.getByRole("button", { name: /save run health/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it("accepts 0 to disable a health signal (PRD #47)", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ health_queued_seconds: "0" }));
    renderPage();
    const queued = (await screen.findByLabelText(/Stuck queued after/i)) as HTMLInputElement;
    fireEvent.change(queued, { target: { value: "0" } });

    const btn = screen.getByRole("button", { name: /save run health/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ health_queued_seconds: "0" }),
    );
  });
});

describe("AdminSettings — run judge (PRD #46)", () => {
  it("loads the current global toggle and judge model", async () => {
    mockApi.getSettings.mockResolvedValue(response({ judge_enabled: "true", judge_model: "sonnet" }));
    renderPage();
    const toggle = (await screen.findByLabelText(/Enable the run judge for this instance/i)) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
    expect((screen.getByLabelText("Judge model") as HTMLInputElement).value).toBe("sonnet");
  });

  it("saves the global toggle and judge model together", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ judge_enabled: "true", judge_model: "opus" }));
    renderPage();
    await screen.findByLabelText(/Enable the run judge for this instance/i);
    fireEvent.click(screen.getByLabelText(/Enable the run judge for this instance/i));
    fireEvent.change(screen.getByLabelText("Judge model"), { target: { value: "opus" } });

    const btn = screen.getByRole("button", { name: /save run judge settings/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        judge_enabled: "true",
        judge_model: "opus",
        // enforce-all stays off (fixture default), the spend guards keep their loaded
        // values (PRD #69 M4/M5): the card sends the whole judge group on one save.
        judge_enforce_all: "false",
        judge_cooldown_seconds: "60",
        judge_daily_budget: "0",
      }),
    );
  });

  it("greys the enforce-all toggle while the judge kill-switch is off, and sends it on once enabled", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ judge_enabled: "true", judge_enforce_all: "true" }));
    renderPage();
    const enforce = (await screen.findByLabelText(/Enforce the judge on every run/i)) as HTMLInputElement;
    // Kill-switch off (fixture default): the enforce toggle is disabled.
    expect(enforce.disabled).toBe(true);
    // Turning the judge on frees the enforce toggle; then enforce + save.
    fireEvent.click(screen.getByLabelText(/Enable the run judge for this instance/i));
    expect((screen.getByLabelText(/Enforce the judge on every run/i) as HTMLInputElement).disabled).toBe(false);
    fireEvent.click(screen.getByLabelText(/Enforce the judge on every run/i));
    fireEvent.click(screen.getByRole("button", { name: /save run judge settings/i }));
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith(
        expect.objectContaining({ judge_enabled: "true", judge_enforce_all: "true" }),
      ),
    );
  });

  it("blocks an empty judge model client-side without calling the API", async () => {
    renderPage();
    await screen.findByLabelText("Judge model");
    fireEvent.change(screen.getByLabelText("Judge model"), { target: { value: "  " } });
    fireEvent.click(screen.getByRole("button", { name: /save run judge settings/i }));
    expect(await screen.findByText(/judge model must not be empty/i)).toBeTruthy();
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });
});

describe("AdminSettings — ephemeral workers (PRD #649 M1)", () => {
  it("loads the current ephemeral-workers kill-switch value", async () => {
    mockApi.getSettings.mockResolvedValue(response({ ephemeral_workers_enabled: "true" }));
    renderPage();
    const toggle = (await screen.findByLabelText(
      /Auto-provision workers on demand for this instance/i,
    )) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
  });

  it("reflects the seeded off value and stays disabled until toggled", async () => {
    renderPage();
    const toggle = (await screen.findByLabelText(
      /Auto-provision workers on demand for this instance/i,
    )) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    const btn = screen.getByRole("button", {
      name: /save ephemeral workers settings/i,
    }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it("saves the flipped kill-switch as a string bool", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ ephemeral_workers_enabled: "true" }));
    renderPage();
    await screen.findByLabelText(/Auto-provision workers on demand for this instance/i);
    fireEvent.click(screen.getByLabelText(/Auto-provision workers on demand for this instance/i));

    const btn = screen.getByRole("button", {
      name: /save ephemeral workers settings/i,
    }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith(
        expect.objectContaining({ ephemeral_workers_enabled: "true" }),
      ),
    );
  });
});

describe("AdminSettings — run summaries (PRD #362)", () => {
  it("loads the current summary model", async () => {
    mockApi.getSettings.mockResolvedValue(response({ summary_model: "sonnet" }));
    renderPage();
    expect((await screen.findByLabelText("Summary model") as HTMLInputElement).value).toBe("sonnet");
  });

  it("saves the summary model through the settings update path", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ summary_model: "opus" }));
    renderPage();
    fireEvent.change(await screen.findByLabelText("Summary model"), { target: { value: "opus" } });
    const btn = screen.getByRole("button", { name: /save run summary settings/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ summary_model: "opus" }));
  });

  it("blocks an empty summary model client-side without calling the API", async () => {
    renderPage();
    await screen.findByLabelText("Summary model");
    fireEvent.change(screen.getByLabelText("Summary model"), { target: { value: "  " } });
    fireEvent.click(screen.getByRole("button", { name: /save run summary settings/i }));
    expect(await screen.findByText(/summary model must not be empty/i)).toBeTruthy();
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });
});


describe("AdminSettings — docker repo allowlist (PRD #89 M-allow)", () => {
  // The card edits a security control (which repos a docker worker may claim), so its
  // save logic is behaviorally pinned here, not just its rendering. Auditor Low: the
  // setting is GLOBAL but listRepos is per-user, so ids the editing admin cannot see
  // must be PRESERVED on save, never silently clobbered.
  const twoRepos = () =>
    mockApi.listRepos.mockResolvedValue({
      repos: [
        { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" },
        { id: "repo-two", path_with_namespace: "vtmocanu/two" },
      ] as unknown as import("../lib/api").Repo[],
    });
  const saveBtn = () =>
    screen.getByRole("button", { name: /save repo allowlist/i }) as HTMLButtonElement;

  it("keeps Save disabled until the selection changes (dirty-check)", async () => {
    mockApi.getSettings.mockResolvedValue(response({ docker_repo_allowlist: "repo-uzi" }));
    twoRepos();
    renderPage();

    // Selection equals the stored value → nothing to save.
    await screen.findByLabelText("vtmocanu/two");
    expect(saveBtn().disabled).toBe(true);

    fireEvent.click(screen.getByLabelText("vtmocanu/two"));
    expect(saveBtn().disabled).toBe(false);
  });

  it("saves the ticked repos as comma-separated ids and PRESERVES ids outside the admin's visibility", async () => {
    // repo-other is not in this admin's listRepos (another admin's connection).
    mockApi.getSettings.mockResolvedValue(response({ docker_repo_allowlist: "repo-uzi,repo-other" }));
    twoRepos();
    mockApi.updateSettings.mockResolvedValue(
      response({ docker_repo_allowlist: "repo-other,repo-two,repo-uzi" }),
    );
    renderPage();

    // The invisible id is surfaced as preserved, not dropped.
    expect(await screen.findByText(/outside your visibility \(preserved\)/i)).toBeTruthy();

    // Tick a visible repo and save. The write must still carry repo-other — the entry
    // this admin cannot see rides through untouched.
    fireEvent.click(await screen.findByLabelText("vtmocanu/two"));
    fireEvent.click(saveBtn());

    await waitFor(() => {
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        docker_repo_allowlist: "repo-other,repo-two,repo-uzi",
      });
    });
  });

  it("removing the only visible repo writes an empty (fail-closed) allowlist", async () => {
    mockApi.getSettings.mockResolvedValue(response({ docker_repo_allowlist: "repo-uzi" }));
    mockApi.listRepos.mockResolvedValue({
      repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }] as unknown as import("../lib/api").Repo[],
    });
    mockApi.updateSettings.mockResolvedValue(response({ docker_repo_allowlist: "" }));
    renderPage();

    // The stored repo is ticked; untick it and save → empty string (fail-closed).
    const box = (await screen.findByLabelText("vtmocanu/uzi")) as HTMLInputElement;
    expect(box.checked).toBe(true);
    fireEvent.click(box);
    fireEvent.click(saveBtn());

    await waitFor(() => {
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ docker_repo_allowlist: "" });
    });
  });

  it("does not show the out-of-visibility indicator when the repos list fails to load", async () => {
    mockApi.getSettings.mockResolvedValue(response({ docker_repo_allowlist: "repo-uzi,repo-other" }));
    mockApi.listRepos.mockRejectedValue(new Error("network"));
    renderPage();

    // The card reports the load failure but never the spurious "N outside your
    // visibility" count — repos never loaded, so it cannot be known, and it must not
    // promise a removal the admin can't perform.
    expect(await screen.findByText(/could not load repositories/i)).toBeTruthy();
    expect(screen.queryByText(/outside your visibility/i)).toBeNull();
  });
});

describe("AdminSettings — capability-aware scheduling kill-switch (PRD #84 M2)", () => {
  const toggle = () =>
    screen.getByLabelText(/enable capability-aware scheduling/i) as HTMLInputElement;
  const saveBtn = () =>
    screen.getByRole("button", { name: /save capability scheduling/i }) as HTMLButtonElement;

  it("renders the toggle checked (default ON) with Save disabled until changed", async () => {
    renderPage();
    await screen.findByText("Capability-aware scheduling");
    expect(toggle().checked).toBe(true);
    expect(saveBtn().disabled).toBe(true);

    fireEvent.click(toggle());
    expect(saveBtn().disabled).toBe(false);
  });

  it("saves the flag OFF through the settings PATCH", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ capability_aware_scheduling: "false" }));
    renderPage();
    await screen.findByText("Capability-aware scheduling");

    fireEvent.click(toggle());
    fireEvent.click(saveBtn());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ capability_aware_scheduling: "false" }),
    );
  });

  it("disables the toggle when the setting is fixed by the environment", async () => {
    mockApi.getSettings.mockResolvedValue(
      response({ capability_aware_scheduling: "true" }, {}, { capability_aware_scheduling: "env" }),
    );
    renderPage();
    await screen.findByText("Capability-aware scheduling");
    expect(toggle().disabled).toBe(true);
    expect(saveBtn().disabled).toBe(true);
  });
});

describe("AdminSettings — GitHub Projects sync kill-switch (issue #534 M2)", () => {
  const toggle = () => screen.getByLabelText(/enable github projects sync/i) as HTMLInputElement;
  const saveBtn = () =>
    screen.getByRole("button", { name: /save github projects sync/i }) as HTMLButtonElement;

  it("renders the toggle unchecked (default OFF) with Save disabled until changed", async () => {
    // The fixture defaults github_project_sync_enabled to "false".
    renderPage();
    await screen.findByText("GitHub Projects sync");
    expect(toggle().checked).toBe(false);
    expect(saveBtn().disabled).toBe(true);

    fireEvent.click(toggle());
    expect(saveBtn().disabled).toBe(false);
  });

  it("reflects the served value ON when github_project_sync_enabled is \"true\"", async () => {
    mockApi.getSettings.mockResolvedValue(response({ github_project_sync_enabled: "true" }));
    renderPage();
    await screen.findByText("GitHub Projects sync");
    expect(toggle().checked).toBe(true);
    // Already at the served value, so nothing is dirty.
    expect(saveBtn().disabled).toBe(true);
  });

  it("saves the flag ON through the settings PATCH, sending only that key", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ github_project_sync_enabled: "true" }));
    renderPage();
    await screen.findByText("GitHub Projects sync");

    fireEvent.click(toggle());
    fireEvent.click(saveBtn());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ github_project_sync_enabled: "true" }),
    );
    expect(mockApi.updateSettings.mock.calls[0][0]).toEqual({ github_project_sync_enabled: "true" });
  });

  it("saves the flag back OFF when the served value is ON", async () => {
    mockApi.getSettings.mockResolvedValue(response({ github_project_sync_enabled: "true" }));
    mockApi.updateSettings.mockResolvedValue(response({ github_project_sync_enabled: "false" }));
    renderPage();
    await screen.findByText("GitHub Projects sync");

    fireEvent.click(toggle());
    fireEvent.click(saveBtn());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ github_project_sync_enabled: "false" }),
    );
  });

  it("links to the GitHub Projects sync guide", async () => {
    renderPage();
    const link = await screen.findByRole("link", { name: "GitHub Projects v2 sync" });
    expect(link.getAttribute("href")).toBe("/docs/github-project-sync");
  });

  it("disables the toggle when the setting is fixed by the environment", async () => {
    mockApi.getSettings.mockResolvedValue(
      response({ github_project_sync_enabled: "true" }, {}, { github_project_sync_enabled: "env" }),
    );
    renderPage();
    await screen.findByText("GitHub Projects sync");
    expect(toggle().disabled).toBe(true);
    expect(saveBtn().disabled).toBe(true);
  });
});

describe("AdminSettings — forge labels (PRD #764)", () => {
  it("renders the uzi and autopilot label fields, and the eligible-set label editors are gone", async () => {
    renderPage();
    // The single run-eligibility label field is present and loads the configured value.
    const uzi = (await screen.findByLabelText("uzi label")) as HTMLInputElement;
    expect(uzi.value).toBe("uzi");
    expect((screen.getByLabelText("Autopilot label") as HTMLInputElement).value).toBe("autopilot");
    // The old multi-value label editors (the TagInputs) are gone: the section renders no
    // "add a label…" affordance. This pairs with the positive uzi assertion above, so it
    // is not a vacuous never-rendered-string check.
    expect(screen.queryByPlaceholderText("add a label…")).toBeNull();
    // The retired waiver checkbox is gone too.
    expect(screen.queryByLabelText(/waives the requirement/i)).toBeNull();
  });
});

describe("AdminSettings — agent source (PRD #602 M5)", () => {
  // The two Interval / Repository labels also occur in the Self-improvement card, so
  // every agent-source query is scoped to the card's own section container.
  const card = () => within(document.getElementById("agent-source") as HTMLElement);

  it("reads cleanly on a fresh install: empty URL, never synced, Sync now disabled", async () => {
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    // Fresh state: URL blank, no credential, and the status reads "never synced"
    // rather than an empty panel.
    expect((c.getByLabelText("Repository URL") as HTMLInputElement).value).toBe("");
    expect(c.getByText("never synced")).toBeTruthy();
    // Nothing configured ⇒ Sync now is disabled and nothing is staged for review.
    expect((c.getByRole("button", { name: "Sync now" }) as HTMLButtonElement).disabled).toBe(true);
    expect(c.queryByText(/review needed/i)).toBeNull();
    // The credential field signals "not configured" without ever revealing a value.
    expect((c.getByLabelText("Access credential") as HTMLInputElement).value).toBe("");
    expect(c.getByText(/No credential is set/i)).toBeTruthy();
  });

  it("Preset button fills URL, folder, and the resolved latest tag from one click (PRD #702 M3)", async () => {
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    fireEvent.click(c.getByRole("button", { name: /use uzi skills preset/i }));

    // The resolve resolves to the mock's latest tag; all three inputs fill from one click.
    await waitFor(() =>
      expect(mockApi.resolveAgentSourceLatest).toHaveBeenCalledWith("https://github.com/vtmocanu/skills"),
    );
    await waitFor(() =>
      expect((c.getByLabelText("Ref") as HTMLInputElement).value).toBe("v1.10.0"),
    );
    expect((c.getByLabelText("Repository URL") as HTMLInputElement).value).toBe(
      "https://github.com/vtmocanu/skills",
    );
    expect((c.getByLabelText("Source folder") as HTMLInputElement).value).toBe("product-agents/");
    // The success feedback names the resolved tag AND renders INLINE inside the preset
    // block, co-located with the button (PRD #702 M3 UX fix) — not in the card-level
    // banner pinned far above. Scope the query to the button's own bordered container so
    // the assertion verifies the inline location, not just that the text exists somewhere.
    const presetBlock = c.getByRole("button", { name: /use uzi skills preset/i })
      .closest("div") as HTMLElement;
    const inline = within(presetBlock).getByText(/resolved latest tag v1\.10\.0/i);
    expect(inline).toBeTruthy();
    // role="status" (success tone) so screen readers still announce the outcome.
    expect(inline.closest('[role="status"]')).not.toBeNull();
  });

  it("loads a configured view into the fields and marks the credential configured", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    expect((c.getByLabelText("Repository URL") as HTMLInputElement).value).toBe(
      "https://github.com/uzi-dev/agents.git",
    );
    expect((c.getByLabelText("Ref") as HTMLInputElement).value).toBe("v1.2.0");
    expect((c.getByLabelText(/Sync automatically/i) as HTMLInputElement).checked).toBe(true);
    // Write-only credential: never pre-filled, only signalled as configured.
    const cred = c.getByLabelText("Access credential") as HTMLInputElement;
    expect(cred.value).toBe("");
    expect(cred.placeholder).toMatch(/configured/i);
    // A healthy sync shows the short SHA of the last sync (it also appears on the
    // staged snapshot, which shares the SHA — so at least one, not exactly one).
    expect(c.getAllByText("9f2c17a4e0").length).toBeGreaterThan(0);
  });

  it("saves the config through updateSettings with the agent_source_* keys, omitting an untyped credential", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    fireEvent.change(c.getByLabelText("Ref"), { target: { value: "v1.3.0" } });
    fireEvent.click(c.getByRole("button", { name: /save agent-source settings/i }));

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        agent_source_repo_url: "https://github.com/uzi-dev/agents.git",
        agent_source_ref: "v1.3.0",
        agent_source_folder: ".claude/agents",
        agent_source_enabled: "true",
        agent_source_interval: "1h",
      }),
    );
    // The credential was left blank, so it is NOT sent (leave-unchanged semantics).
    expect(mockApi.updateSettings.mock.calls[0][0]).not.toHaveProperty("agent_source_credential");
  });

  it("sends the credential only when the admin types one", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    fireEvent.change(c.getByLabelText("Access credential"), { target: { value: "ghp_secret" } });
    fireEvent.click(c.getByRole("button", { name: /save agent-source settings/i }));
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith(
        expect.objectContaining({ agent_source_credential: "ghp_secret" }),
      ),
    );
  });

  it("blocks enabling with an empty URL client-side, without calling the API", async () => {
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    fireEvent.click(c.getByLabelText(/Sync automatically/i));
    fireEvent.click(c.getByRole("button", { name: /save agent-source settings/i }));
    expect(await screen.findByText(/Set a repository URL before enabling/i)).toBeTruthy();
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("surfaces the SSRF-allowlist rejection from the server", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "URL is not in the agent-source allowlist"));
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    fireEvent.change(c.getByLabelText("Repository URL"), { target: { value: "https://evil.example/agents.git" } });
    fireEvent.click(c.getByRole("button", { name: /save agent-source settings/i }));
    expect(await screen.findByText("URL is not in the agent-source allowlist")).toBeTruthy();
  });

  it("renders the pending staged diff with per-role actions and the counts", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    expect(c.getByText(/review needed/i)).toBeTruthy();
    // Each action classification renders its chip and its role name.
    expect(c.getByText("Add")).toBeTruthy();
    expect(c.getByText("Conflict")).toBeTruthy();
    expect(c.getByText("Remove")).toBeTruthy();
    expect(c.getByText("planner")).toBeTruthy();
    // A failed parse is still surfaced (role present, no diff entry).
    expect(c.getByText("broken-role")).toBeTruthy();
    // The counts summary reflects the snapshot.
    expect(c.getByText("1", { selector: "strong" })).toBeTruthy();
  });

  it("Approve calls applyAgentSource with the reviewed snapshot SHA and shows the counts", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    fireEvent.click(c.getByRole("button", { name: /approve & apply/i }));
    await waitFor(() =>
      // Bound to the exact snapshot the admin reviewed, not applied blind.
      expect(mockApi.applyAgentSource).toHaveBeenCalledWith("9f2c17a4e0b3d6c8a1f5"),
    );
    // After applying, the view is re-read and a success notice summarizes the outcome.
    expect(await screen.findByText(/Applied 1 change/i)).toBeTruthy();
    expect(mockApi.getAgentSource).toHaveBeenCalledTimes(2);
  });

  it("handles a 409 stale-approval by re-reading and telling the admin to re-review", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    mockApi.applyAgentSource.mockRejectedValue(new ApiError(409, "the staged snapshot changed"));
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    fireEvent.click(c.getByRole("button", { name: /approve & apply/i }));
    // The 409 re-reads the view (2nd getAgentSource) and surfaces a re-review message.
    expect(await screen.findByText(/changed since you reviewed it/i)).toBeTruthy();
    await waitFor(() => expect(mockApi.getAgentSource).toHaveBeenCalledTimes(2));
  });

  it("Sync now triggers a sync and prompts to review the staged changes", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    mockApi.syncAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    fireEvent.click(c.getByRole("button", { name: "Sync now" }));
    await waitFor(() => expect(mockApi.syncAgentSource).toHaveBeenCalled());
    expect(await screen.findByText(/Review the staged changes/i)).toBeTruthy();
  });

  it("does not enable Sync now from an unsaved URL edit — it reads the saved config", async () => {
    // Fresh install (emptyAgentSource): nothing is stored yet.
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    const sync = () => c.getByRole("button", { name: "Sync now" }) as HTMLButtonElement;
    expect(sync().disabled).toBe(true);
    // Typing a URL WITHOUT saving must not arm Sync now: the sync would run against the
    // still-empty STORED config ("No repository URL is configured"), so the button stays
    // gated on the saved config, not the local input.
    fireEvent.change(c.getByLabelText("Repository URL"), {
      target: { value: "https://github.com/uzi-dev/agents.git" },
    });
    expect(sync().disabled).toBe(true);
    expect(c.getByText(/Save a repository URL below to sync/i)).toBeTruthy();
  });

  it("Sync now preserves unsaved form edits and refreshes only the status/staged panels", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    mockApi.syncAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    // Edit the Ref without saving.
    fireEvent.change(c.getByLabelText("Ref"), { target: { value: "v9.9.9" } });
    expect((c.getByLabelText("Ref") as HTMLInputElement).value).toBe("v9.9.9");
    // On a configured source, Sync now stays enabled (a manual refresh shouldn't force
    // saving unrelated edits first), but a note makes clear it uses the saved config.
    const sync = c.getByRole("button", { name: "Sync now" }) as HTMLButtonElement;
    expect(sync.disabled).toBe(false);
    expect(c.getByText(/Sync now uses the saved configuration/i)).toBeTruthy();
    fireEvent.click(sync);
    await waitFor(() => expect(mockApi.syncAgentSource).toHaveBeenCalled());
    // The sync response (ref v1.2.0) must NOT clobber the admin's unsaved edit.
    expect((c.getByLabelText("Ref") as HTMLInputElement).value).toBe("v9.9.9");
  });

  it("flags a staged role whose preview body was sanitized of hidden characters", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: pendingAgentSource() });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    // Exactly one fixture role is body_sanitized (planner); broken-role is not, so the
    // hidden-characters warning appears once — for the flagged role only.
    const warnings = c.getAllByText(/hidden formatting characters were removed/i);
    expect(warnings.length).toBe(1);
  });

  // PRD #702 M4: the "update available" badge + bump-pin flow. A configured, tag-pinned
  // (v1.2.0) source with nothing staged and no update signal yet.
  const configuredAgentSource = (
    over: Partial<import("../lib/api").AgentSourceStatus> = {},
    ref = "v1.2.0",
  ): import("../lib/api").AgentSourceView => ({
    config: {
      url: "https://github.com/vtmocanu/skills",
      ref,
      folder: "product-agents",
      enabled: true,
      interval: "1h",
      credential_configured: true,
    },
    status: {
      last_sync_at: "2026-08-23T09:00:00Z",
      last_sync_sha: "9f2c17a4e0b3d6c8a1f5",
      last_sync_status: "ok",
      last_applied_at: "2026-08-21T09:00:00Z",
      last_applied_sha: "3b7d0e51c9a2f4681db0",
      ...over,
    },
    staged: null,
  });

  it("checks for updates, shows the badge + bump pin, and self-clears after a bump (PRD #702 M4)", async () => {
    // Fresh load: configured, no update signal yet ⇒ no badge.
    mockApi.getAgentSource.mockResolvedValue({ agent_source: configuredAgentSource() });
    // The update check derives update_available + names the newer tag (v1.10.0 > v1.2.0).
    mockApi.updateCheckAgentSource.mockResolvedValue({
      agent_source: configuredAgentSource({
        update_available: true,
        latest_ref: "v1.10.0",
        update_checked_at: "2026-08-25T10:00:00Z",
      }),
    });
    mockApi.updateSettings.mockResolvedValue(response());

    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();

    // No badge and no Bump pin before a check has run.
    expect(c.queryByText(/Update available/i)).toBeNull();
    expect(c.queryByRole("button", { name: /bump pin/i })).toBeNull();

    // Check for updates ⇒ the badge names the newer tag and Bump pin appears, and the
    // inline outcome (co-located with the control, not the card-top banner) confirms it.
    fireEvent.click(c.getByRole("button", { name: /check for updates/i }));
    expect(await c.findByText("Update available: v1.10.0")).toBeTruthy();
    expect(c.getByText("Update check complete.")).toBeTruthy();
    const bump = c.getByRole("button", { name: /bump pin to v1\.10\.0/i }) as HTMLButtonElement;

    // The next read (after the bump's refreshView) reflects the new saved ref and, being
    // equal to the latest tag, derives update_available false ⇒ the badge self-clears.
    mockApi.getAgentSource.mockResolvedValue({ agent_source: configuredAgentSource({}, "v1.10.0") });

    fireEvent.click(bump);
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ agent_source_ref: "v1.10.0" }),
    );
    // The ref field is bumped to the latest tag, the badge is gone, and the inline bump
    // confirmation renders beside the control.
    await waitFor(() => expect((c.getByLabelText("Ref") as HTMLInputElement).value).toBe("v1.10.0"));
    expect(c.queryByText(/Update available/i)).toBeNull();
    expect(c.getByText(/Pinned ref updated to v1\.10\.0/i)).toBeTruthy();
  });

  it("bump pin preserves unsaved form edits (only the ref changes) (PRD #702 M4)", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: configuredAgentSource() });
    mockApi.updateCheckAgentSource.mockResolvedValue({
      agent_source: configuredAgentSource({
        update_available: true,
        latest_ref: "v1.10.0",
        update_checked_at: "2026-08-25T10:00:00Z",
      }),
    });
    mockApi.updateSettings.mockResolvedValue(response());

    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();

    // The admin edits the folder but has NOT saved.
    fireEvent.change(c.getByLabelText("Source folder"), { target: { value: "custom-agents/" } });

    fireEvent.click(c.getByRole("button", { name: /check for updates/i }));
    const bump = (await c.findByRole("button", { name: /bump pin to v1\.10\.0/i })) as HTMLButtonElement;

    // refreshView re-reads status only (form untouched); setRef bumps only the ref.
    mockApi.getAgentSource.mockResolvedValue({ agent_source: configuredAgentSource({}, "v1.10.0") });
    fireEvent.click(bump);

    await waitFor(() => expect((c.getByLabelText("Ref") as HTMLInputElement).value).toBe("v1.10.0"));
    // The unsaved folder edit survives the bump — it was NOT reset from the saved config.
    expect((c.getByLabelText("Source folder") as HTMLInputElement).value).toBe("custom-agents/");
  });

  it("shows an inline 'no update' outcome when the check finds nothing newer (PRD #702 M4)", async () => {
    mockApi.getAgentSource.mockResolvedValue({ agent_source: configuredAgentSource() });
    // The check runs but the source has no newer tag ⇒ update_available stays false.
    mockApi.updateCheckAgentSource.mockResolvedValue({
      agent_source: configuredAgentSource({ update_checked_at: "2026-08-25T10:00:00Z" }),
    });

    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();

    fireEvent.click(c.getByRole("button", { name: /check for updates/i }));
    // The success signal is inline beside the control, not off-screen at the card top.
    expect(await c.findByText(/No update available — you're on the latest\./i)).toBeTruthy();
    // No "Update available: <ref>" badge and no Bump pin — nothing newer was found.
    expect(c.queryByText(/Update available:/i)).toBeNull();
    expect(c.queryByRole("button", { name: /bump pin/i })).toBeNull();
  });

  it("renders the branch 'Source moved' badge when latest_ref is empty (PRD #702 M4)", async () => {
    mockApi.getAgentSource.mockResolvedValue({
      agent_source: configuredAgentSource(
        { update_available: true, update_checked_at: "2026-08-25T10:00:00Z" },
        "main",
      ),
    });
    renderPage();
    await screen.findByLabelText("Repository URL");
    const c = card();
    // Branch-pinned "moved" signal: no version named, and no Bump pin (tag-mode only).
    expect(await c.findByText("Source moved")).toBeTruthy();
    expect(c.queryByRole("button", { name: /bump pin/i })).toBeNull();
  });
});
