// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, type User } from "../lib/api";
import type { UserSettings, UserSettingsPatch } from "../lib/apiTypes";
import { mockTaskReviewModelCells } from "../mocks/mockApi/taskReviewModels";

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

function settings(claude: string | null, codex: string | null): UserSettings {
  return {
    default_harness: null, judge_runs_on: null, harness_overflow_enabled: false, default_model: null, default_effort: null, judge_model: null,
    summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null,
    task_review_model: claude, codex_task_review_model: codex,
    task_review_models: mockTaskReviewModelCells(claude, codex),
  };
}

let stored: { claude: string | null; codex: string | null };
beforeEach(() => {
  stored = { claude: null, codex: null };
  mockApi.listSecrets.mockResolvedValue({ secrets: [key("anthropic_token"), key("openai_api_key")] } as never);
  mockApi.getMySettings.mockImplementation(async () => ({ settings: settings(stored.claude, stored.codex) }));
  mockApi.putMySettings.mockImplementation(async (patch: UserSettingsPatch) => {
    if (patch.task_review_model !== undefined) stored.claude = patch.task_review_model ?? null;
    if (patch.codex_task_review_model !== undefined) stored.codex = patch.codex_task_review_model ?? null;
    return { settings: settings(stored.claude, stored.codex) };
  });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

const renderPage = () => render(<MemoryRouter><RunDefaults /></MemoryRouter>);

describe("Run defaults: task review model (PRD #1570 M1)", () => {
  it("renders the Task review section with the built-in defaults", async () => {
    renderPage();
    await screen.findByText("Task review");
    const claude = (await screen.findByLabelText("Claude task-review model")) as HTMLInputElement;
    expect(claude.value).toBe("");
    expect(claude.placeholder).toBe("SDK default (built-in)");
    const codex = (await screen.findByLabelText("Codex task-review model")) as HTMLSelectElement;
    expect(Array.from(codex.options)[0].textContent).toBe("Default · gpt-6-sol (built-in)");
    await waitFor(() => expect(document.getElementById("codex-task-review-model-effect")?.textContent)
      .toBe("In effect: gpt-6-sol · built-in"));
    expect(document.getElementById("task-review-model-effect")?.textContent).toBe("In effect: SDK default · built-in");
  });

  it("saves the Claude field and then resets it with null", async () => {
    renderPage();
    const claude = (await screen.findByLabelText("Claude task-review model")) as HTMLInputElement;
    fireEvent.change(claude, { target: { value: " opus " } });
    fireEvent.click(screen.getByRole("button", { name: "Save Claude task-review model" }));
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ task_review_model: "opus" }));
    await waitFor(() => expect(document.getElementById("task-review-model-effect")?.textContent)
      .toBe("In effect: opus · your choice"));
    fireEvent.change(claude, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Claude task-review model" }));
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenLastCalledWith({ task_review_model: null }));
    await waitFor(() => expect(document.getElementById("task-review-model-effect")?.textContent)
      .toBe("In effect: SDK default · built-in"));
  });

  it("saves the Codex field and resets it with null", async () => {
    renderPage();
    const codex = (await screen.findByLabelText("Codex task-review model")) as HTMLSelectElement;
    fireEvent.change(codex, { target: { value: "gpt-6-astra" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Codex task-review model" }));
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ codex_task_review_model: "gpt-6-astra" }));
    await waitFor(() => expect(document.getElementById("codex-task-review-model-effect")?.textContent)
      .toBe("In effect: gpt-6-astra · your choice"));
    fireEvent.change(codex, { target: { value: "inherit" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Codex task-review model" }));
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenLastCalledWith({ codex_task_review_model: null }));
  });

  it("warns on a Codex id in the Claude field and blocks Save", async () => {
    renderPage();
    const claude = (await screen.findByLabelText("Claude task-review model")) as HTMLInputElement;
    fireEvent.change(claude, { target: { value: "gpt-6-astra" } });
    expect(await screen.findByText(/Not a Claude model: gpt-6-astra is a Codex model/)).toBeTruthy();
    const save = screen.getByRole("button", { name: "Save Claude task-review model" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.click(save);
    expect(mockApi.putMySettings).not.toHaveBeenCalled();
    const describedBy = (claude.getAttribute("aria-describedby") ?? "").split(" ");
    expect(describedBy).toContain("task-review-model-help");
    expect(describedBy).toContain("task-review-model-warning");
    expect(document.getElementById("task-review-model-warning")?.textContent).toMatch(/Not a Claude model/);
    expect(document.getElementById("task-review-model-warning")?.textContent).toContain("the server refuses this save");
  });
});
