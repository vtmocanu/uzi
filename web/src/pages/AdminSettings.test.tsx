// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
      getSelfimprove: vi.fn(),
      updateSelfimprove: vi.fn(),
      listRepos: vi.fn(),
    },
  };
});
// The page re-resolves the admin's own theme after a save via useAuth().refresh.
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ refresh: vi.fn().mockResolvedValue(undefined) }) }));

const mockApi = vi.mocked(api);

// A full AppSettings fixture; tests override the fields they exercise.
const settings = (over: Partial<import("../lib/api").AppSettings> = {}) => ({
  prd_label: "PRD",
  autopilot_label: "autopilot",
  default_theme: "ember",
  prdless_enabled: "true",
  prdless_label: "PRDLESS",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
  summary_model: "haiku",
  health_enabled: "true",
  health_stall_seconds: "300",
  health_slow_seconds: "2700",
  health_queued_seconds: "600",
  health_approval_seconds: "3600",
  health_nudge_cooldown_seconds: "1800",
  docker_repo_allowlist: "",
  capability_aware_scheduling: "true",
  run_eligible_labels: "PRD,bug",
  board_extra_labels: "bug",
  eligible_label_waives_prd_link: "true",
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
    prd_label: "db",
    autopilot_label: "default",
    default_theme: "default",
    prdless_enabled: "default",
    prdless_label: "default",
    slack_enabled: "default",
    public_base_url: "default",
    slack_bot_token: "default",
    slack_app_token: "default",
    ...sources,
  } as Record<string, Src>,
  slack_status,
});

const selfimproveConfig = (over: Partial<import("../lib/api").SelfimproveConfig> = {}) => ({
  enabled: false,
  interval: "48h",
  repo_id: null,
  repo_path: null,
  user_id: null,
  user_email: null,
  last_run_at: null,
  active: false,
  ...over,
});

beforeEach(() => {
  mockApi.getSettings.mockResolvedValue(response());
  mockApi.vaultMigration.mockResolvedValue({ master_sealed: 0 });
  mockApi.getSelfimprove.mockResolvedValue({ selfimprove: selfimproveConfig() });
  mockApi.updateSelfimprove.mockResolvedValue({ selfimprove: selfimproveConfig() });
  mockApi.listRepos.mockResolvedValue({
    repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }] as unknown as import("../lib/api").Repo[],
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
    await screen.findByLabelText("PRD label");
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
    const prd = (await screen.findByLabelText("PRD label")) as HTMLInputElement;
    expect(prd.value).toBe("PRD");
    expect(input("Autopilot label").value).toBe("autopilot");
    // Save is disabled until a field changes.
    expect(saveButton().disabled).toBe(true);
  });

  it("saves edited labels and shows a success notice", async () => {
    // The server re-pins the renamed primary into the eligible set, so the response
    // carries run_eligible_labels updated to match — otherwise the recomputed value
    // would leave the form dirty after save.
    mockApi.updateSettings.mockResolvedValue(
      response({ prd_label: "Feature", run_eligible_labels: "Feature,bug" }),
    );
    renderPage();
    await screen.findByLabelText("PRD label");
    fireEvent.change(input("PRD label"), { target: { value: "Feature" } });

    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        prd_label: "Feature",
        autopilot_label: "autopilot",
        default_theme: "ember",
        prdless_enabled: "true",
        prdless_label: "PRDLESS",
        // The pinned primary follows the renamed PRD label into the eligible set.
        run_eligible_labels: "Feature,bug",
        board_extra_labels: "bug",
        eligible_label_waives_prd_link: "true",
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
    await screen.findByLabelText("PRD label");
    const theme = screen.getByLabelText("Default theme") as HTMLSelectElement;
    // Loads at the current instance default.
    expect(theme.value).toBe("ember");
    fireEvent.change(theme, { target: { value: "mission" } });

    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        prd_label: "PRD",
        autopilot_label: "autopilot",
        default_theme: "mission",
        prdless_enabled: "true",
        prdless_label: "PRDLESS",
        run_eligible_labels: "PRD,bug",
        board_extra_labels: "bug",
        eligible_label_waives_prd_link: "true",
      }),
    );
    // A theme-only change is presentation-only: the notice must NOT claim a resync (N1).
    expect(await screen.findByText("Settings saved.")).toBeTruthy();
    expect(screen.queryByText(/next sync/i)).toBeNull();
    expect(saveButton().disabled).toBe(true);
  });

  it("disables the PRDLESS name field while the toggle is off (PRD #22 M1)", async () => {
    renderPage();
    const name = (await screen.findByLabelText("PRDLESS label")) as HTMLInputElement;
    const toggle = screen.getByLabelText(/Enable the PRDLESS escape hatch/i) as HTMLInputElement;
    // Loads enabled → name editable.
    expect(toggle.checked).toBe(true);
    expect(name.disabled).toBe(false);
    // Turning the feature off disables the name field.
    fireEvent.click(toggle);
    expect(name.disabled).toBe(true);
  });

  it("blocks a PRDLESS label colliding with the PRD label client-side", async () => {
    renderPage();
    await screen.findByLabelText("PRDLESS label");
    fireEvent.change(input("PRDLESS label"), { target: { value: "PRD" } });
    fireEvent.click(saveButton());

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/PRDLESS label must differ from the PRD label/i);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("surfaces a server validation error", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "prd_label already in use"));
    renderPage();
    await screen.findByLabelText("PRD label");
    fireEvent.change(input("PRD label"), { target: { value: "Backlog" } });
    fireEvent.click(saveButton());

    expect(await screen.findByText("prd_label already in use")).toBeTruthy();
  });

  it("blocks equal labels client-side without calling the API", async () => {
    renderPage();
    await screen.findByLabelText("Autopilot label");
    fireEvent.change(input("Autopilot label"), { target: { value: "PRD" } });
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

describe("AdminSettings — self-improvement (PRD #46 M5)", () => {
  it("shows the token-consent copy and the connected repo in the picker", async () => {
    renderPage();
    // The jump-link pill in the section index also says "Self-improvement", so target
    // the card heading by role.
    await screen.findByRole("heading", { name: "Self-improvement" });
    // The standing token-consent warning must be present.
    expect(screen.getByText(/your own Anthropic token/i)).toBeTruthy();
    expect(screen.getByText(/never merges to/i)).toBeTruthy();
    // The connected repo is offered in the picker.
    await waitFor(() => expect(screen.getByRole("option", { name: "vtmocanu/uzi" })).toBeTruthy());
  });

  it("enables with a chosen repo and interval; the body carries no user id (audit H3)", async () => {
    mockApi.updateSelfimprove.mockResolvedValue({
      selfimprove: selfimproveConfig({ enabled: true, repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", user_email: "vlad@uzi.local" }),
    });
    renderPage();
    // The jump-link pill in the section index also says "Self-improvement", so target
    // the card heading by role.
    await screen.findByRole("heading", { name: "Self-improvement" });

    fireEvent.click(screen.getByLabelText(/Enable the self-improvement job/i));
    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "repo-uzi" } });
    fireEvent.change(screen.getByLabelText("Interval"), { target: { value: "24h" } });
    fireEvent.click(screen.getByRole("button", { name: /save self-improvement settings/i }));

    await waitFor(() =>
      expect(mockApi.updateSelfimprove).toHaveBeenCalledWith({ enabled: true, interval: "24h", repo_id: "repo-uzi" }),
    );
    // Structurally impossible to send a user id — the update payload has no such field.
    const arg = mockApi.updateSelfimprove.mock.calls[0][0] as unknown as Record<string, unknown>;
    expect(arg).not.toHaveProperty("user_id");
  });

  it("blocks enabling without a repo, client-side, without calling the API", async () => {
    renderPage();
    // The jump-link pill in the section index also says "Self-improvement", so target
    // the card heading by role.
    await screen.findByRole("heading", { name: "Self-improvement" });
    fireEvent.click(screen.getByLabelText(/Enable the self-improvement job/i));
    fireEvent.click(screen.getByRole("button", { name: /save self-improvement settings/i }));
    expect(await screen.findByText(/Choose a repository/i)).toBeTruthy();
    expect(mockApi.updateSelfimprove).not.toHaveBeenCalled();
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

describe("AdminSettings — membership + eligible labels (PRD #196 M2)", () => {
  const runEligibleAdd = () => screen.getByLabelText("Add to Run-eligible labels") as HTMLInputElement;
  const boardExtrasAdd = () => screen.getByLabelText("Add to Also show on boards") as HTMLInputElement;

  it("pins the primary in the eligible set with no remove control, and lists the extras", async () => {
    // board_extra distinct from the eligible extra so "Remove bug" is unambiguous.
    mockApi.getSettings.mockResolvedValue(
      response({ run_eligible_labels: "PRD,bug", prd_label: "PRD", board_extra_labels: "documentation" }),
    );
    renderPage();
    await screen.findByLabelText("PRD label");
    // The primary renders as a "primary"-marked, non-removable tag. ("primary" also
    // occurs in the hint copy, so assert on presence, not uniqueness.)
    expect(screen.getAllByText("primary").length).toBeGreaterThanOrEqual(1);
    expect(screen.queryByLabelText("Remove PRD")).toBeNull();
    // The non-primary eligible label is removable.
    expect(screen.getByLabelText("Remove bug")).toBeTruthy();
  });

  it("adds an eligible label and persists it comma-joined with the pinned primary", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ run_eligible_labels: "PRD,bug,security" }));
    renderPage();
    await screen.findByLabelText("PRD label");
    fireEvent.change(runEligibleAdd(), { target: { value: "security" } });
    fireEvent.keyDown(runEligibleAdd(), { key: "Enter" });
    fireEvent.click(saveButton());
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith(
        expect.objectContaining({ run_eligible_labels: "PRD,bug,security" }),
      ),
    );
  });

  it("removes a non-primary eligible label and persists the shrunk set", async () => {
    mockApi.getSettings.mockResolvedValue(
      response({ run_eligible_labels: "PRD,bug", board_extra_labels: "documentation" }),
    );
    mockApi.updateSettings.mockResolvedValue(response({ run_eligible_labels: "PRD" }));
    renderPage();
    await screen.findByLabelText("PRD label");
    fireEvent.click(screen.getByLabelText("Remove bug"));
    fireEvent.click(saveButton());
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith(
        expect.objectContaining({ run_eligible_labels: "PRD" }),
      ),
    );
  });

  it("adds a board-extra label and persists board_extra_labels", async () => {
    mockApi.getSettings.mockResolvedValue(response({ board_extra_labels: "bug" }));
    mockApi.updateSettings.mockResolvedValue(response({ board_extra_labels: "bug,documentation" }));
    renderPage();
    await screen.findByLabelText("PRD label");
    fireEvent.change(boardExtrasAdd(), { target: { value: "documentation" } });
    fireEvent.click(screen.getAllByRole("button", { name: /^Add$/ })[1]);
    fireEvent.click(saveButton());
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith(
        expect.objectContaining({ board_extra_labels: "bug,documentation" }),
      ),
    );
  });

  it("saves the PRD-link waiver toggle as a strict bool", async () => {
    mockApi.getSettings.mockResolvedValue(response({ eligible_label_waives_prd_link: "true" }));
    mockApi.updateSettings.mockResolvedValue(response({ eligible_label_waives_prd_link: "false" }));
    renderPage();
    const waiver = (await screen.findByLabelText(/waives the PRD-link requirement/i)) as HTMLInputElement;
    expect(waiver.checked).toBe(true);
    fireEvent.click(waiver);
    fireEvent.click(saveButton());
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith(
        expect.objectContaining({ eligible_label_waives_prd_link: "false" }),
      ),
    );
  });

  it("greys the three keys when they are env-sourced and omits them from the save", async () => {
    mockApi.getSettings.mockResolvedValue(
      response({}, {}, {
        run_eligible_labels: "env",
        board_extra_labels: "env",
        eligible_label_waives_prd_link: "env",
      }),
    );
    mockApi.updateSettings.mockResolvedValue(response({ prd_label: "Feature", run_eligible_labels: "PRD,bug" }));
    renderPage();
    await screen.findByLabelText("PRD label");
    // Env-fixed: the add inputs and the waiver checkbox are disabled.
    expect(runEligibleAdd().disabled).toBe(true);
    expect(boardExtrasAdd().disabled).toBe(true);
    expect((screen.getByLabelText(/waives the PRD-link requirement/i) as HTMLInputElement).disabled).toBe(true);
    // A non-env change still saves, without the env-fixed keys in the payload.
    fireEvent.change(input("PRD label"), { target: { value: "Feature" } });
    fireEvent.click(saveButton());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalled());
    const payload = mockApi.updateSettings.mock.calls[0][0];
    expect(payload).not.toHaveProperty("run_eligible_labels");
    expect(payload).not.toHaveProperty("board_extra_labels");
    expect(payload).not.toHaveProperty("eligible_label_waives_prd_link");
  });

  it("blocks an eligible label colliding with the autopilot label client-side", async () => {
    renderPage();
    await screen.findByLabelText("PRD label");
    fireEvent.change(runEligibleAdd(), { target: { value: "autopilot" } });
    fireEvent.keyDown(runEligibleAdd(), { key: "Enter" });
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/must not include the autopilot label/i);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });
});
