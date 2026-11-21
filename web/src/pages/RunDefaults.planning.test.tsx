// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, ApiError, type User } from "../lib/api";
import type { EffortSettingCell, UserSettings } from "../lib/apiTypes";

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
const codexKey = {
  id: "k1", kind: "openai_api_key", label: "codex", is_default: true, enabled: true, disabled_at: null, auto_eligible: false,
  created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
};
const cell = (harness: "claude" | "codex", extra: Partial<EffortSettingCell> = {}): EffortSettingCell => ({
  harness, effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium", source: "builtin", mode: "explicit", ...extra,
});

let settings: UserSettings;
beforeEach(() => {
  settings = {
    default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null,
    judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null,
    planning_codex_model: null, planning_codex_effort: null,
    planning_models: [
      { harness: "codex", model: null, default_model: "gpt-6.1-sol", default_source: "builtin", resolved_model: "gpt-6.1-sol", source: "builtin" },
    ],
    planning_efforts: [cell("codex", { default_effort: "runtime", resolved_effort: null, source: "user_default", mode: "runtime_default" })],
  };
  mockApi.listSecrets.mockResolvedValue({ secrets: [codexKey] } as never);
  mockApi.getMySettings.mockImplementation(async () => ({ settings }));
  mockApi.putMySettings.mockImplementation(async patch => {
    settings = { ...settings, ...patch } as UserSettings;
    return { settings };
  });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });
const mount = () => render(<MemoryRouter><RunDefaults /></MemoryRouter>);
const save = () => screen.getByText("Save planning defaults") as HTMLButtonElement;

it("shows the Codex-only planning lane with server defaults and no runtime effort option", async () => {
  mount();
  const model = await screen.findByLabelText("Codex planning model") as HTMLSelectElement;
  expect(screen.queryByLabelText("Claude planning model")).toBeNull();
  expect(model.options[0].text).toBe("Same as the run · gpt-6.1-sol (built-in)");
  const effort = screen.getByLabelText("Codex planning effort") as HTMLSelectElement;
  expect(effort.options[0].text).toContain("runtime default");
  expect(Array.from(effort.options).map(o => o.value)).toEqual(["", "low", "medium", "high", "xhigh", "max"]);
});

it("hides Codex planning without a usable credential and keeps the zero-credential Claude lane", async () => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mount();
  await screen.findByLabelText("Claude planning model");
  expect(screen.queryByLabelText("Codex planning model")).toBeNull();
});

it("saves a custom Codex pair atomically, then resets both with null", async () => {
  mount();
  const model = await screen.findByLabelText("Codex planning model") as HTMLSelectElement;
  fireEvent.change(model, { target: { value: "custom" } });
  expect(save().disabled).toBe(true);
  fireEvent.change(await screen.findByLabelText("Custom Codex planning model ID"), { target: { value: "custom-plan" } });
  const effort = screen.getByLabelText("Codex planning effort");
  fireEvent.change(effort, { target: { value: "max" } });
  fireEvent.click(save());
  await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ planning_codex_model: "custom-plan", planning_codex_effort: "max" }));
  await waitFor(() => expect(save().disabled).toBe(true));
  fireEvent.change(model, { target: { value: "inherit" } });
  fireEvent.change(effort, { target: { value: "" } });
  fireEvent.click(save());
  await waitFor(() => expect(mockApi.putMySettings).toHaveBeenLastCalledWith({ planning_codex_model: null, planning_codex_effort: null }));
});

it("blocks invalid custom models and associates a server model error with the Codex input", async () => {
  mount();
  fireEvent.change(await screen.findByLabelText("Codex planning model"), { target: { value: "custom" } });
  const input = await screen.findByLabelText("Custom Codex planning model ID");
  for (const value of ["two words", "opus"]) {
    fireEvent.change(input, { target: { value } });
    expect(save().disabled).toBe(true);
  }
  expect(mockApi.putMySettings).not.toHaveBeenCalled();
  fireEvent.change(input, { target: { value: "custom-plan" } });
  mockApi.putMySettings.mockRejectedValueOnce(new ApiError(400, "planning_codex_model: refused"));
  fireEvent.click(save());
  const error = await screen.findByText("planning_codex_model: refused");
  expect(error.id).toBe("planning-codex-model-error");
  expect(input.getAttribute("aria-describedby")).toContain(error.id);
  expect(document.getElementById("planning-model-error")).toBeNull();
});

for (const effort of ["max", "runtime"] as const) it(`refreshes the inherited Codex planning label after saving worker effort ${effort}`, async () => {
  settings = { ...settings, planning_efforts: [cell("codex")] };
  mockApi.putMySettings.mockImplementationOnce(async patch => {
    settings = { ...settings, ...patch, planning_efforts: [cell("codex", {
      default_effort: effort, default_source: "user_default",
      resolved_effort: effort === "runtime" ? null : effort, source: "user_default",
      mode: effort === "runtime" ? "runtime_default" : "explicit",
    })] } as UserSettings;
    return { settings };
  });
  mount();
  const planning = await screen.findByLabelText("Codex planning effort") as HTMLSelectElement;
  expect(planning.options[0].text).toContain("medium");
  fireEvent.change(screen.getByLabelText("Codex effort"), { target: { value: effort } });
  fireEvent.click(screen.getByText("Save Codex effort"));
  await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledExactlyOnceWith({ default_codex_effort: effort }));
  await waitFor(() => expect(planning.options[0].text).toContain(effort === "runtime" ? "runtime default" : effort));
  expect(planning.options[0].text).not.toContain("medium");
});

it("saves edits to both harnesses in one request", async () => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [codexKey, { ...codexKey, id: "claude", kind: "anthropic_token" }] } as never);
  mount();
  fireEvent.change(await screen.findByLabelText("Claude planning model"), { target: { value: "opus" } });
  fireEvent.change(screen.getByLabelText("Claude planning effort"), { target: { value: "high" } });
  fireEvent.change(screen.getByLabelText("Codex planning model"), { target: { value: "gpt-6-sol" } });
  fireEvent.change(screen.getByLabelText("Codex planning effort"), { target: { value: "max" } });
  fireEvent.click(save());
  await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledExactlyOnceWith({
    planning_claude_model: "opus", planning_claude_effort: "high", planning_codex_model: "gpt-6-sol", planning_codex_effort: "max",
  }));
});
