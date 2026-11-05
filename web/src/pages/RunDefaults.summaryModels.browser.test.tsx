import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, type User } from "../lib/api";
import type { UserSettings, UserSettingsPatch } from "../lib/apiTypes";
import { mockSummaryModelCells } from "../mocks/mockApi/summaryModels";

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

// A tiny stateful stand-in for /me/settings whose cells come from the mock summary
// resolver, so the page is driven through the same lane/cell contract the server serves.
function settings(codex: string | null): UserSettings {
  return {
    default_harness: null, harness_overflow_enabled: false, default_model: null, default_effort: null, judge_model: null,
    summary_model: null, codex_summary_model: codex, appearance_mode: null, light_theme: null, dark_theme: null,
    typeface: null, theme: null,
    summary_models: mockSummaryModelCells({ claude: null, codex }, { claude: "haiku", codex: "gpt-6.1-sol" }),
  };
}

beforeEach(() => {
  let stored: string | null = null;
  mockApi.listSecrets.mockResolvedValue({ secrets: [codexKey] } as never);
  mockApi.getMySettings.mockImplementation(async () => ({ settings: settings(stored) }));
  mockApi.putMySettings.mockImplementation(async (patch: UserSettingsPatch) => {
    stored = patch.codex_summary_model ?? null;
    return { settings: settings(stored) };
  });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

it("a Codex-only user picks a Codex summary model, saves it, and sees it in effect", async () => {
  render(<MemoryRouter><RunDefaults /></MemoryRouter>);
  const select = (await screen.findByLabelText("Codex summary model")) as HTMLSelectElement;
  expect(screen.queryByLabelText("Claude summary model")).toBeNull();
  await waitFor(() => expect(document.getElementById("codex-summary-model-effect")?.textContent)
    .toBe("In effect: gpt-6.1-sol · instance default"));
  await userEvent.selectOptions(select, "gpt-6-astra");
  await userEvent.click(screen.getByRole("button", { name: "Save Codex summary model" }));
  await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ codex_summary_model: "gpt-6-astra" }));
  await waitFor(() => expect(document.getElementById("codex-summary-model-effect")?.textContent)
    .toBe("In effect: gpt-6-astra · your choice"));
  expect(select.value).toBe("gpt-6-astra");
});
