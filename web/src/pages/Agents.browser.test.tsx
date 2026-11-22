import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { EffectiveSettings } from "../lib/apiTypes";
import { Agents } from "./Agents";
import { api, type AgentTemplate, type TemplateAllocation, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listAgentTemplates: vi.fn(),
      getTemplateAllocations: vi.fn(),
      setTemplateAllocations: vi.fn(),
      getEffectiveSettings: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const mockUseAuth = vi.mocked(useAuth);
const originalTheme = document.documentElement.getAttribute("data-theme");

const MEMBER: User = {
  id: "u-mira",
  email: "mira@uzi.local",
  display_name: "Alex",
  is_admin: false,
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

function tmpl(over: Partial<AgentTemplate> & Pick<AgentTemplate, "id" | "name" | "scope">): AgentTemplate {
  return {
    description: `desc ${over.name}`,
    model: null,
    codex_model: null,
    tools: null,
    prompt_body: "body",
    is_builtin: over.scope === "builtin",
    user_id: null,
    updated_by: null,
    differs_from_builtin: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function alloc(over: Partial<TemplateAllocation> & Pick<TemplateAllocation, "id" | "name" | "scope">): TemplateAllocation {
  return {
    description: `desc ${over.name}`,
    is_builtin: true,
    global_default: true,
    my_override: null,
    effective: true,
    ...over,
  };
}

const EFFECTIVE = {
  version: 1,
  harnesses: [
    { harness: "claude", usable: true, refusal: null, refusal_detail: null, credential: null },
  ],
  models: [
    { harness: "claude", model: null, default_model: "", default_source: "builtin", resolved_model: "opus-a", source: "user_default", setting: "worker_model", fixed_at: "claim", ignored: null, notes: [] },
  ],
  efforts: [
    { harness: "claude", effort: null, default_effort: "", default_source: "builtin", resolved_effort: "high", source: "user_default", mode: "explicit", setting: "worker_effort", fixed_at: "claim", notes: [] },
  ],
} as unknown as EffectiveSettings;

beforeEach(() => {
  mockUseAuth.mockReturnValue({ user: MEMBER } as ReturnType<typeof useAuth>);
  mockApi.listAgentTemplates.mockResolvedValue({
    templates: [
      tmpl({ id: "b-lead", name: "lead", scope: "builtin" }),
      tmpl({ id: "b-coder", name: "coder", scope: "builtin", model: "sonnet", codex_model: "gpt-6-sol", effort: "high", codex_effort: "low" }),
      tmpl({ id: "b-reviewer", name: "reviewer", scope: "builtin" }),
    ],
  });
  mockApi.getTemplateAllocations.mockResolvedValue({
    templates: [
      alloc({ id: "b-lead", name: "lead", scope: "builtin" }),
      alloc({ id: "b-coder", name: "coder", scope: "builtin" }),
      alloc({ id: "b-reviewer", name: "reviewer", scope: "builtin" }),
    ],
  });
  mockApi.getEffectiveSettings.mockResolvedValue(EFFECTIVE);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});

function expectVisible(el: HTMLElement, what: string) {
  const rect = el.getBoundingClientRect();
  expect(rect.width, what).toBeGreaterThan(0);
  expect(rect.height, what).toBeGreaterThan(0);
}

it.each(["dawn", "ember"])("shows per-family pins, the collapsed default and the lead line in %s", async (theme) => {
  document.documentElement.setAttribute("data-theme", theme);
  render(
    <MemoryRouter>
      <Agents />
    </MemoryRouter>,
  );

  const coderRow = (await screen.findByRole("link", { name: "coder" })).closest("tr")!;
  expectVisible(within(coderRow).getByLabelText("Claude model and effort"), "Claude cell");
  expectVisible(within(coderRow).getByLabelText("Codex model and effort"), "Codex cell");

  const reviewerRow = screen.getByRole("link", { name: "reviewer" }).closest("tr")!;
  expectVisible(within(reviewerRow).getByText("Default · follows lead"), "collapsed default");
  expectVisible(within(reviewerRow).getByText("effort: Default · follows the run's effort"), "collapsed effort");

  const leadRow = screen.getByRole("link", { name: "lead" }).closest("tr")!;
  expectVisible(await within(leadRow).findByText(/^For your Claude runs/), "lead line");
});
