// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, type User } from "../lib/api";
import type { EffortSettingCell, UserSettings, UserSettingsPatch } from "../lib/apiTypes";
import { mockTaskReviewEffortCells, mockTaskReviewModelCells } from "../mocks/mockApi/taskReviewModels";

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
const key = (kind: string) => ({
  id: kind, kind, label: kind, is_default: true, enabled: true, disabled_at: null, auto_eligible: false,
  created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
});
const workerCell = (harness: "claude" | "codex", level: string | null): EffortSettingCell =>
  level === null
    ? { harness, effort: "runtime", default_effort: "medium", default_source: "builtin", resolved_effort: null, source: "user_default", mode: "runtime_default" }
    : { harness, effort: level, default_effort: "medium", default_source: "builtin", resolved_effort: level, source: "user_default", mode: "explicit" };

// The worker efforts are fixed: Claude high, Codex Runtime default.
const worker = { claude: workerCell("claude", "high"), codex: workerCell("codex", null) };

function settings(claude: string | null, codex: string | null): UserSettings {
  return {
    default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: "high",
    default_codex_effort: "runtime", judge_model: null, summary_model: null, appearance_mode: null, light_theme: null,
    dark_theme: null, typeface: null, theme: null,
    task_review_models: mockTaskReviewModelCells(null, null),
    task_review_effort: claude, codex_task_review_effort: codex,
    task_review_efforts: mockTaskReviewEffortCells({ claude, codex }, worker),
    effort_settings: [worker.claude, worker.codex],
  };
}

let stored: { claude: string | null; codex: string | null };
beforeEach(() => {
  stored = { claude: null, codex: null };
  mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token"), key("openai_api_key")] } as never);
  mockApi.getMySettings.mockImplementation(async () => ({ settings: settings(stored.claude, stored.codex) }));
  mockApi.putMySettings.mockImplementation(async (patch: UserSettingsPatch) => {
    if (patch.task_review_effort !== undefined) stored.claude = patch.task_review_effort ?? null;
    if (patch.codex_task_review_effort !== undefined) stored.codex = patch.codex_task_review_effort ?? null;
    return { settings: settings(stored.claude, stored.codex) };
  });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

const renderPage = () => render(<MemoryRouter><RunDefaults /></MemoryRouter>);

const lanes = [
  { name: "Claude", label: "Claude task-review effort", field: "task_review_effort", inherited: "high", other: "codex_task_review_effort" },
  { name: "Codex", label: "Codex task-review effort", field: "codex_task_review_effort", inherited: "Runtime default", other: "task_review_effort" },
] as const;

describe.each(lanes)("Run defaults: $name task-review effort (PRD #1570 M2)", ({ name, label, field, inherited }) => {
  it("labels the Default option with the inherited worker effort", async () => {
    renderPage();
    const select = (await screen.findByLabelText(label)) as HTMLSelectElement;
    expect(select.options[0].textContent).toBe(`Default · follows worker effort (${inherited})`);
    expect(select.options[1].textContent).toBe("Runtime default");
    expect(select.value).toBe("");
  });

  it("saves a level, Runtime default, and then Default as null", async () => {
    renderPage();
    const select = (await screen.findByLabelText(label)) as HTMLSelectElement;
    const save = screen.getByRole("button", { name: `Save ${name} task-review effort` });
    expect((save as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(select, { target: { value: "high" } });
    fireEvent.click(save);
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenLastCalledWith({ [field]: "high" }));
    await waitFor(() => expect(select.value).toBe("high"));

    fireEvent.change(select, { target: { value: "runtime" } });
    fireEvent.click(save);
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenLastCalledWith({ [field]: "runtime" }));

    fireEvent.change(select, { target: { value: "" } });
    fireEvent.click(save);
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenLastCalledWith({ [field]: null }));
    expect(mockApi.putMySettings).toHaveBeenCalledTimes(3);
  });
});

describe.each([
  { late: "Claude task-review effort", field: "task_review_effort", newer: "Codex", newerField: "default_codex_effort" },
  { late: "Codex task-review effort", field: "codex_task_review_effort", newer: "Claude", newerField: "default_effort" },
  { late: "Claude effort", field: "default_effort", newer: "Codex", newerField: "default_codex_effort" },
  { late: "Codex effort", field: "default_codex_effort", newer: "Claude", newerField: "default_effort" },
] as const)("overlapping saves: late $late response", ({ late, field, newer, newerField }) => {
  it("keeps the other lane's newly saved inherited effort when responses arrive in reverse order", async () => {
    const values = { default_effort: "high", default_codex_effort: "runtime", task_review_effort: null, codex_task_review_effort: null } as {
      default_effort: string; default_codex_effort: string; task_review_effort: string | null; codex_task_review_effort: string | null;
    };
    const snapshot = (): UserSettings => {
      const claudeWorker = workerCell("claude", values.default_effort === "runtime" ? null : values.default_effort);
      const codexWorker = workerCell("codex", values.default_codex_effort === "runtime" ? null : values.default_codex_effort);
      return {
        ...settings(values.task_review_effort, values.codex_task_review_effort), ...values,
        effort_settings: [claudeWorker, codexWorker],
        planning_efforts: [claudeWorker, codexWorker].map(cell => ({
          ...cell, effort: null, default_effort: cell.resolved_effort ?? "runtime", default_source: "user_default" as const,
        })),
        task_review_efforts: mockTaskReviewEffortCells(
          { claude: values.task_review_effort, codex: values.codex_task_review_effort },
          { claude: claudeWorker, codex: codexWorker },
        ),
      };
    };
    const pending: Array<{ response: UserSettings; resolve: (value: { settings: UserSettings }) => void }> = [];
    mockApi.getMySettings.mockResolvedValue({ settings: snapshot() });
    mockApi.putMySettings.mockImplementation((patch) => {
      Object.assign(values, patch);
      const response = snapshot();
      return new Promise(resolve => pending.push({ response, resolve }));
    });
    renderPage();
    const lateSelect = await screen.findByLabelText(late);
    const lateButton = screen.getByRole("button", { name: `Save ${late}` }) as HTMLButtonElement;
    fireEvent.change(lateSelect, { target: { value: "max" } });
    fireEvent.click(lateButton);
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenNthCalledWith(1, { [field]: "max" }));
    const newerSelect = screen.getByLabelText(`${newer} effort`);
    const newerButton = screen.getByRole("button", { name: `Save ${newer} effort` }) as HTMLButtonElement;
    fireEvent.change(newerSelect, { target: { value: "low" } });
    fireEvent.click(newerButton);
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenNthCalledWith(2, { [newerField]: "low" }));
    pending[1].resolve({ settings: pending[1].response });
    await waitFor(() => expect(newerButton.disabled).toBe(true));
    const inherited = screen.getByLabelText(`${newer} task-review effort`) as HTMLSelectElement;
    expect(inherited.options[0].textContent).toBe("Default · follows worker effort (low)");
    const planning = screen.getByLabelText(`${newer} planning effort`) as HTMLSelectElement;
    expect(planning.options[0].textContent).toContain("Same as the run · low");
    pending[0].resolve({ settings: pending[0].response });
    await waitFor(() => expect(lateButton.disabled).toBe(true));
    expect(inherited.options[0].textContent).toBe("Default · follows worker effort (low)");
    expect(planning.options[0].textContent).toContain("Same as the run · low");
  });
});
