// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, ApiError, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import type { ModelSettingCell, UserSettings } from "../lib/apiTypes";

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
      setPlanCrossCheckEnabled: vi.fn(),
      setCodeCrossCheckEnabled: vi.fn(),
      setWaitOnLimit: vi.fn(),
      setNotifyEarlyReset: vi.fn(),
      setUsageReserve: vi.fn(),
      setWeeklyWarningMargin: vi.fn(),
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
  ephemeral_docker_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default",
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function mockAuth(user: User, over: Partial<ReturnType<typeof useAuth>> = {}) {
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
    refresh,
    updateUser: vi.fn(),
    serverUnreachable: false,
    retry: vi.fn(),
    ...over,
  });
}

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getMySettings.mockResolvedValue({ settings: { default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null } });
  mockApi.putMySettings.mockResolvedValue({ settings: { default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: "mission" } });
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

describe("Run defaults — code cross-check consent", () => {
  const code = () => screen.getByLabelText("Code cross-check · Advisory before publication") as HTMLInputElement;
  it("defaults absent code consent off independently of plan", () => {
    mockAuth({ ...baseUser, plan_cross_check_enabled: true });
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    expect(code().checked).toBe(false);
    expect((screen.getByLabelText("Plan cross-check · Required before implementation") as HTMLInputElement).checked).toBe(true);
    expect(screen.getByText("Adds the other model family's findings to the merge request; never blocks it")).toBeTruthy();
  });
  it("writes only code consent and displays local worker warning", async () => {
    mockApi.setCodeCrossCheckEnabled.mockResolvedValue({ user: baseUser, warning: "No online local worker supports code cross-check" });
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    fireEvent.click(code());
    expect(await screen.findByText("No online local worker supports code cross-check")).toBeTruthy();
    expect(mockApi.setCodeCrossCheckEnabled).toHaveBeenCalledWith(true);
    expect(mockApi.setPlanCrossCheckEnabled).not.toHaveBeenCalled();
    expect(refresh).toHaveBeenCalledOnce();
  });
  it("surfaces both-family 400 refusal and preserves consent", async () => {
    mockApi.setCodeCrossCheckEnabled.mockRejectedValue(new ApiError(400, "both Claude and Codex credentials must be usable"));
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    fireEvent.click(code());
    expect(await screen.findByText("both Claude and Codex credentials must be usable")).toBeTruthy();
    expect(code().checked).toBe(false);
    expect(code().disabled).toBe(false);
    expect(refresh).not.toHaveBeenCalled();
  });
});

describe("Run defaults — plan cross-check consent", () => {
  const crossCheckToggle = () =>
    screen.getByLabelText("Plan cross-check · Required before implementation") as HTMLInputElement;

  it("shows the stage and helper with the saved state", () => {
    mockAuth({ ...baseUser, plan_cross_check_enabled: true });
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    expect(screen.getByText("Cross-check")).toBeTruthy();
    expect(screen.getByText(/A second opinion from the other model family/)).toBeTruthy();
    expect(crossCheckToggle().checked).toBe(true);
  });

  it("sends plan consent and shows the API warning after refreshing", async () => {
    mockApi.setPlanCrossCheckEnabled.mockResolvedValue({
      user: { ...baseUser, plan_cross_check_enabled: true },
      warning: "No online worker can run Codex plan cross-checks",
    });
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    fireEvent.click(crossCheckToggle());
    await waitFor(() => expect(mockApi.setPlanCrossCheckEnabled).toHaveBeenCalledWith(true));
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("No online worker can run Codex plan cross-checks")).toBeTruthy();
  });

  it("keeps the saved value and allows retry after refusal", async () => {
    mockApi.setPlanCrossCheckEnabled.mockRejectedValue(
      new ApiError(409, "both Claude and Codex credentials must be usable"),
    );
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    fireEvent.click(crossCheckToggle());
    expect(await screen.findByText("both Claude and Codex credentials must be usable")).toBeTruthy();
    expect(crossCheckToggle().checked).toBe(false);
    expect(crossCheckToggle().disabled).toBe(false);
    expect(refresh).not.toHaveBeenCalled();
  });
});

describe("Run defaults — run judge opt-in (PRD #46, Decision 7)", () => {
  const judgeToggle = () =>
    screen.getByLabelText("Judge my finished runs") as HTMLInputElement;

  it("states plainly what the judge does and that it spends your credential for the harness it runs on", () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const text = container.textContent ?? "";
    expect(text).toMatch(/finished/i);
    // PRD #3245 / #3257: the judge spends your credential for the harness it runs on (chosen
    // under Judge runs on), not always Anthropic and not the reviewed run's credential.
    expect(text).toContain(
      "reviewed by an LLM using your own credential for the harness it runs on, shown below: your Anthropic token on Claude, your Codex credential on Codex.",
    );
    expect(text).not.toContain("credential of the run\u2019s harness");
    expect(text).toMatch(/recommend/i);
  });

  it("says where the judge's results appear, with no inbox (PRD #1650)", () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const text = container.textContent ?? "";
    // Positive half first: the results' real homes. The negative below is the
    // did-the-retired-copy-come-back guard and stays paired with it.
    expect(text).toContain(
      "Results appear on the Judge page and on the run\u2019s page, plus a Slack DM when your Slack account is linked.",
    );
    expect(text).toContain("It only recommends and never changes code.");
    expect(text).not.toMatch(/inbox/i);
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

describe("Run defaults — judge enforced banner (PRD #69 M4)", () => {
  it("shows no enforced banner when the admin has not enforced the judge", async () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Run judge");
    expect(container.textContent ?? "").not.toMatch(/ENFORCED/i);
  });

  it("renders the enforced banner naming both resolved judge models and the credential opt-out", async () => {
    mockAuth(baseUser, { judgeEnforcedByAdmin: true, effectiveJudgeModel: "sonnet" });
    mockApi.getMySettings.mockResolvedValue({ settings: {
      default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null, judge_model: null, summary_model: null,
      appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null,
      judge_models: [
        { harness: "claude", model: null, default_model: "sonnet", default_source: "instance_default", resolved_model: "sonnet", source: "instance_default" },
        { harness: "codex", model: "gpt-6-sol", default_model: "gpt-6.2-sol", default_source: "builtin", resolved_model: "gpt-6-sol", source: "user_default" },
      ],
    } });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const banner = await screen.findByText(/ENFORCED/);
    await waitFor(() => expect(banner.textContent).toContain("(Claude runs on sonnet, Codex runs on gpt-6-sol)"));
    expect(banner.textContent).toContain("your own credential for the harness the judge runs on");
    expect(banner.textContent).not.toContain("for the run's harness");
    expect(banner.textContent).toContain("has no usable credential, the judge is skipped");
    expect(banner.textContent).toContain("remove your credential for that harness");
  });

  it("falls back to the session's effective model in the banner on a server without judge_models", async () => {
    mockAuth(baseUser, { judgeEnforcedByAdmin: true, effectiveJudgeModel: "sonnet" });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText(/ENFORCED/).textContent).toContain("(Claude runs on sonnet)"));
  });
});

describe("Run defaults judge token picker (PRD #104)", () => {
  const twoTokens = [
    {
      id: "sec-default",
      kind: "anthropic_token",
      label: "default",
      is_default: true,
      enabled: true,
      disabled_at: null,
      auto_eligible: false,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    {
      id: "sec-cheap",
      kind: "anthropic_token",
      label: "cheap-console",
      is_default: false,
      enabled: true,
      disabled_at: null,
      auto_eligible: false,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
  ];

  // PRD #1140 M3 flipped the visibility guard from `> 1` to `>= 1`: with one token
  // the pool and the default coincide, but auto is now the default, so the MODE is
  // worth showing. The picker is hidden only at zero tokens.
  it("renders the judge picker (with the auto option) when the user holds a single token", async () => {
    mockAuth(baseUser);
    mockApi.listSecrets.mockResolvedValue({ secrets: [twoTokens[0]] });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Run judge");
    expect(screen.getByLabelText("Token the judge spends")).toBeTruthy();
    expect(
      screen.getByRole("option", { name: "Auto-select from the pool" }),
    ).toBeTruthy();
  });

  it("hides the judge picker when the user holds no tokens", async () => {
    mockAuth(baseUser);
    mockApi.listSecrets.mockResolvedValue({ secrets: [] });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByText("Run judge");
    expect(screen.queryByLabelText("Token the judge spends")).toBeNull();
  });

  it("points the judge lane at a named token, sending the label and mode pinned", async () => {
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
      // enabled rides along unchanged; the token is the label, not an id, and the
      // third arg is the derived mode.
      expect(mockApi.setJudgeEnabled).toHaveBeenCalledWith(false, "cheap-console", "pinned"),
    );
  });

  it("clears the judge binding back to the default by sending null and mode default", async () => {
    mockAuth({ ...baseUser, judge_anthropic_secret_id: "sec-cheap", judge_anthropic_secret_label: "cheap-console", judge_anthropic_bind_mode: "pinned" });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    mockApi.setJudgeEnabled.mockResolvedValue({ user: baseUser });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const picker = await screen.findByLabelText("Token the judge spends");
    fireEvent.change(picker, { target: { value: "" } });
    await waitFor(() => expect(mockApi.setJudgeEnabled).toHaveBeenCalledWith(false, null, "default"));
  });

  // The auto sentinel maps to mode "auto" with a null token — clearing (default) and
  // auto (the pool) are distinct, so the pool is never reached by an empty value.
  it("selecting Auto-select from the pool sends mode auto with a null token", async () => {
    mockAuth(baseUser);
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    mockApi.setJudgeEnabled.mockResolvedValue({ user: baseUser });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const picker = await screen.findByLabelText("Token the judge spends");
    // AUTO_OPTION is the NUL-prefixed sentinel WorkersSettings uses, not a readable value.
    fireEvent.change(picker, { target: { value: "\u0000auto" } });
    await waitFor(() =>
      expect(mockApi.setJudgeEnabled).toHaveBeenCalledWith(false, null, "auto"),
    );
  });

  it("renders the effective mode from judge_anthropic_bind_mode (auto → the auto option is selected)", async () => {
    mockAuth({ ...baseUser, judge_anthropic_bind_mode: "auto" });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const picker = (await screen.findByLabelText("Token the judge spends")) as HTMLSelectElement;
    expect(picker.value).toBe("\u0000auto");
  });

  // GET /api/auth/me carries the pinned judge token's ID but never its LABEL (only the
  // PUT /me/judge response fills it), so on a fresh page load the picker must resolve
  // the label from the loaded secrets by id. Rendering "Use my default token" here is
  // not cosmetic: the next picker change would replace the pin the user cannot see.
  it("selects the pinned token by resolving its label from secrets when /me carries only the id", async () => {
    mockAuth({
      ...baseUser,
      judge_anthropic_secret_id: "sec-cheap",
      judge_anthropic_secret_label: null,
      judge_anthropic_bind_mode: "pinned",
    });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const picker = (await screen.findByLabelText("Token the judge spends")) as HTMLSelectElement;
    await waitFor(() => expect(picker.value).toBe("cheap-console"));
  });

  // D4: an auto judge lane with an EMPTY pool spends the default token (unlike a
  // worker, which holds), so the warning names the default-token fallback.
  it("warns that an empty pool falls back to the default token in auto mode", async () => {
    mockAuth({ ...baseUser, judge_anthropic_bind_mode: "auto" });
    // Both tokens are un-pooled (auto_eligible: false), so pooledCount is 0.
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    await screen.findByLabelText("Token the judge spends");
    expect(container.textContent ?? "").toMatch(
      /your pool is empty, so retrospectives spend your default token/i,
    );
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

  it("explains automatic resume, recovered work and retained disk", () => {
    const { container } = render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    const text = screen.getByText("Usage limits").parentElement?.textContent ?? "";
    expect(text).toContain("Claude runs and Codex subscription runs");
    expect(text).toMatch(/pause/i);
    expect(text).toMatch(/resume/i);
    expect(text).toContain("Recovered work and an approved plan carry forward");
    expect(text).toMatch(/autopilot/i);
    expect(text).toMatch(/disk/i);
    expect(container.textContent).not.toContain("its work is lost");
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
    expect(screen.getByText(/weekly Anthropic or Codex limit reopens/)).toBeTruthy();
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


// PRD #2603: the per-user Model-written Now line switch, tri-state like mr_rework_enabled.
describe("Run defaults — Now line switch (PRD #2603)", () => {
  const baseSettings = { default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null };
  const nowToggle = () => screen.getByLabelText("Model-written Now line") as HTMLInputElement;
  const renderPage = () =>
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );

  it("reads null as ON and an explicit false as OFF", async () => {
    mockAuth({ ...baseUser });
    mockApi.getMySettings.mockResolvedValue({ settings: { ...baseSettings, now_summary_enabled: null } });
    renderPage();
    // The checkbox is disabled until the settings have loaded, so waiting for it to enable
    // proves the null reading is the loaded one, not the pre-load default.
    await waitFor(() => expect(nowToggle().disabled).toBe(false));
    expect(nowToggle().checked).toBe(true);
    cleanup();
    mockApi.getMySettings.mockResolvedValue({ settings: { ...baseSettings, now_summary_enabled: false } });
    renderPage();
    await waitFor(() => expect(nowToggle().checked).toBe(false));
  });

  it("saves the choice through putMySettings", async () => {
    mockAuth({ ...baseUser });
    mockApi.getMySettings.mockResolvedValue({ settings: { ...baseSettings, now_summary_enabled: null } });
    mockApi.putMySettings.mockResolvedValue({ settings: { ...baseSettings, now_summary_enabled: false } });
    renderPage();
    await waitFor(() => expect(nowToggle().disabled).toBe(false));
    fireEvent.click(nowToggle());
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ now_summary_enabled: false }));
    await waitFor(() => expect(nowToggle().checked).toBe(false));
  });
});

// Harness overflow stays here (PRD #3263 M3); the default harness it depends on is saved in
// Models, so the toggle reads the saved default and explains when it cannot apply.
describe("Run defaults — harness overflow availability", () => {
  const key = (kind: string) => ({
    id: kind, kind, label: kind, is_default: true, enabled: true, disabled_at: null, auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
  });
  const settings = (over: Partial<UserSettings> = {}) => ({
    settings: {
      default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null,
      judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null,
      ...over,
    } as UserSettings,
  });
  const overflowSwitch = () => screen.getByRole("switch", { name: /Overflow to the other harness/ });
  const NOTE = /Overflow applies only when a Claude or Codex default harness is saved in Models/;

  it("disables the overflow toggle with an explanation under Default · automatic and enables it under an explicit default", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token"), key("openai_api_key")] });
    mockApi.getMySettings.mockResolvedValue(settings());
    const view = render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    await waitFor(() => expect(screen.getByText(NOTE).textContent).toContain("With Default · automatic, uzi already picks by weekly limits."));
    expect(overflowSwitch().getAttribute("aria-disabled")).toBe("true");
    expect(overflowSwitch().getAttribute("aria-describedby")?.split(" ")).toContain(screen.getByText(NOTE).id);
    fireEvent.click(overflowSwitch());
    expect(mockApi.setHarnessOverflow).not.toHaveBeenCalled();
    view.unmount();
    mockApi.getMySettings.mockResolvedValue(settings({ default_harness: "codex" }));
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    await waitFor(() => expect(overflowSwitch().getAttribute("aria-disabled")).not.toBe("true"));
    expect(screen.queryByText(NOTE)).toBeNull();
  });

  it("with one usable harness and Default · automatic, the overflow note makes no weekly-limits claim", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token")] });
    mockApi.getMySettings.mockResolvedValue(settings());
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    // Positive control: the load landed (the Claude judge lane's credential reads the token).
    await screen.findByText("Your default token (anthropic_token)");
    expect(screen.getByText(NOTE).textContent).not.toContain("weekly limits");
  });
});

it("usage-limit provider regression #2360: shared default has neutral copy", () => {
  mockAuth(baseUser);
  render(<MemoryRouter><RunDefaults /></MemoryRouter>);
  expect(screen.getByText("Usage limits")).toBeTruthy();
  expect(screen.getByText(/Claude runs and Codex subscription runs can/).textContent).toContain("On by default.");
  // The saved opt-out stays off even though newly-created users default on.
  expect((screen.getByLabelText("Pause my new runs on a usage limit instead of failing them") as HTMLInputElement).checked).toBe(false);
});

// PRD #3263 M2: the checker model/effort grid moved to Models, saved with every other model
// choice there (ModelsSettings.test.tsx covers its Defaults, guards and custom IDs). Run
// defaults keeps only the plan/code cross-check enables and links to Models.
describe("cross-check model and effort live in Models", () => {
  it("shows the enables and a Models link, and no checker controls or separate save", async () => {
    render(<MemoryRouter><RunDefaults /></MemoryRouter>);
    await waitFor(() => expect(screen.getByText("Runs on")).toBeTruthy());
    expect(screen.getByText(/Plan cross-check · Required before implementation/)).toBeTruthy();
    expect(screen.getByText(/Code cross-check · Advisory before publication/)).toBeTruthy();
    const pointer = screen.getByText(/Model and effort for each cross-check stage are set in/);
    expect(pointer.textContent).toBe("Model and effort for each cross-check stage are set in Models.");
    expect(within(pointer).getByRole("link", { name: "Models" }).getAttribute("href")).toBe("/settings/models");
    expect(screen.queryByLabelText(/checker (model|effort)/)).toBeNull();
    expect(screen.queryByRole("table", { name: "Cross-check defaults" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save cross-check defaults" })).toBeNull();
  });
});

// PRD #3522 M1: the weekly usage reserve. One endpoint owns the pair (threshold, pause), so
// every control sends both; writes are optimistic and roll back on failure.
describe("Run defaults — weekly usage reserve (PRD #3522)", () => {
  const reserveSwitch = () => screen.getByRole("switch", { name: "Keep a weekly usage reserve" });
  const pctInput = () =>
    screen.getByLabelText("Stop starting runs when your 7-day usage reaches") as HTMLInputElement;
  const pauseBox = () =>
    screen.getByLabelText("Also pause running runs after their current milestone") as HTMLInputElement;
  const renderPage = () =>
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
  // The server echoes the stored pair in the user envelope.
  const echo = () =>
    mockApi.setUsageReserve.mockImplementation(async (pct, pause) => ({
      user: { ...baseUser, weekly_reserve_pct: pct, weekly_reserve_pause_running: pause },
    }));

  it("renders off by default, hiding the threshold and pause controls", () => {
    renderPage();
    expect(reserveSwitch().getAttribute("aria-checked")).toBe("false");
    expect(screen.queryByLabelText("Stop starting runs when your 7-day usage reaches")).toBeNull();
    expect(screen.queryByLabelText("Also pause running runs after their current milestone")).toBeNull();
    const help = document.getElementById(reserveSwitch().getAttribute("aria-describedby") ?? "");
    expect(help?.textContent).toMatch(/7-day/);
    expect(help?.textContent).toMatch(/Chat is not affected/);
  });

  it("shows a saved threshold with pause on, and the reserve explained", () => {
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    const { container } = renderPage();
    expect(reserveSwitch().getAttribute("aria-checked")).toBe("true");
    expect(pctInput().value).toBe("70");
    expect(pauseBox().checked).toBe(true);
    const text = container.textContent ?? "";
    expect(text).toContain("The last 30% of your 7-day window stays free");
    expect(text).toMatch(/Best effort/);
    expect(text).toMatch(/With no fresh reading, new work is held/);
  });

  it("enabling with 70 calls setUsageReserve(70, true) and refreshes the session", async () => {
    echo();
    renderPage();
    fireEvent.click(reserveSwitch());
    // Switching on saves at once with the offered default, then the typed value on blur.
    await waitFor(() => expect(mockApi.setUsageReserve).toHaveBeenCalledWith(90, true));
    fireEvent.change(pctInput(), { target: { value: "70" } });
    fireEvent.blur(pctInput());
    await waitFor(() => expect(mockApi.setUsageReserve).toHaveBeenLastCalledWith(70, true));
    expect(refresh).toHaveBeenCalled();
    expect(reserveSwitch().getAttribute("aria-checked")).toBe("true");
  });

  it("Enter commits the threshold, and an unchanged value sends nothing", async () => {
    echo();
    mockAuth({ ...baseUser, weekly_reserve_pct: 80, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.blur(pctInput());
    fireEvent.change(pctInput(), { target: { value: "65" } });
    fireEvent.keyDown(pctInput(), { key: "Enter" });
    await waitFor(() => expect(mockApi.setUsageReserve).toHaveBeenCalledWith(65, true));
    expect(mockApi.setUsageReserve).toHaveBeenCalledTimes(1);
  });

  it("toggling pause off sends (70, false)", async () => {
    echo();
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.click(pauseBox());
    await waitFor(() => expect(mockApi.setUsageReserve).toHaveBeenCalledWith(70, false));
    await waitFor(() => expect(pauseBox().checked).toBe(false));
  });

  it.each(["0", "101", "1.5", ""])("refuses %j with an inline error and makes no call", async (value) => {
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.change(pctInput(), { target: { value } });
    fireEvent.blur(pctInput());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Enter a whole number from 1 to 100.");
    expect(pctInput().getAttribute("aria-invalid")).toBe("true");
    expect(pctInput().getAttribute("aria-describedby")).toBe(alert.id);
    expect(mockApi.setUsageReserve).not.toHaveBeenCalled();
  });

  it("turning off sends (null, current pause value) and hides the controls", async () => {
    echo();
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: false });
    renderPage();
    fireEvent.click(reserveSwitch());
    await waitFor(() => expect(mockApi.setUsageReserve).toHaveBeenCalledWith(null, false));
    expect(reserveSwitch().getAttribute("aria-checked")).toBe("false");
    expect(screen.queryByLabelText("Also pause running runs after their current milestone")).toBeNull();
  });

  it("rolls back and names the failure when the write fails", async () => {
    mockApi.setUsageReserve.mockRejectedValue(new ApiError(500, "reserve endpoint down"));
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.change(pctInput(), { target: { value: "40" } });
    fireEvent.blur(pctInput());
    expect(await screen.findByText("reserve endpoint down")).toBeTruthy();
    expect(pctInput().value).toBe("70");
    expect(refresh).not.toHaveBeenCalled();

    // A failed switch-off comes back on.
    fireEvent.click(reserveSwitch());
    await waitFor(() => expect(mockApi.setUsageReserve).toHaveBeenLastCalledWith(null, true));
    await waitFor(() => expect(reserveSwitch().getAttribute("aria-checked")).toBe("true"));
    expect(reserveSwitch().getAttribute("aria-disabled")).toBeNull();
  });

  it("its failure does not disable or blame the usage-limit toggle beside it", async () => {
    mockApi.setUsageReserve.mockRejectedValue(new ApiError(500, "reserve endpoint down"));
    renderPage();
    fireEvent.click(reserveSwitch());
    expect(await screen.findByText("reserve endpoint down")).toBeTruthy();
    const limit = screen.getByLabelText(
      "Pause my new runs on a usage limit instead of failing them",
    ) as HTMLInputElement;
    expect(limit.disabled).toBe(false);
    expect(mockApi.setWaitOnLimit).not.toHaveBeenCalled();
  });

  it("gives the threshold input an accessible name that includes the unit", () => {
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    expect(screen.getByRole("spinbutton", { name: /percent/ })).toBe(pctInput());
  });

  it("Escape reverts an uncommitted edit and sends nothing", () => {
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.change(pctInput(), { target: { value: "55" } });
    expect(pctInput().value).toBe("55");
    fireEvent.keyDown(pctInput(), { key: "Escape" });
    expect(pctInput().value).toBe("70");
    expect(mockApi.setUsageReserve).not.toHaveBeenCalled();
  });

  // Deferred writes let a test act while a save is in flight.
  const deferred = () => {
    const calls: Array<{ pct: number | null; pause: boolean; done: () => void }> = [];
    mockApi.setUsageReserve.mockImplementation(
      (pct, pause) =>
        new Promise((resolve) => {
          calls.push({
            pct,
            pause,
            done: () =>
              resolve({ user: { ...baseUser, weekly_reserve_pct: pct, weekly_reserve_pause_running: pause } }),
          });
        }),
    );
    return calls;
  };

  it("a switch click right after a threshold blur-commit is not dropped", async () => {
    const calls = deferred();
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.change(pctInput(), { target: { value: "60" } });
    fireEvent.blur(pctInput());
    fireEvent.click(reserveSwitch());
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0]).toMatchObject({ pct: 60, pause: true });
    calls[0].done();
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[1]).toMatchObject({ pct: null, pause: true });
    calls[1].done();
    await waitFor(() => expect(reserveSwitch().getAttribute("aria-checked")).toBe("false"));
  });

  it("a pause click right after a threshold blur-commit keeps both changes", async () => {
    const calls = deferred();
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.change(pctInput(), { target: { value: "60" } });
    fireEvent.blur(pctInput());
    fireEvent.click(pauseBox());
    await waitFor(() => expect(calls).toHaveLength(1));
    calls[0].done();
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[1]).toMatchObject({ pct: 60, pause: false });
    calls[1].done();
    await waitFor(() => expect(mockApi.setUsageReserve).toHaveBeenCalledTimes(2));
  });

  it("overlapping saves are sent one at a time and end with the last intent", async () => {
    const calls = deferred();
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.change(pctInput(), { target: { value: "60" } });
    fireEvent.blur(pctInput());
    fireEvent.change(pctInput(), { target: { value: "50" } });
    fireEvent.blur(pctInput());
    fireEvent.change(pctInput(), { target: { value: "40" } });
    fireEvent.blur(pctInput());
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(mockApi.setUsageReserve).toHaveBeenCalledTimes(1);
    calls[0].done();
    await waitFor(() => expect(calls).toHaveLength(2));
    // The 50 intent was superseded by 40 before it was sent.
    expect(calls[1]).toMatchObject({ pct: 40, pause: true });
    calls[1].done();
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    expect(pctInput().value).toBe("40");
  });

  it("a change made while the session refresh is pending is still sent", async () => {
    const calls = deferred();
    let release: () => void = () => {};
    refresh.mockImplementationOnce(
      () =>
        new Promise<void>((resolve) => {
          release = resolve;
        }),
    );
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    renderPage();
    fireEvent.click(pauseBox());
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0]).toMatchObject({ pct: 70, pause: false });
    calls[0].done();
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
    // The write landed and the refresh is in flight: type a new threshold and blur.
    fireEvent.change(pctInput(), { target: { value: "40" } });
    fireEvent.blur(pctInput());
    release();
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[1]).toMatchObject({ pct: 40, pause: false });
    calls[1].done();
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(2));
  });

  it("follows a session user change made elsewhere, and a bare focus and blur sends nothing", () => {
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    const view = renderPage();
    expect(pctInput().value).toBe("70");
    mockAuth({ ...baseUser, weekly_reserve_pct: 50, weekly_reserve_pause_running: true });
    view.rerender(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
    expect(pctInput().value).toBe("50");
    fireEvent.focus(pctInput());
    fireEvent.blur(pctInput());
    expect(mockApi.setUsageReserve).not.toHaveBeenCalled();
  });

  it("drops its optimistic value once the session user moves on after a save", async () => {
    echo();
    mockAuth({ ...baseUser, weekly_reserve_pct: 70, weekly_reserve_pause_running: true });
    const view = renderPage();
    fireEvent.change(pctInput(), { target: { value: "60" } });
    fireEvent.blur(pctInput());
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    // The session catches up, then the CLI changes it again.
    for (const pct of [60, 50]) {
      mockAuth({ ...baseUser, weekly_reserve_pct: pct, weekly_reserve_pause_running: true });
      view.rerender(
        <MemoryRouter>
          <RunDefaults />
        </MemoryRouter>,
      );
    }
    expect(pctInput().value).toBe("50");
    expect(reserveSwitch().getAttribute("aria-checked")).toBe("true");
  });
});


describe("harness overflow autosave", () => {
  const label = "Overflow to the other harness when my default is near its limits";
  const renderPage = () => render(<MemoryRouter><RunDefaults /></MemoryRouter>);
  const control = () => screen.getByRole("switch", { name: label }) as HTMLButtonElement;
  const checked = () => control().getAttribute("aria-checked") === "true";

  it("saves on and explicitly off, hydrates on reload, and never writes other settings", async () => {
    let stored = false;
    const initial = (await mockApi.getMySettings()).settings;
    mockApi.getMySettings.mockImplementation(async () => ({ settings: { ...initial, default_harness: "claude" as const, harness_overflow_enabled: stored } }));
    mockApi.setHarnessOverflow.mockImplementation(async enabled => {
      stored = enabled;
      return { harness_overflow_enabled: enabled };
    });
    const page = renderPage();
    await waitFor(() => expect(checked()).toBe(false));
    expect(document.getElementById(control().getAttribute("aria-describedby")!)?.textContent).toContain("Saves automatically");
    control().focus();
    expect(document.activeElement).toBe(control());
    fireEvent.click(control());
    await waitFor(() => expect(checked()).toBe(true));
    expect(mockApi.setHarnessOverflow).toHaveBeenLastCalledWith(true);
    expect(mockApi.putMySettings).not.toHaveBeenCalled();
    page.unmount();
    renderPage();
    await waitFor(() => expect(checked()).toBe(true));
    fireEvent.click(control());
    await waitFor(() => expect(checked()).toBe(false));
    expect(mockApi.setHarnessOverflow).toHaveBeenLastCalledWith(false);
    cleanup();
    renderPage();
    await waitFor(() => expect(checked()).toBe(false));
  });

  it.each([false, true])("retains saved %s on error and allows retry", async saved => {
    const initial = (await mockApi.getMySettings()).settings;
    mockApi.getMySettings.mockResolvedValue({ settings: { ...initial, default_harness: "claude" as const, harness_overflow_enabled: saved } });
    mockApi.setHarnessOverflow.mockRejectedValueOnce(new ApiError(500, "overflow save failed"));
    renderPage();
    await waitFor(() => expect(checked()).toBe(saved));
    fireEvent.click(control());
    await screen.findByText("overflow save failed");
    expect(checked()).toBe(saved);
    expect(control().getAttribute("aria-disabled")).not.toBe("true");
    mockApi.setHarnessOverflow.mockResolvedValueOnce({ harness_overflow_enabled: !saved });
    fireEvent.click(control());
    await waitFor(() => expect(checked()).toBe(!saved));
    expect(screen.queryByText("overflow save failed")).toBeNull();
  });

  it.each([true, false])("merges only overflow while an independent settings save completes (overflow first: %s)", async overflowFirst => {
    let finishOverflow!: (value: { harness_overflow_enabled: boolean }) => void;
    let finishSettings!: (value: { settings: UserSettings }) => void;
    const initial = { ...(await mockApi.getMySettings()).settings, default_harness: "claude" as const };
    mockApi.getMySettings.mockResolvedValue({ settings: initial });
    mockApi.setHarnessOverflow.mockReturnValueOnce(new Promise(resolve => { finishOverflow = resolve; }));
    mockApi.putMySettings.mockReturnValueOnce(new Promise(resolve => { finishSettings = resolve; }));
    renderPage();
    await waitFor(() => expect(control().getAttribute("aria-disabled")).not.toBe("true"));
    control().focus();
    fireEvent.click(control());
    expect(control().getAttribute("aria-disabled")).toBe("true");
    expect(control().disabled).toBe(false);
    expect(document.activeElement).toBe(control());
    fireEvent.click(control());
    expect(mockApi.setHarnessOverflow).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByLabelText("Model-written Now line"));
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalled());
    const overflow = () => finishOverflow({ harness_overflow_enabled: true });
    const settings = () => finishSettings({ settings: { ...initial, now_summary_enabled: false } });
    (overflowFirst ? overflow : settings)();
    const nowLine = () => screen.getByLabelText("Model-written Now line") as HTMLInputElement;
    await waitFor(() => expect(overflowFirst ? checked() : nowLine().checked).toBe(overflowFirst ? true : false));
    (overflowFirst ? settings : overflow)();
    await waitFor(() => {
      expect(checked()).toBe(true);
      expect(nowLine().checked).toBe(false);
      expect(nowLine().disabled).toBe(false);
    });
  });
});

// PRD #3263 M3: Run defaults is behavior-only. Every model and effort editor moved to Models
// (ModelsSettings.test.tsx and modelsDraft.test.ts cover their Defaults, warnings, Runtime
// default and Same as the run); this page keeps the behavior controls and shows the judge's
// routing, models and credentials read-only with a Change in Models link.
describe("Run defaults is behavior-only (PRD #3263 M3)", () => {
  const key = (kind: string, over: Record<string, unknown> = {}) => ({
    id: kind, kind, label: kind, is_default: true, enabled: true, disabled_at: null, auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", ...over,
  });
  const renderPage = () => render(<MemoryRouter><RunDefaults /></MemoryRouter>);

  it("shows no model, effort, harness, planning, judge, summary or task-review editor and no model save", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token"), key("openai_api_key")] });
    const { container } = renderPage();
    // Positive control: the page has loaded and the judge summary rendered both lanes.
    await screen.findByText("Codex judge model");
    for (const name of [
      /worker model/i, /^Claude model$/, /^Codex model$/, /reasoning effort/i, /^(Claude|Codex) effort$/,
      /planning (model|effort)/i, /^Default harness$/, /^Judge runs on$/, /judge model/i, /summary model/i,
      /task-review (model|effort)/i, /task review (model|effort)/i,
    ]) {
      expect(screen.queryByLabelText(name), String(name)).toBeNull();
    }
    expect(container.querySelectorAll("select")).toHaveLength(1); // only the judge token picker
    expect(screen.getByLabelText("Token the judge spends")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Save / })).toBeNull();
    for (const title of ["Harness and worker models", "Reasoning effort", "Planning"]) {
      expect(screen.queryByRole("heading", { name: title }), title).toBeNull();
    }
    expect(mockApi.putMySettings).not.toHaveBeenCalled();
  });

  it("keeps every behavior control", async () => {
    renderPage();
    await screen.findByText("Run judge");
    for (const label of [
      "Enable autopilot for my account",
      "Pause my new runs on a usage limit instead of failing them",
      "Alert me when my 7-day limit resets early",
      "Judge my finished runs",
      "Model-written Now line",
      "Automatically fix my failed CI pipelines",
      "Include the Co-Authored-By: Claude trailer in my worker commits",
      "Auto-rework MR review comments on my runs",
    ]) {
      expect(screen.getByLabelText(label), label).toBeTruthy();
    }
    expect(screen.getByText(/Plan cross-check · Required before implementation/)).toBeTruthy();
    expect(screen.getByText(/Code cross-check · Advisory before publication/)).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Keep a weekly usage reserve" })).toBeTruthy();
    // specs/human.md pins the overflow label verbatim.
    expect(screen.getByRole("switch", { name: "Overflow to the other harness when my default is near its limits" })).toBeTruthy();
  });

  it("points the emptied summary and task-review cards at Models", async () => {
    renderPage();
    const summaries = await screen.findByText(/The summary model for each harness is set in/);
    expect(summaries.textContent).toBe("The summary model for each harness is set in Models.");
    expect(within(summaries).getByRole("link", { name: "Models" }).getAttribute("href")).toBe("/settings/models");
    const taskReview = screen.getByText(/pass uses are set in/);
    expect(taskReview.textContent).toBe("The model and effort the optional uzi handoff --review pass uses are set in Models.");
    expect(within(taskReview).getByRole("link", { name: "Models" }).getAttribute("href")).toBe("/settings/models");
    const overflow = screen.getByText(/Your default harness and worker models are set in/);
    expect(within(overflow).getByRole("link", { name: "Models" }).getAttribute("href")).toBe("/settings/models");
  });
});

// The judge's read-only summary: routing from the saved judge_runs_on, each lane's model from the
// server-resolved judge_models cells, and each lane's credential from the judge's own binding
// (Claude) or the owned Codex credentials, never from a lead credential prediction.
describe("Run defaults — judge summary (PRD #3263 M3)", () => {
  const key = (kind: string, over: Record<string, unknown> = {}) => ({
    id: kind, kind, label: kind, is_default: true, enabled: true, disabled_at: null, auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", ...over,
  });
  const cells = (): ModelSettingCell[] => [
    { harness: "claude", model: null, default_model: "sonnet", default_source: "instance_default", resolved_model: "sonnet", source: "instance_default" },
    { harness: "codex", model: "gpt-6-sol", default_model: "gpt-6.2-sol", default_source: "builtin", resolved_model: "gpt-6-sol", source: "user_default" },
  ];
  const judgeSettings = (judgeRunsOn: UserSettings["judge_runs_on"], over: Partial<UserSettings> = {}): { settings: UserSettings } => ({
    settings: {
      default_harness: null, judge_runs_on: judgeRunsOn, harness_overflow_enabled: false, default_model: null,
      default_effort: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null,
      typeface: null, theme: null, judge_model: null, codex_judge_model: "gpt-6-sol", judge_models: cells(), ...over,
    },
  });
  const renderPage = () => render(<MemoryRouter><RunDefaults /></MemoryRouter>);
  const summary = () => screen.getByRole("region", { name: "Where your judge runs" });
  // The summary as label → value pairs, in order.
  const pairs = () =>
    Array.from(summary().querySelectorAll("dt")).map((dt) => [dt.textContent, dt.nextElementSibling?.textContent]);
  const dimmed = (el: HTMLElement) => el.closest(".opacity-45") !== null;

  it("names the routing, each lane's model in effect and its credential, with a Change in Models link", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token", { label: "personal" }), key("openai_api_key")] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings(null));
    renderPage();
    await waitFor(() => expect(pairs()).toHaveLength(5));
    expect(pairs()).toEqual([
      ["Runs on", "Same as the run" + "Claude runs use the Claude judge model, Codex runs the Codex model. To judge every run on one harness, change it in Models."],
      ["Claude judge model", "sonnet · instance default"],
      ["Claude judge credential", "Your default token (personal)"],
      ["Codex judge model", "gpt-6-sol · your choice"],
      ["Codex judge credential", "Your usable Codex credential when the judge is queued"],
    ]);
    const link = within(summary()).getByRole("link", { name: "Change in Models" });
    expect(link.getAttribute("href")).toBe("/settings/models");
    expect(summary().textContent ?? "").not.toContain("Not used:");
  });

  it("shows the Claude provider restriction ahead of Codex credential readiness", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("openai_api_key")] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings("codex", { claude_provider_id: "gateway" }));
    renderPage();
    await waitFor(() => expect(pairs()).toContainEqual(["Codex judge credential", "Judge runs are skipped while a custom Claude provider is selected"]));
    expect(pairs()).toContainEqual(["Codex judge model", "gpt-6-sol · your choice"]);
  });

  it("does not claim a linked Codex alias makes a judge usable", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("codex_auth", { codex_status: "linked", auto_eligible: true })] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings("codex", { codex_account_mode: "auto" }));
    renderPage();
    await waitFor(() => expect(pairs()).toContainEqual(["Codex judge credential", "Codex credential availability is checked when the judge is queued"]));
  });

  it("drops a previous viewer's credential while loading the next viewer", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token", { label: "previous viewer token" })] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings(null));
    const page = () => <MemoryRouter><RunDefaults /></MemoryRouter>;
    const { rerender } = render(page());
    await screen.findByText("Your default token (previous viewer token)");
    let release!: (value: Awaited<ReturnType<typeof api.listSecrets>>) => void;
    mockApi.listSecrets.mockReturnValue(new Promise(resolve => { release = resolve; }));
    mockAuth({ ...baseUser, id: "u2" });
    rerender(page());
    expect(screen.queryByText(/previous viewer token/)).toBeNull();
    release({ secrets: [key("anthropic_token", { label: "next viewer token" })] });
    await screen.findByText("Your default token (next viewer token)");
    expect(screen.queryByText(/previous viewer token/)).toBeNull();
  });

  it("marks the lane the saved routing leaves unused in full contrast, and keeps the token picker operable", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token"), key("openai_api_key")] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings("codex"));
    renderPage();
    await waitFor(() => expect(pairs()[0][1]).toContain("Codex"));
    expect(pairs()[0][1]).toBe(
      "Codex" + "Every run is judged on Codex, so Claude runs get a Codex judge. If Codex can't run, the judge is skipped, never charged to Claude.",
    );
    expect(pairs()[1]).toEqual(["Claude judge model", "sonnet · instance default" + "Not used: the judge runs on Codex"]);
    expect(pairs()[3]).toEqual(["Codex judge model", "gpt-6-sol · your choice"]);
    // The summary never fades: a not-used lane says so in words.
    expect(summary().querySelector("[class*='opacity']")).toBeNull();
    const token = screen.getByLabelText("Token the judge spends") as HTMLSelectElement;
    expect(dimmed(token)).toBe(true);
    expect(token.disabled).toBe(false);
    expect(document.getElementById(token.getAttribute("aria-describedby") ?? "")?.textContent).toBe("Not used: the judge runs on Codex");
  });

  it.each([
    ["auto", 0, "Auto-select from the pool (empty, so your default token)"],
    ["auto", 1, "Auto-select from the pool"],
    ["pinned", 1, "Pinned to cheap-console"],
    ["default", 0, "Your default token (default)"],
  ] as const)("reads the Claude credential from the judge's own binding (%s, %d pooled)", async (mode, pooled, text) => {
    mockAuth({ ...baseUser, judge_anthropic_bind_mode: mode, judge_anthropic_secret_id: mode === "pinned" ? "sec-cheap" : null });
    mockApi.listSecrets.mockResolvedValue({ secrets: [
      key("anthropic_token", { id: "sec-default", label: "default" }),
      key("anthropic_token", { id: "sec-cheap", label: "cheap-console", is_default: false, auto_eligible: pooled > 0 }),
    ] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings(null));
    renderPage();
    await waitFor(() => expect(pairs()).toHaveLength(3));
    expect(pairs()[2]).toEqual(["Claude judge credential", text]);
  });

  it("says a lane is skipped when it has no usable credential, and never rewrites the explicit choice", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token")] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings("codex"));
    renderPage();
    await waitFor(() => expect(pairs()).toHaveLength(5));
    expect(pairs()[4]).toEqual(["Codex judge credential", "No usable Codex credential, so Codex judges are skipped"]);
    expect(document.getElementById("judge-runs-on-unusable")?.textContent).toBe(
      "Codex has no usable credential, so the judge is skipped on every run until you add one in Account & tokens. Your choice stays saved.",
    );
    expect(mockApi.putMySettings).not.toHaveBeenCalled();
  });

  it("shows only the usable lane for a single-harness user, and Unavailable without judge cells", async () => {
    mockAuth(baseUser, { effectiveJudgeModel: "" });
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("openai_api_key")] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings(null, { judge_models: undefined }));
    renderPage();
    await waitFor(() => expect(pairs()).toHaveLength(3));
    expect(pairs().slice(1)).toEqual([
      ["Codex judge model", "Unavailable"],
      ["Codex judge credential", "Your usable Codex credential when the judge is queued"],
    ]);
  });

  // Before the credential facts and saved settings load (and when that load fails) the
  // pre-load defaults would read as "no usable token" and "no saved default harness": the
  // summary and overflow make neither claim.
  const FALSE_CLAIMS = [
    /No usable Anthropic token/,
    /No usable Codex credential/,
    /has no usable credential/,
    /Overflow applies only when a Claude or Codex default harness is saved in Models/,
  ];
  const overflowSwitch = () => screen.getByRole("switch", { name: /Overflow to the other harness/ });
  const expectNoFalseClaims = () => {
    for (const claim of FALSE_CLAIMS) expect(screen.queryByText(claim), String(claim)).toBeNull();
    expect(summary().querySelector("dl")).toBeNull();
    expect(overflowSwitch().getAttribute("aria-disabled")).toBe("true");
  };

  it("shows a loading skeleton and no credential or overflow claim while settings load", async () => {
    mockApi.listSecrets.mockReturnValue(new Promise(() => {}));
    mockApi.getMySettings.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(summary().querySelector("[aria-busy='true']")).not.toBeNull();
    expect(summary().querySelectorAll(".animate-pulse").length).toBeGreaterThan(0);
    expectNoFalseClaims();
    expect(summary().textContent).not.toContain("Unavailable");
    // The disabled toggle names why: its describedby includes the loading note, not the
    // Unavailable one (that is only for a failed load).
    const loading = document.getElementById("harness-overflow-loading");
    expect(loading?.textContent).toBe("Loading your saved default harness");
    expect(loading?.className).toContain("sr-only");
    const describedBy = overflowSwitch().getAttribute("aria-describedby")?.split(" ");
    expect(describedBy).toEqual(["harness-overflow-scope", "harness-overflow-loading"]);
    fireEvent.click(overflowSwitch());
    expect(mockApi.setHarnessOverflow).not.toHaveBeenCalled();
  });

  it("says Unavailable, and makes no credential or overflow claim, when settings fail to load", async () => {
    mockApi.listSecrets.mockRejectedValue(new ApiError(500, "secrets unavailable"));
    mockApi.getMySettings.mockResolvedValue(judgeSettings("codex"));
    renderPage();
    await screen.findByText("secrets unavailable");
    expect(summary().textContent).toContain("Unavailable until your settings load");
    expect(summary().querySelectorAll(".animate-pulse")).toHaveLength(0);
    expectNoFalseClaims();
    const note = document.getElementById("harness-overflow-unavailable");
    expect(note?.textContent).toContain("Unavailable until your settings load");
    expect(overflowSwitch().getAttribute("aria-describedby")?.split(" ")).toContain("harness-overflow-unavailable");
    expect(document.getElementById("harness-overflow-loading")).toBeNull();
    fireEvent.click(overflowSwitch());
    expect(mockApi.setHarnessOverflow).not.toHaveBeenCalled();
  });

  it("wraps each dt/dd pair in its own group", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token"), key("openai_api_key")] });
    mockApi.getMySettings.mockResolvedValue(judgeSettings(null));
    renderPage();
    await waitFor(() => expect(pairs()).toHaveLength(5));
    const groups = Array.from(summary().querySelector("dl")!.children);
    expect(groups).toHaveLength(5);
    for (const g of groups) {
      expect(Array.from(g.children).map((c) => c.tagName)).toEqual(["DT", "DD"]);
    }
  });

  it("closes the card with the skip footnote", async () => {
    mockApi.getMySettings.mockResolvedValue(judgeSettings(null));
    renderPage();
    await screen.findByText("Run judge");
    expect(screen.getByText(/If the judge’s harness can’t run/).textContent).toBe(
      "If the judge’s harness can’t run, the judge is skipped, never charged to the other provider.",
    );
  });
});

// PRD #4123 module 6: the weekly-limit warning margin sits under the reserve, fed by the
// settings read; WeeklyWarningMarginSetting.test.tsx covers its validation and save states.
describe("Run defaults — weekly limit warning margin (PRD #4123)", () => {
  const renderPage = () =>
    render(
      <MemoryRouter>
        <RunDefaults />
      </MemoryRouter>,
    );
  const settingsWith = (over: Partial<UserSettings>) =>
    mockApi.getMySettings.mockResolvedValue({
      settings: { default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null, ...over } as UserSettings,
    });

  it("shows the stored margin from GET /me/settings and saves a new one", async () => {
    settingsWith({ weekly_warning_margin_pts: 15 });
    mockApi.setWeeklyWarningMargin.mockImplementation(async (n) => ({ weekly_warning_margin_pts: n }));
    renderPage();
    const input = (await screen.findByLabelText("Warn this many points before the limit")) as HTMLInputElement;
    expect(input.value).toBe("15");
    expect(screen.getByText("Warnings start at 85% weekly usage, 15 points before the limit.")).toBeTruthy();
    // Edit the way a user does: focus first. The setting mounts once the settings read
    // resolves outside act, so its [saved] effect can still be pending here. Without focus
    // (#4717), the change flushes that effect, which re-adopts 15 because nothing marks the
    // field as being edited, and blur then sees no change to save.
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: "0" } });
    fireEvent.blur(input);
    await waitFor(() => expect(mockApi.setWeeklyWarningMargin).toHaveBeenCalledWith(0));
    expect(await screen.findByText(/Early warnings are off/)).toBeTruthy();
  });

  it("stays hidden on a server whose settings carry no margin", async () => {
    renderPage();
    await waitFor(() => expect(mockApi.getMySettings).toHaveBeenCalled());
    await screen.findByText("Weekly usage reserve");
    expect(screen.queryByLabelText("Warn this many points before the limit")).toBeNull();
  });
});
