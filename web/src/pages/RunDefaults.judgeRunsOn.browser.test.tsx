import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, type User } from "../lib/api";
import type { ModelSettingCell, UserSettings, UserSettingsPatch } from "../lib/apiTypes";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSecrets: vi.fn(),
      getMySettings: vi.fn(),
      putMySettings: vi.fn(),
      listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
      getMyRateLimits: vi.fn().mockResolvedValue({ tokens: [] }),
      getMySlack: vi.fn().mockResolvedValue({}),
      listAgentTemplates: vi.fn().mockResolvedValue({ templates: [] }),
      getTemplateAllocations: vi.fn().mockResolvedValue({ allocations: [] }),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({
  useAuth: () => ({
    user: { id: "u1", email: "a@uzi.local", is_admin: false, is_active: true, judge_anthropic_bind_mode: "default" } as unknown as User,
    uziLabel: "uzi", autopilotLabel: "autopilot", judgeEnforcedByAdmin: false, effectiveJudgeModel: "opus",
    refresh: vi.fn(), updateUser: vi.fn(),
  }),
}));

const mockApi = vi.mocked(api);
const anthropicToken = {
  id: "t1", kind: "anthropic_token", label: "personal", is_default: true, enabled: true, disabled_at: null, auto_eligible: false,
  created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
};
const cells: ModelSettingCell[] = [
  { harness: "claude", model: null, default_model: "opus", default_source: "instance_default", resolved_model: "opus", source: "instance_default" },
  { harness: "codex", model: null, default_model: "gpt-6-astra", default_source: "instance_default", resolved_model: "gpt-6-astra", source: "instance_default" },
];
function settings(judgeRunsOn: UserSettings["judge_runs_on"]): UserSettings {
  return {
    default_harness: null, judge_runs_on: judgeRunsOn, harness_overflow_enabled: false, default_model: null, default_effort: null,
    judge_model: null, codex_judge_model: null, judge_models: cells, summary_model: null, appearance_mode: null,
    light_theme: null, dark_theme: null, typeface: null, theme: null,
  };
}

beforeEach(async () => {
  await page.viewport(400, 900);
  // A Claude-only user: Codex has no usable credential, so before PRD #3257 the Codex judge
  // lane was hidden. Choosing Codex must keep that lane and dim the Claude side.
  mockApi.listSecrets.mockResolvedValue({ secrets: [anthropicToken] } as never);
  mockApi.getMySettings.mockResolvedValue({ settings: settings(null) });
  mockApi.putMySettings.mockImplementation(async (patch: UserSettingsPatch) => ({ settings: settings(patch.judge_runs_on ?? null) }));
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

const opacityChain = (el: Element): number => {
  let o = 1;
  for (let n: Element | null = el; n; n = n.parentElement) o *= Number(getComputedStyle(n).opacity);
  return o;
};

it("lays the judge card out in one column at 400px with Codex chosen and the Claude side dimmed", async () => {
  render(<MemoryRouter><RunDefaults /></MemoryRouter>);
  const runsOn = (await screen.findByLabelText("Judge runs on")) as HTMLSelectElement;
  await screen.findByLabelText("Token the judge spends");

  // Keyboard reaches the select, and choosing Codex saves at once.
  runsOn.focus();
  expect(document.activeElement).toBe(runsOn);
  await userEvent.selectOptions(runsOn, "codex");
  await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ judge_runs_on: "codex" }));
  await waitFor(() => expect(runsOn.getAttribute("aria-busy")).toBeNull());
  // The select stays enabled through the save, so keyboard focus never drops to <body>.
  expect(runsOn.disabled).toBe(false);
  expect(document.activeElement).toBe(runsOn);
  expect(runsOn.value).toBe("codex");

  const codexLane = screen.getByLabelText("Codex judge model");
  const claudeLane = screen.getByLabelText("Claude judge model");
  const token = screen.getByLabelText("Token the judge spends");
  const unusable = document.getElementById("judge-runs-on-unusable")!;
  expect(unusable.textContent).toContain("Codex has no usable credential");

  // Nothing spills past the viewport, and the two lanes stack (single column below sm).
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(400);
  for (const el of [runsOn, codexLane, claudeLane, token, unusable]) {
    expect(el.getBoundingClientRect().right).toBeLessThanOrEqual(400);
  }
  expect(codexLane.getBoundingClientRect().top).toBeGreaterThan(claudeLane.getBoundingClientRect().bottom);

  // The real rendered opacity: the not-used controls at .45, the chosen lane and every
  // explanatory line at full strength so the reason stays legible.
  expect(opacityChain(claudeLane)).toBeCloseTo(0.45, 2);
  expect(opacityChain(token)).toBeCloseTo(0.45, 2);
  expect(opacityChain(codexLane)).toBe(1);
  const tokenNote = document.getElementById("judge-token-not-used")!;
  const claudeNote = document.getElementById("judge-model-effect")!;
  expect(tokenNote.textContent).toBe("Not used: the judge runs on Codex");
  expect(claudeNote.textContent).toContain("Not used: the judge runs on Codex");
  for (const el of [tokenNote, claudeNote, unusable]) expect(opacityChain(el)).toBe(1);

  // The dimmed picker is still operable from the keyboard.
  (token as HTMLSelectElement).focus();
  expect(document.activeElement).toBe(token);
});

it("keeps keyboard focus on the select through a save, so a second ArrowDown still moves it", async () => {
  // Hold the first PUT open so the second key press lands mid-save.
  let release: () => void = () => {};
  mockApi.putMySettings.mockImplementationOnce((patch: UserSettingsPatch) => new Promise((resolve) => {
    release = () => resolve({ settings: settings(patch.judge_runs_on ?? null) });
  }));
  render(<MemoryRouter><RunDefaults /></MemoryRouter>);
  const runsOn = (await screen.findByLabelText("Judge runs on")) as HTMLSelectElement;
  await screen.findByLabelText("Token the judge spends");
  runsOn.focus();

  await userEvent.keyboard("{ArrowDown}");
  await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ judge_runs_on: "claude" }));
  expect(document.activeElement).toBe(runsOn);
  expect(runsOn.getAttribute("aria-busy")).toBe("true");

  await userEvent.keyboard("{ArrowDown}");
  expect(runsOn.value).toBe("codex");
  // Queued behind the in-flight save, not sent alongside it.
  expect(mockApi.putMySettings).toHaveBeenCalledTimes(1);
  release();
  await waitFor(() => expect(mockApi.putMySettings).toHaveBeenLastCalledWith({ judge_runs_on: "codex" }));
  await waitFor(() => expect(runsOn.getAttribute("aria-busy")).toBeNull());
  expect(mockApi.putMySettings).toHaveBeenCalledTimes(2);
  expect(runsOn.value).toBe("codex");
  expect(document.activeElement).toBe(runsOn);
});
