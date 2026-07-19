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
  judge_model: "haiku",
  health_enabled: "true",
  health_stall_seconds: "300",
  health_slow_seconds: "2700",
  health_queued_seconds: "600",
  health_approval_seconds: "3600",
  health_nudge_cooldown_seconds: "1800",
  docker_repo_allowlist: "",
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
  it("loads the current labels into the fields", async () => {
    renderPage();
    const prd = (await screen.findByLabelText("PRD label")) as HTMLInputElement;
    expect(prd.value).toBe("PRD");
    expect(input("Autopilot label").value).toBe("autopilot");
    // Save is disabled until a field changes.
    expect(saveButton().disabled).toBe(true);
  });

  it("saves edited labels and shows a success notice", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ prd_label: "Feature" }));
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
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ judge_enabled: "true", judge_model: "opus" }),
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

describe("AdminSettings — self-improvement (PRD #46 M5)", () => {
  it("shows the token-consent copy and the connected repo in the picker", async () => {
    renderPage();
    await screen.findByText("Self-improvement");
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
    await screen.findByText("Self-improvement");

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
    await screen.findByText("Self-improvement");
    fireEvent.click(screen.getByLabelText(/Enable the self-improvement job/i));
    fireEvent.click(screen.getByRole("button", { name: /save self-improvement settings/i }));
    expect(await screen.findByText(/Choose a repository/i)).toBeTruthy();
    expect(mockApi.updateSelfimprove).not.toHaveBeenCalled();
  });
});
