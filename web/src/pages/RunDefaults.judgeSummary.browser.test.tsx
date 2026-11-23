import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunDefaults } from "./RunDefaults";
import { api, type User } from "../lib/api";
import type { ModelSettingCell, UserSettings } from "../lib/apiTypes";

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
  // A Claude-only user whose judge runs on Codex (saved in Models): the summary keeps the
  // Codex lane, says it is skipped, and marks the Claude side not used.
  mockApi.listSecrets.mockResolvedValue({ secrets: [anthropicToken] } as never);
  mockApi.getMySettings.mockResolvedValue({ settings: settings("codex") });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

const opacityChain = (el: Element): number => {
  let o = 1;
  for (let n: Element | null = el; n; n = n.parentElement) o *= Number(getComputedStyle(n).opacity);
  return o;
};

// The app renders every page inside AppShell's <main> gutter (px-4 sm:px-6), and the
// settings shell's pinned tab row bleeds into that gutter (PinnedBlock, #4419).
const renderInGutter = () =>
  render(<MemoryRouter><div className="px-4 sm:px-6"><RunDefaults /></div></MemoryRouter>);

it("lays the judge summary out in one column at 400px, readable, with a keyboard-reachable Change in Models link", async () => {
  renderInGutter();
  const summary = await screen.findByRole("region", { name: "Where your judge runs" });
  await waitFor(() => expect(summary.querySelectorAll("dt")).toHaveLength(5));
  const unusable = document.getElementById("judge-runs-on-unusable")!;
  expect(unusable.textContent).toContain("Codex has no usable credential");

  // Nothing spills past the viewport; below sm each label sits above its value.
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(400);
  for (const el of [summary, unusable, ...Array.from(summary.querySelectorAll("dd"))]) {
    expect(el.getBoundingClientRect().right).toBeLessThanOrEqual(400);
  }
  const [dt, dd] = [summary.querySelector("dt")!, summary.querySelector("dd")!];
  expect(dd.getBoundingClientRect().top).toBeGreaterThanOrEqual(dt.getBoundingClientRect().bottom);

  // A read-only summary never fades: every line renders at full opacity, and the not-used lane
  // says so in words.
  for (const el of Array.from(summary.querySelectorAll("dt, dd"))) expect(opacityChain(el)).toBe(1);
  expect(summary.textContent).toContain("Not used: the judge runs on Codex");

  // The link is a real focus stop that goes to Models.
  const link = within(summary).getByRole("link", { name: "Change in Models" });
  expect(link.getAttribute("href")).toBe("/settings/models");
  link.focus();
  expect(document.activeElement).toBe(link);

  // The editable credential control (the Claude token picker) is dimmed but still operable.
  const token = screen.getByLabelText("Token the judge spends") as HTMLSelectElement;
  expect(opacityChain(token)).toBeCloseTo(0.45, 2);
  await userEvent.click(token);
  expect(document.activeElement).toBe(token);
});

it("stacks the label and value side by side at 1280px", async () => {
  await page.viewport(1280, 900);
  renderInGutter();
  const summary = await screen.findByRole("region", { name: "Where your judge runs" });
  await waitFor(() => expect(summary.querySelectorAll("dt")).toHaveLength(5));
  const [dt, dd] = [summary.querySelector("dt")!, summary.querySelector("dd")!];
  expect(dd.getBoundingClientRect().left).toBeGreaterThan(dt.getBoundingClientRect().right);
  expect(Math.abs(dd.getBoundingClientRect().top - dt.getBoundingClientRect().top)).toBeLessThan(4);
});
