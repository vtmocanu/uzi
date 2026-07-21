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
  display_name: "Vlad",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
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
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh,
  });
}

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getMySettings.mockResolvedValue({ settings: { default_model: null, theme: null } });
  mockApi.putMySettings.mockResolvedValue({ settings: { default_model: null, theme: "mission" } });
  mockApi.getMySlack.mockResolvedValue({
    slack: { member_id: null, notify: true, resolved_id: null, confirmed: false, state: "unlinked" },
  });
  mockAuth(baseUser);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  document.documentElement.removeAttribute("data-theme");
});

const toggle = () =>
  screen.getByLabelText("Enable autopilot for my account") as HTMLInputElement;

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

describe("Settings — autopilot opt-in (PRD #19 M3, Decision 7)", () => {
  it("states plainly what autopilot does", () => {
    const { container } = render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    const text = container.textContent ?? "";
    expect(text).toMatch(/unattended/i);
    expect(text).toMatch(/skips the pre-execution plan review/i);
    expect(text).toMatch(/your own Anthropic tokens/i);
    expect(text).toMatch(/merge-request review/i);
  });

  it("reflects the current opt-in state (off)", () => {
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    expect(toggle().checked).toBe(false);
  });

  it("reflects the current opt-in state (on)", () => {
    mockAuth({ ...baseUser, autopilot_enabled: true });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    expect(toggle().checked).toBe(true);
  });

  it("enabling calls the API and refreshes the session", async () => {
    mockApi.setAutopilotEnabled.mockResolvedValue({
      user: { ...baseUser, autopilot_enabled: true },
    });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    fireEvent.click(toggle());

    await waitFor(() =>
      expect(mockApi.setAutopilotEnabled).toHaveBeenCalledWith(true),
    );
    expect(refresh).toHaveBeenCalled();
  });

  it("surfaces an error and does not leave the toggle stuck", async () => {
    mockApi.setAutopilotEnabled.mockRejectedValue(
      new ApiError(500, "internal error"),
    );
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    fireEvent.click(toggle());

    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(toggle().disabled).toBe(false);
  });
});

describe("Settings — run judge opt-in (PRD #46, Decision 7)", () => {
  const judgeToggle = () =>
    screen.getByLabelText("Judge my finished runs") as HTMLInputElement;

  it("states plainly what the judge does and that it spends the user's tokens", () => {
    const { container } = render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    const text = container.textContent ?? "";
    expect(text).toMatch(/finished/i);
    expect(text).toMatch(/your own Anthropic tokens/i);
    expect(text).toMatch(/recommend/i);
  });

  it("reflects the current opt-in state", () => {
    mockAuth({ ...baseUser, judge_enabled: true });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    expect(judgeToggle().checked).toBe(true);
  });

  it("enabling calls the API and refreshes the session", async () => {
    mockApi.setJudgeEnabled.mockResolvedValue({ user: { ...baseUser, judge_enabled: true } });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    fireEvent.click(judgeToggle());
    await waitFor(() => expect(mockApi.setJudgeEnabled).toHaveBeenCalledWith(true));
    expect(refresh).toHaveBeenCalled();
  });

  it("surfaces an error and does not leave the toggle stuck", async () => {
    mockApi.setJudgeEnabled.mockRejectedValue(new ApiError(500, "internal error"));
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    fireEvent.click(judgeToggle());
    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(judgeToggle().disabled).toBe(false);
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

// ── Judge → token binding (PRD #104 M4/M6) ──────────────────────────────────
// Without this control the success criterion "the judge lane can burn a different
// token, set from the web UI" is unreachable, which is why it is required.

describe("Settings judge token picker (PRD #104)", () => {
  const twoTokens = [
    {
      id: "sec-default",
      kind: "anthropic_token",
      label: "default",
      is_default: true,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    {
      id: "sec-cheap",
      kind: "anthropic_token",
      label: "cheap-console",
      is_default: false,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
  ];

  it("offers no judge picker when the user holds a single token", async () => {
    mockAuth(baseUser);
    mockApi.listSecrets.mockResolvedValue({ secrets: [twoTokens[0]] });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    await screen.findByText("Run judge");
    expect(screen.queryByLabelText("Token the judge spends")).toBeNull();
  });

  it("points the judge lane at a named token, sending the label", async () => {
    mockAuth(baseUser);
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    mockApi.setJudgeEnabled.mockResolvedValue({ user: baseUser });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    const picker = await screen.findByLabelText("Token the judge spends");
    fireEvent.change(picker, { target: { value: "cheap-console" } });
    await waitFor(() =>
      // enabled rides along unchanged; the token is the label, not an id.
      expect(mockApi.setJudgeEnabled).toHaveBeenCalledWith(false, "cheap-console"),
    );
  });

  it("clears the judge binding back to the default by sending null", async () => {
    mockAuth({ ...baseUser, judge_anthropic_secret_id: "sec-cheap", judge_anthropic_secret_label: "cheap-console" });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    mockApi.setJudgeEnabled.mockResolvedValue({ user: baseUser });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    const picker = await screen.findByLabelText("Token the judge spends");
    fireEvent.change(picker, { target: { value: "" } });
    await waitFor(() => expect(mockApi.setJudgeEnabled).toHaveBeenCalledWith(false, null));
  });

  // Toggling the opt-in must NOT touch the binding: the token field is omitted,
  // not sent as null, or enabling the judge would silently unbind the credential.
  it("omits the token field when only the opt-in is toggled", async () => {
    mockAuth({ ...baseUser, judge_anthropic_secret_id: "sec-cheap", judge_anthropic_secret_label: "cheap-console" });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    mockApi.setJudgeEnabled.mockResolvedValue({ user: baseUser });
    render(
      <MemoryRouter>
        <Settings />
      </MemoryRouter>,
    );
    const toggle = await screen.findByLabelText("Judge my finished runs");
    fireEvent.click(toggle);
    await waitFor(() => expect(mockApi.setJudgeEnabled).toHaveBeenCalledWith(true));
  });
});
