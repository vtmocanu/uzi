// @vitest-environment jsdom
//
// The Autopilot section's note about repos linked to an issue tracker (PRD 4173 M6): autopilot
// has no per-repo control, so the per-user section says it does not act there, and only to a
// user who has such a repo.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, type Repo, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSecrets: vi.fn(),
      putAnthropicToken: vi.fn(),
      deleteAnthropicToken: vi.fn(),
      // PRD #104 M2 token CRUD, driven by the AnthropicTokens card.
      createAnthropicToken: vi.fn(),
      patchAnthropicToken: vi.fn(),
      deleteAnthropicTokenById: vi.fn(),
      // The token card reads workers so a delete can NAME the ones bound to the
      // token (PRD #104 D5). Settings renders that card, so its tree calls this —
      // resolved empty, since these tests are about vault/autopilot/judge/theme and
      // never open a delete confirmation.
      listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
      listRepos: vi.fn(),
      setAutopilotEnabled: vi.fn(),
      setPlanCrossCheckEnabled: vi.fn(),
      setCodeCrossCheckEnabled: vi.fn(),
      setWaitOnLimit: vi.fn(),
      setNotifyEarlyReset: vi.fn(),
      setUsageReserve: vi.fn(),
      setJudgeEnabled: vi.fn(),
      setAttributionEnabled: vi.fn(),
      getMySettings: vi.fn(),
      putMySettings: vi.fn(),
      setHarnessOverflow: vi.fn(),
      listAgentTemplates: vi.fn(),
      getTemplateAllocations: vi.fn(),
      vaultLock: vi.fn(),
      // The Claude limits card (PRD #53/#104) self-gates: an EMPTY token list is
      // how the API reports a token-less user since M5, so the card renders nothing
      // and stays out of these token/vault/theme assertions.
      getMyRateLimits: vi.fn().mockResolvedValue({ tokens: [] }),
      // The Notifications section (a child of Settings) loads its own state.
      getMySlack: vi.fn(),
      setMySlackNotify: vi.fn(),
      setMySlackOverride: vi.fn(),
      testMySlackDM: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const user: User = {
  id: "u1",
  email: "vlad@uzi.local",
  display_name: "Robin Diaz",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  ci_autofix_enabled: false,
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  ephemeral_docker_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default",
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

const LINK = {
  connection_id: "itc-1",
  project_id: "0-4",
  project_short_name: "ATL",
  tag_id: "6-2",
  tag_name: "uzi",
  linked_at: "2026-11-19T10:00:00Z",
};

const repo = (id: string, over: Partial<Repo> = {}) => ({ id, path_with_namespace: `acme/${id}`, ...over }) as Repo;

const NOTE = "Autopilot does not act on repos linked to an issue tracker.";

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    appearance: {
      mode: "dark",
      light_theme: "hall",
      dark_theme: "ember",
      typeface: "system",
      overrides: { mode: null, light_theme: null, dark_theme: null, typeface: null },
      defaults: { mode: "dark", light_theme: "hall", dark_theme: "ember", typeface: "system" },
    },
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    judgeEnforcedByAdmin: false,
    effectiveJudgeModel: "opus",
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
    updateUser: vi.fn(),
    serverUnreachable: false,
    retry: vi.fn(),
  });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getMySettings.mockResolvedValue({ settings: { default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null } });
  mockApi.getMySlack.mockResolvedValue({
    slack: { member_id: null, notify: true, resolved_id: null, confirmed: false, state: "unlinked", workspace: "connected" },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const renderPage = () =>
  render(
    <MemoryRouter>
      <RunDefaults />
    </MemoryRouter>,
  );

describe("RunDefaults: autopilot and repos linked to an issue tracker (PRD 4173 M6)", () => {
  it("shows the note to a user with a linked repo", async () => {
    mockApi.listRepos.mockResolvedValue({ repos: [repo("plain"), repo("linked", { issue_tracker_link: LINK })] });
    renderPage();
    expect(await screen.findByText(NOTE)).toBeTruthy();
  });

  it("shows no note to a user with no linked repo", async () => {
    mockApi.listRepos.mockResolvedValue({ repos: [repo("plain"), repo("other", { issue_tracker_link: null })] });
    renderPage();
    await screen.findByText("Enable autopilot for my account");
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    expect(screen.queryByText(NOTE)).toBeNull();
  });

  it("shows no note, and no error, when the repo list cannot be read", async () => {
    mockApi.listRepos.mockRejectedValue(new Error("boom"));
    renderPage();
    await screen.findByText("Enable autopilot for my account");
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    expect(screen.queryByText(NOTE)).toBeNull();
    expect(screen.queryByText(/boom/)).toBeNull();
  });
});
