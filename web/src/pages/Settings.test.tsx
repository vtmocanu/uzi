// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Settings } from "./Settings";
import { api, ApiError, type User } from "../lib/api";
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
      setAutopilotEnabled: vi.fn(),
      setWaitOnLimit: vi.fn(),
      setJudgeEnabled: vi.fn(),
      getMySettings: vi.fn(),
      putMySettings: vi.fn(),
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
const refresh = vi.fn();

const baseUser: User = {
  id: "u1",
  email: "vlad@uzi.local",
  display_name: "Robin Diaz",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  ci_autofix_enabled: false,
  wait_on_limit: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function mockAuth(user: User) {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
    prdlessLabel: "PRDLESS",
    prdlessEnabled: false,
    runEligibleLabels: ["PRD"],
    eligibleLabelWaivesPrdLink: true,
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    judgeEnforcedByAdmin: false,
    effectiveJudgeModel: "opus",
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh,
  });
}

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getMySettings.mockResolvedValue({ settings: { default_model: null, judge_model: null, summary_model: null, theme: null } });
  mockApi.putMySettings.mockResolvedValue({ settings: { default_model: null, judge_model: null, summary_model: null, theme: "mission" } });
  mockApi.getMySlack.mockResolvedValue({
    slack: { member_id: null, notify: true, resolved_id: null, confirmed: false, state: "unlinked", workspace: "connected" },
  });
  mockAuth(baseUser);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  document.documentElement.removeAttribute("data-theme");
});

describe("Settings — vault (PRD #32)", () => {
  it("shows the irrecoverability notice on the token card", () => {
    const { container } = render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    expect((container.textContent ?? "")).toMatch(/cannot be recovered/i);
  });

  it("Lock vault calls the API, refreshes, and confirms runs will queue", async () => {
    mockApi.vaultLock.mockResolvedValue(null);
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    fireEvent.click(screen.getByRole("button", { name: /lock vault/i }));

    await waitFor(() => expect(mockApi.vaultLock).toHaveBeenCalled());
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    // The success notice is unique to the lock confirmation (the card description
    // also mentions "waiting for vault unlock", so match the notice's own text).
    await waitFor(() => expect(screen.getByText(/Runs already in flight finish/i)).toBeTruthy());
  });
});

describe("Settings — Appearance theme picker (PRD #21)", () => {
  const themeSelect = () => screen.getByLabelText("Theme") as HTMLSelectElement;

  it("offers 'Use default (<name>)' plus each theme and selects a null override as default", async () => {
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(themeSelect()).toBeTruthy());
    // The default option is labelled with the instance default's name.
    expect(screen.getByRole("option", { name: /Use default \(Ember\)/i })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Mission control" })).toBeTruthy();
    // A null override selects "use default" (value "").
    expect(themeSelect().value).toBe("");
  });

  it("applies live (optimistic) and persists the override on change", async () => {
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(themeSelect()).toBeTruthy());
    fireEvent.change(themeSelect(), { target: { value: "mission" } });
    // Optimistic: <html data-theme> flips immediately, before the request resolves.
    expect(document.documentElement.dataset.theme).toBe("mission");
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ theme: "mission" }));
    // Reconciled by a session refresh (syncs the override + re-applies authoritative).
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("surfaces an error and refreshes (reverts) on a failed save", async () => {
    mockApi.putMySettings.mockRejectedValue(new ApiError(400, "unknown theme"));
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(themeSelect()).toBeTruthy());
    fireEvent.change(themeSelect(), { target: { value: "mission" } });
    expect(await screen.findByText("unknown theme")).toBeTruthy();
    // refresh is the revert mechanism: re-fetch me() and re-apply the server truth.
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });
});

// ── Sidebar meter opt-in (round-3 IA feedback) ───────────────────────────────
describe("Settings — per-token 'Show in sidebar' toggle", () => {
  const twoSecrets = [
    {
      id: "sec-1",
      kind: "anthropic_token",
      label: "default",
      is_default: true,
      auto_eligible: false,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    {
      id: "sec-2",
      kind: "anthropic_token",
      label: "console-key",
      is_default: false,
      auto_eligible: false,
      created_at: "2026-01-02T00:00:00Z",
      updated_at: "2026-01-02T00:00:00Z",
    },
  ];

  it("pins the default token's box checked and disabled, others unchecked and live", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: twoSecrets });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    const pinned = (await screen.findByLabelText(
      "Show default in the sidebar",
    )) as HTMLInputElement;
    expect(pinned.checked).toBe(true);
    expect(pinned.disabled).toBe(true);
    const extra = screen.getByLabelText("Show console-key in the sidebar") as HTMLInputElement;
    expect(extra.checked).toBe(false);
    expect(extra.disabled).toBe(false);
  });

  it("checking an extra saves the whole set over PUT /me/settings", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: twoSecrets });
    mockApi.putMySettings.mockResolvedValue({
      settings: { default_model: null, judge_model: null, summary_model: null, theme: null, sidebar_token_ids: ["sec-2"] },
    });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    const extra = (await screen.findByLabelText(
      "Show console-key in the sidebar",
    )) as HTMLInputElement;
    fireEvent.click(extra);
    await waitFor(() =>
      expect(mockApi.putMySettings).toHaveBeenCalledWith({ sidebar_token_ids: ["sec-2"] }),
    );
    await waitFor(() => expect(extra.checked).toBe(true));
  });
});
