import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, type User } from "../lib/api";
import type { EffortSettingCell } from "../lib/apiTypes";

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

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [codexKey] } as never);
  mockApi.getMySettings.mockResolvedValue({ settings: {
    default_harness: null, harness_overflow_enabled: false, default_model: null, default_effort: null, default_codex_effort: null,
    judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: null,
    effort_settings: [cell("claude"), cell("codex", { catalog_default: {
      state: "known", effort: "medium", model: "gpt-6.1-sol", runtime_version: "0.164.0", observed_at: "2026-11-05T09:30:00Z" } })],
  } });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

it("the Codex effort select offers the known catalog default as its Runtime default option", async () => {
  render(<MemoryRouter><RunDefaults /></MemoryRouter>);
  const select = (await screen.findByLabelText("Codex effort")) as HTMLSelectElement;
  const runtime = Array.from(select.options).find((o) => o.value === "runtime");
  expect(runtime?.text).toBe("Runtime default · catalog default: medium (gpt-6.1-sol, Codex 0.164.0)");
});
