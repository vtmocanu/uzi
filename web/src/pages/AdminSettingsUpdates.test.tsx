// @vitest-environment jsdom
// Co-located tests for the admin Updates card (PRD #836 M5). The whole AdminSettings
// page is rendered (the card is registered in its list); assertions scope to the
// card via its unique copy. Only the `api` object is swapped — ApiError and types
// stay real so `instanceof ApiError` checks match the mocked throws.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminSettings } from "./AdminSettings";
import { api, ApiError } from "../lib/api";

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
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ refresh: vi.fn().mockResolvedValue(undefined) }) }));

const mockApi = vi.mocked(api);

type Settings = import("../lib/api").AppSettings;
type Src = "env" | "db" | "default";

const settings = (over: Partial<Settings> = {}): Settings => ({
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
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_keep_name: "true",
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: "false",
  ...over,
});

const settingsResponse = () => ({
  settings: settings(),
  secrets: { slack_bot_token: false, slack_app_token: false },
  sources: {} as Record<string, Src>,
  slack_status: "disabled",
});

const emptyAgentSource = (): import("../lib/api").AgentSourceView => ({
  config: { url: "", ref: "", folder: ".claude/agents", enabled: false, interval: "1h", credential_configured: false },
  status: {},
  staged: null,
});

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
  mockApi.getSettings.mockResolvedValue(settingsResponse());
  mockApi.vaultMigration.mockResolvedValue({ master_sealed: 0 });
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.getAgentSource.mockResolvedValue({ agent_source: emptyAgentSource() });
  mockApi.getReleaseCheck.mockResolvedValue({ release_check: releaseCheck() });
  mockApi.checkReleaseNow.mockResolvedValue({ release_check: releaseCheck() });
  mockApi.updateSettings.mockResolvedValue(settingsResponse());
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

describe("AdminSettings — Updates card (PRD #836 M5)", () => {
  it("renders the version delta, notes excerpt, and copyable runbook when a newer release exists", async () => {
    renderPage();
    // Check-now button is unique to this card and only appears once loaded.
    await screen.findByRole("button", { name: /check now/i });
    expect(screen.getByText("v0.4.2")).toBeTruthy();
    expect(screen.getByText("v0.5.0")).toBeTruthy();
    expect(screen.getByText("Update available")).toBeTruthy();
    expect(screen.getByText("Hosted worker drain controls")).toBeTruthy();
    // Notes excerpt is plain text from the body (one of its bullets).
    expect(screen.getByText(/Worker drain deadline controls/)).toBeTruthy();
    // Runbook contains the helm upgrade command.
    expect(screen.getByText(/helm upgrade uzi oci:\/\/ghcr\.io\/vtmocanu\/uzi/)).toBeTruthy();
    // Link out to the full GitHub notes.
    const link = screen.getByRole("link", { name: /full notes on github/i }) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("https://github.com/vtmocanu/uzi/releases/tag/v0.5.0");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("shows an up-to-date state when no update is available", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({ update_available: false, latest_tag: "v0.4.2", latest_name: undefined }),
    });
    renderPage();
    await screen.findByRole("button", { name: /check now/i });
    expect(screen.getByText("Up to date")).toBeTruthy();
    expect(screen.getByText(/running the latest release/i)).toBeTruthy();
  });

  it("triggers a POST and refreshes the card when Check now is clicked", async () => {
    mockApi.checkReleaseNow.mockResolvedValue({
      release_check: releaseCheck({ latest_name: "Freshly re-checked release" }),
    });
    renderPage();
    const btn = await screen.findByRole("button", { name: /check now/i });
    fireEvent.click(btn);
    await waitFor(() => expect(mockApi.checkReleaseNow).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Freshly re-checked release")).toBeTruthy();
  });

  it("persists the master toggle through updateSettings with the string key", async () => {
    renderPage();
    await screen.findByRole("button", { name: /check now/i });
    fireEvent.click(screen.getByLabelText(/enable update checks/i));
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ release_check_enabled: "false" }),
    );
  });

  it("persists the banner toggle through updateSettings with the string key", async () => {
    renderPage();
    await screen.findByRole("button", { name: /check now/i });
    fireEvent.click(screen.getByLabelText(/escalation banner/i));
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ release_check_banner_enabled: "false" }),
    );
  });

  it("states the disabled (air-gap) status in words", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({ release_check_enabled: false, status: "disabled", update_available: false }),
    });
    renderPage();
    // The master toggle reflects the off state, and the air-gap wording renders.
    await waitFor(() => expect(screen.getByText(/Update checks are turned off/i)).toBeTruthy());
    const master = screen.getByLabelText(/enable update checks/i) as HTMLInputElement;
    expect(master.checked).toBe(false);
  });

  it("states the error status with the scrubbed message", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({ status: "error", message: "rate limited by github", update_available: false }),
    });
    renderPage();
    await waitFor(() => expect(screen.getByText(/rate limited by github/i)).toBeTruthy());
  });

  it("states the never-checked status in words", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({
        status: "never",
        update_available: false,
        latest_tag: undefined,
        latest_name: undefined,
        body: undefined,
        notes_url: undefined,
        published_at: undefined,
        checked_at: undefined,
      }),
    });
    renderPage();
    await waitFor(() => expect(screen.getByText(/No release check has run yet/i)).toBeTruthy());
  });

  it("renders an error with a retry affordance when the initial load fails, and retry can succeed", async () => {
    mockApi.getReleaseCheck
      .mockRejectedValueOnce(new ApiError(503, "release check unreachable"))
      .mockResolvedValueOnce({ release_check: releaseCheck({ latest_name: "Recovered on retry" }) });
    renderPage();
    // The dead-end skeleton is replaced by the error + a retry button; "Check now"
    // (gated on a loaded status) is absent while the load has failed.
    const retry = await screen.findByRole("button", { name: /retry/i });
    expect(screen.getByText(/release check unreachable/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /check now/i })).toBeNull();
    // Retry re-calls the API; the second (resolved) call loads the card body.
    fireEvent.click(retry);
    await screen.findByRole("button", { name: /check now/i });
    expect(screen.getByText("Recovered on retry")).toBeTruthy();
    expect(mockApi.getReleaseCheck).toHaveBeenCalledTimes(2);
  });

  it("strips markdown heading and bullet markers from the notes excerpt", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({
        body: "### Added\n- Worker drain deadline controls (#812)\n* Per-run cost roll-up (#799)\n",
      }),
    });
    renderPage();
    const excerpt = await screen.findByText(/Worker drain deadline controls/);
    // Plain text preserved, markers gone: "### Added" -> "Added", bullets dropped.
    expect(excerpt.textContent).toContain("Added");
    expect(excerpt.textContent).not.toContain("###");
    expect(excerpt.textContent).not.toContain("- ");
    expect(excerpt.textContent).not.toContain("* ");
  });

  it("shows the security badge on a security release", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({ security: true, latest_tag: "v0.5.1" }),
    });
    renderPage();
    await screen.findByRole("button", { name: /check now/i });
    // The badge carries the exact label; the danger callout additionally names the tag.
    expect(screen.getByText("Security release")).toBeTruthy();
    expect(screen.getByText(/flagged as a security release/i)).toBeTruthy();
  });
});
