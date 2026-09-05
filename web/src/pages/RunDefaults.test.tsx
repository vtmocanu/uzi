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
import { RunDefaults } from "./RunDefaults";
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
      setNotifyEarlyReset: vi.fn(),
      setJudgeEnabled: vi.fn(),
      setAttributionEnabled: vi.fn(),
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
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function mockAuth(user: User, over: Partial<ReturnType<typeof useAuth>> = {}) {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    judgeEnforcedByAdmin: false,
    effectiveJudgeModel: "opus",
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh,
    ...over,
  });
}

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getMySettings.mockResolvedValue({ settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null } });
  mockApi.putMySettings.mockResolvedValue({ settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: "mission" } });
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

const toggle = () =>
  screen.getByLabelText("Enable autopilot for my account") as HTMLInputElement;

describe("Run defaults — autopilot opt-in (PRD #19 M3, Decision 7)", () => {
  it("states plainly what autopilot does", () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
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
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(toggle().checked).toBe(false);
  });

  it("reflects the current opt-in state (on)", () => {
    mockAuth({ ...baseUser, autopilot_enabled: true });
    render(
      <MemoryRouter>
        <RunDefaults />
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
        <RunDefaults />
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
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(toggle());

    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(toggle().disabled).toBe(false);
  });
});

describe("Run defaults — run judge opt-in (PRD #46, Decision 7)", () => {
  const judgeToggle = () =>
    screen.getByLabelText("Judge my finished runs") as HTMLInputElement;

  it("states plainly what the judge does and that it spends the user's tokens", () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
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
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(judgeToggle().checked).toBe(true);
  });

  it("enabling calls the API and refreshes the session", async () => {
    mockApi.setJudgeEnabled.mockResolvedValue({ user: { ...baseUser, judge_enabled: true } });
    render(
      <MemoryRouter>
        <RunDefaults />
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
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(judgeToggle());
    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(judgeToggle().disabled).toBe(false);
  });
});

describe("Run defaults — judge enforced banner + per-user model (PRD #69 M4)", () => {
  it("shows no enforced banner when the admin has not enforced the judge", async () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Run judge");
    expect(container.textContent ?? "").not.toMatch(/ENFORCED/i);
  });

  it("renders the enforced banner naming the effective model and the token opt-out", async () => {
    mockAuth(baseUser, { judgeEnforcedByAdmin: true, effectiveJudgeModel: "sonnet" });
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Run judge");
    const text = container.textContent ?? "";
    expect(text).toMatch(/ENFORCED/);
    expect(text).toMatch(/model: sonnet/);
    expect(text).toMatch(/remove your Anthropic token/i);
  });

  it("saves the per-user judge model through PUT /me/settings", async () => {
    mockApi.getMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null },
    });
    mockApi.putMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: null, judge_model: "haiku", summary_model: null, theme: null },
    });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Run judge");
    // The judge-model picker offers a "haiku" option; selecting it dirties Save.
    const select = screen.getByLabelText("Judge model") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "haiku" } });
    fireEvent.click(screen.getByText("Save judge model"));
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ judge_model: "haiku" }));
  });
});

describe("Run defaults — per-user summary model (PRD #362 M2)", () => {
  it("renders the Run summaries card with the summary-model picker", async () => {
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Run summaries");
    expect(screen.getByLabelText("Summary model")).toBeTruthy();
  });

  it("saves the per-user summary model through PUT /me/settings", async () => {
    mockApi.getMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null },
    });
    mockApi.putMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: null, judge_model: null, summary_model: "haiku", theme: null },
    });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Run summaries");
    // The summary-model picker offers a "haiku" option; selecting it dirties Save.
    const select = screen.getByLabelText("Summary model") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "haiku" } });
    fireEvent.click(screen.getByText("Save summary model"));
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ summary_model: "haiku" }));
  });
});

describe("Run defaults — per-user reasoning effort (PRD #617 M5)", () => {
  it("loads and shows the saved effort", async () => {
    mockApi.getMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: "low", judge_model: null, summary_model: null, theme: null },
    });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Reasoning effort");
    const select = (await screen.findByLabelText("Effort")) as HTMLSelectElement;
    expect(select.value).toBe("low");
  });

  it("changing the dropdown enables Save and sends the chosen level", async () => {
    mockApi.getMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null },
    });
    mockApi.putMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: "low", judge_model: null, summary_model: null, theme: null },
    });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Reasoning effort");
    const select = (await screen.findByLabelText("Effort")) as HTMLSelectElement;
    const save = screen.getByText("Save effort") as HTMLButtonElement;
    // Inherit is the saved value, so Save starts disabled (not dirty).
    expect(save.disabled).toBe(true);
    fireEvent.change(select, { target: { value: "low" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ default_effort: "low" }));
  });

  it("selecting Inherit sends default_effort: null", async () => {
    mockApi.getMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: "max", judge_model: null, summary_model: null, theme: null },
    });
    mockApi.putMySettings.mockResolvedValue({
      settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null },
    });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Reasoning effort");
    const select = (await screen.findByLabelText("Effort")) as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "" } });
    fireEvent.click(screen.getByText("Save effort"));
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ default_effort: null }));
  });
});

describe("Run defaults judge token picker (PRD #104)", () => {
  const twoTokens = [
    {
      id: "sec-default",
      kind: "anthropic_token",
      label: "default",
      is_default: true,
      auto_eligible: false,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    {
      id: "sec-cheap",
      kind: "anthropic_token",
      label: "cheap-console",
      is_default: false,
      auto_eligible: false,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
  ];

  it("offers no judge picker when the user holds a single token", async () => {
    mockAuth(baseUser);
    mockApi.listSecrets.mockResolvedValue({ secrets: [twoTokens[0]] });
    render(
      <MemoryRouter>
        <RunDefaults />
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
        <RunDefaults />
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
        <RunDefaults />
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
        <RunDefaults />
      </MemoryRouter>,
    );
    const toggle = await screen.findByLabelText("Judge my finished runs");
    fireEvent.click(toggle);
    await waitFor(() => expect(mockApi.setJudgeEnabled).toHaveBeenCalledWith(true));
  });
});

// PRD #35 M3: the per-user DEFAULT for the usage-limit park. The per-RUN override
// lives on the run view (RunView.test.tsx); this is the setting every new run
// inherits, and the only opt-in reachable at all for the three kinds with no start
// affordance (autopilot, ci_fix, self_improve).
describe("Run defaults — usage-limit default (PRD #35 M3)", () => {
  const limitToggle = () =>
    screen.getByLabelText("Pause my new runs on a usage limit instead of failing them") as HTMLInputElement;

  it("explains the choice in terms of what the user loses without it", () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const text = container.textContent ?? "";
    // The stake, not the mechanism: today the run FAILS and its work is gone.
    expect(text).toMatch(/fails?/i);
    expect(text).toMatch(/pauses?/i);
    expect(text).toMatch(/resumes/i);
    // The runs that cannot opt in any other way — the reason this default exists.
    expect(text).toMatch(/autopilot/i);
    // The cost the PRD requires surfacing: a parked run holds its disk.
    expect(text).toMatch(/disk/i);
  });

  it("🔴 says in the UI that it does NOT change runs that already exist", () => {
    // The single most likely misreading, and it is expensive: a user whose run is
    // parked RIGHT NOW comes to Settings to stop it. This setting will not, and the
    // page has to say so where they are looking rather than in a doc.
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(container.textContent ?? "").toMatch(/does not change runs that already exist/i);
  });

  it("reflects the current default", () => {
    mockAuth({ ...baseUser, wait_on_limit: true });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(limitToggle().checked).toBe(true);
  });

  // Issue #520: the shipped column default is ON, so a field-absent response
  // (the boolean missing from the user object) must render the toggle CHECKED,
  // matching the DB default rather than assuming OFF.
  it("assumes the shipped default-on when wait_on_limit is absent", () => {
    mockAuth({ ...baseUser, wait_on_limit: undefined as unknown as boolean });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(limitToggle().checked).toBe(true);
  });

  it("enabling calls the API and refreshes the session", async () => {
    mockApi.setWaitOnLimit.mockResolvedValue({ user: { ...baseUser, wait_on_limit: true } });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(limitToggle());

    await waitFor(() => expect(mockApi.setWaitOnLimit).toHaveBeenCalledWith(true));
    expect(refresh).toHaveBeenCalled();
  });

  it("surfaces an error and does not leave the toggle stuck", async () => {
    mockApi.setWaitOnLimit.mockRejectedValue(new ApiError(500, "internal error"));
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(limitToggle());

    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(limitToggle().disabled).toBe(false);
  });

  it("🔴 does not touch the autopilot toggle, and autopilot does not touch it", async () => {
    // Independent writes to independent endpoints. They sit next to each other and
    // share a card boundary, so a shared busy/error pair is the easy mistake — it
    // would disable one control because the other failed, or blame the wrong one.
    mockApi.setWaitOnLimit.mockRejectedValue(new ApiError(500, "limit endpoint down"));
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(limitToggle());
    await screen.findByText("limit endpoint down");

    const autopilot = screen.getByLabelText("Enable autopilot for my account") as HTMLInputElement;
    expect(autopilot.disabled).toBe(false);
    expect(mockApi.setAutopilotEnabled).not.toHaveBeenCalled();
  });
});

// PRD #1020 M5: the per-user early-limit-reset Slack alert opt-in. DEFAULT ON, so a
// field-absent user renders CHECKED. It sits on the SAME usage-limits surface as the
// wait-on-limit default and writes its own dedicated endpoint (setNotifyEarlyReset).
describe("Run defaults — early-limit-reset alert (PRD #1020 M5)", () => {
  const earlyToggle = () =>
    screen.getByLabelText("Alert me when my 7-day limit resets early") as HTMLInputElement;

  it("reflects the current opt-in state (on)", () => {
    mockAuth({ ...baseUser, notify_early_limit_reset: true });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(earlyToggle().checked).toBe(true);
  });

  it("reflects the current opt-in state (off)", () => {
    mockAuth({ ...baseUser, notify_early_limit_reset: false });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(earlyToggle().checked).toBe(false);
  });

  // The shipped column default is ON, so a field-absent response must render the
  // toggle CHECKED — matching the DB default rather than assuming OFF.
  it("assumes the shipped default-on when notify_early_limit_reset is absent", () => {
    mockAuth({ ...baseUser, notify_early_limit_reset: undefined as unknown as boolean });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(earlyToggle().checked).toBe(true);
  });

  it("toggling calls the API with the flipped value and refreshes the session", async () => {
    // baseUser has notify_early_limit_reset:false, so the checkbox starts off and a
    // click sends true.
    mockApi.setNotifyEarlyReset.mockResolvedValue({
      user: { ...baseUser, notify_early_limit_reset: true },
    });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(earlyToggle());

    await waitFor(() => expect(mockApi.setNotifyEarlyReset).toHaveBeenCalledWith(true));
    expect(refresh).toHaveBeenCalled();
  });

  it("surfaces an error and does not leave the toggle stuck", async () => {
    mockApi.setNotifyEarlyReset.mockRejectedValue(new ApiError(500, "internal error"));
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(earlyToggle());

    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(earlyToggle().disabled).toBe(false);
  });
});

// Issue #916 M4: the per-user AI-attribution opt-out. DEFAULT ON (opt-out) — so a
// field-absent user renders CHECKED, and the copy must describe the commit trailer,
// never the MR description. (Post-PRD-#914-M3 ci_autofix is likewise default-on when
// its value is absent/null, so this is no longer the opposite of ci_autofix.)
describe("Run defaults — AI attribution opt-out (issue #916 M4)", () => {
  const attributionToggle = () =>
    screen.getByLabelText(
      "Include the Co-Authored-By: Claude trailer in my worker commits",
    ) as HTMLInputElement;

  it("describes the commit trailer and does not claim it changes MR descriptions", () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const text = container.textContent ?? "";
    expect(text).toMatch(/Co-Authored-By: Claude/);
    expect(text).toMatch(/on your next run/i);
    // The real effect is the commit trailer; MR descriptions are explicitly unaffected.
    expect(text).toMatch(/Merge-request descriptions are unaffected/i);
  });

  it("reflects the current opt-out state (on, the default)", () => {
    mockAuth({ ...baseUser, attribution_enabled: true });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(attributionToggle().checked).toBe(true);
  });

  it("reflects the current opt-out state (off)", () => {
    mockAuth({ ...baseUser, attribution_enabled: false });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(attributionToggle().checked).toBe(false);
  });

  // The shipped column default is ON, so a field-absent response must render the
  // toggle CHECKED — matching the DB default rather than assuming OFF.
  it("assumes the shipped default-on when attribution_enabled is absent", () => {
    mockAuth({ ...baseUser, attribution_enabled: undefined as unknown as boolean });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(attributionToggle().checked).toBe(true);
  });

  it("toggling off calls the API with false and refreshes the session", async () => {
    mockAuth({ ...baseUser, attribution_enabled: true });
    mockApi.setAttributionEnabled.mockResolvedValue({
      user: { ...baseUser, attribution_enabled: false },
    });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(attributionToggle());

    await waitFor(() =>
      expect(mockApi.setAttributionEnabled).toHaveBeenCalledWith(false),
    );
    expect(refresh).toHaveBeenCalled();
  });

  it("toggling on (from off) calls the API with true", async () => {
    mockAuth({ ...baseUser, attribution_enabled: false });
    mockApi.setAttributionEnabled.mockResolvedValue({
      user: { ...baseUser, attribution_enabled: true },
    });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(attributionToggle());

    await waitFor(() =>
      expect(mockApi.setAttributionEnabled).toHaveBeenCalledWith(true),
    );
    expect(refresh).toHaveBeenCalled();
  });

  it("surfaces an error and does not leave the toggle stuck", async () => {
    mockApi.setAttributionEnabled.mockRejectedValue(new ApiError(500, "internal error"));
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    fireEvent.click(attributionToggle());

    expect(await screen.findByText("internal error")).toBeTruthy();
    expect(attributionToggle().disabled).toBe(false);
  });
});

// PRD #914 M2/M3: the per-user CI-autofix opt-in is now TRI-STATE (boolean | null).
// The DISPLAY collapses inherit (null) and explicit true to CHECKED, and only an
// explicit false to UNCHECKED — checked={user?.ci_autofix_enabled !== false}. The null
// (inherit) branch was previously untested (every fixture pinned false), so a `?? false`
// or plain-bool display would render null as UNCHECKED and still pass the old suite; the
// null→CHECKED assertion below is what pins the tri-state semantics.
describe("Run defaults — CI autofix tri-state display (PRD #914 M3)", () => {
  const ciAutofixToggle = () =>
    screen.getByLabelText(
      "Automatically fix my failed CI pipelines",
    ) as HTMLInputElement;

  it("renders CHECKED when ci_autofix_enabled is null (inherit → on)", () => {
    mockAuth({ ...baseUser, ci_autofix_enabled: null });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    // Under the old `?? false` display this would be UNCHECKED; the tri-state
    // `!== false` renders inherit as on.
    expect(ciAutofixToggle().checked).toBe(true);
  });

  it("renders CHECKED when ci_autofix_enabled is true", () => {
    mockAuth({ ...baseUser, ci_autofix_enabled: true });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(ciAutofixToggle().checked).toBe(true);
  });

  it("renders UNCHECKED when ci_autofix_enabled is explicitly false", () => {
    mockAuth({ ...baseUser, ci_autofix_enabled: false });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(ciAutofixToggle().checked).toBe(false);
  });
});
