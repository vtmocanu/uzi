// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Settings } from "./Settings";
import { api, type User } from "../lib/api";
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
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default",
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

// A resolved AppearanceState fixture. Default: dark mode, hall/ember pair, system
// typeface, no per-field overrides (so it tracks the instance defaults). Tests pass
// `over` to exercise a different painted polarity or a synthesised-legacy shape.
const baseAppearance = (
  over: Partial<import("../lib/api").AppearanceState> = {},
): import("../lib/api").AppearanceState => ({
  mode: "dark",
  light_theme: "hall",
  dark_theme: "ember",
  typeface: "system",
  overrides: { mode: null, light_theme: null, dark_theme: null, typeface: null },
  defaults: { mode: "dark", light_theme: "hall", dark_theme: "ember", typeface: "system" },
  ...over,
});

function mockAuth(user: User, appearance = baseAppearance()) {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    appearance,
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
  mockApi.getMySettings.mockResolvedValue({ settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null } });
  mockApi.putMySettings.mockResolvedValue({ settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: "mission" } });
  mockApi.getMySlack.mockResolvedValue({
    slack: { member_id: null, notify: true, resolved_id: null, confirmed: false, state: "unlinked", workspace: "connected" },
  });
  mockAuth(baseUser);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-font");
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
      settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null, sidebar_token_ids: ["sec-2"] },
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

// ── Item 2 (issue #961): a stable `secrets` identity while `data` is null ──
// Pre-fix `const secrets = data?.secrets ?? []` minted a fresh array identity on every
// render WHILE `data` was null (during the load, and after a load failure `data` stays
// null): `undefined ?? []` builds a new `[]` each render. AnthropicTokens' two fetching
// effects are keyed on `[secrets]` (listWorkers, getMyRateLimits), so every re-render in
// that window re-fired both requests and flickered the auto-fetch chips. The
// `useMemo(() => data?.secrets ?? [], [data])` fix memoizes the fallback on the (stable
// null) `data`, so a re-render with `data` unchanged reuses the same `[]` and the effects
// do not re-run. (Once `data` loads, `data?.secrets ?? []` already returns the stable
// `data.secrets` — the churn is null-window-only, which is why this test keeps data null.)
describe("Settings — stable secrets identity while data is null (item 2)", () => {
  it("does not re-fire the token card's [secrets]-keyed fetches on a re-render while data is null", async () => {
    // Fail the settings load so `data` stays null across re-renders (the churn window).
    mockApi.listSecrets.mockRejectedValue(new Error("load boom"));

    const { rerender } = render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    // Mount seeds the token card with secrets=[] and runs both effects once; the load then
    // fails and leaves data null. Wait for both to settle, then reset the counters.
    await waitFor(() => expect(mockApi.listWorkers).toHaveBeenCalled());
    await waitFor(() => expect(mockApi.getMyRateLimits).toHaveBeenCalled());
    await screen.findByText("Failed to load settings");
    mockApi.listWorkers.mockClear();
    mockApi.getMyRateLimits.mockClear();

    // Re-render Settings twice with `data` still null (no refetch: the hook's deps are []).
    rerender(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    rerender(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    // Flush passive effects so any re-run would be visible by the assertion.
    await act(async () => {
      await Promise.resolve();
    });

    // Fixed: memoized `[]` is stable across the re-renders, so neither effect re-runs.
    // Mutation-checked: reverting Settings.tsx to `data?.secrets ?? []` mints a fresh `[]`
    // each re-render and both fetches fire again here (observed red).
    expect(mockApi.listWorkers).not.toHaveBeenCalled();
    expect(mockApi.getMyRateLimits).not.toHaveBeenCalled();
  });
});
