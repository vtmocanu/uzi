// PRD #3263 M1 in a real browser (layout needs a layout engine): below 768px the grid's
// cells stack under their row with a visible family label; from 768px Claude and Codex sit
// side by side under column headers; the sticky Save bar never covers the last row; and every
// control's computed accessible name is "<Row> — <Family>".
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { api, type EffectiveSettings, type UserSettings } from "../lib/api";
import { ModelsSettings } from "./ModelsSettings";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getMySettings: vi.fn(), getEffectiveSettings: vi.fn(), putMyModelSettings: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ user: { id: "u1" } }) }));

const mockApi = vi.mocked(api);

const settings = {
  default_harness: null, default_model: null, default_claude_model: "sonnet", default_codex_model: null,
  default_effort: null, default_codex_effort: "runtime", judge_model: null, summary_model: null, judge_runs_on: null,
  harness_overflow_enabled: false, theme: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null,
  planning_claude_model: null, planning_claude_effort: null, planning_codex_model: null, planning_codex_effort: null,
} as UserSettings;

const effective = {
  version: 1,
  harnesses: [
    { harness: "claude", usable: true, refusal: null, refusal_detail: null, credential: null },
    { harness: "codex", usable: true, refusal: null, refusal_detail: null, credential: null },
  ],
  start: [{ choice: "default", harness: { value: null, source: "chosen_at_create", fixed_at: "create", notes: ["automatic_weekly_reset"] }, refusal: null, refusal_detail: null, anthropic_override_allowed: true }],
  models: (["worker_model", "planning_model"] as const).flatMap((setting) => (["claude", "codex"] as const).map((harness) => ({
    harness, setting, model: null, default_model: harness === "claude" ? "opus" : "gpt-6.1-sol", default_source: "builtin",
    resolved_model: harness === "claude" ? "sonnet" : "gpt-6.1-sol", source: harness === "claude" ? "user_default" : "builtin",
    fixed_at: "claim", ignored: null, notes: [],
  }))),
  efforts: (["worker_effort", "planning_effort"] as const).flatMap((setting) => (["claude", "codex"] as const).map((harness) => ({
    harness, setting, effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium",
    source: "builtin", mode: "explicit", fixed_at: "claim", notes: [],
  }))),
  cross_check: { models: [], efforts: [] },
  judge_harness: { value: null, source: "same_as_run", fixed_at: "create", notes: [] },
} as unknown as EffectiveSettings;

const NAMES = [
  "Default harness", "Worker model — Claude", "Worker model — Codex", "Worker effort — Claude", "Worker effort — Codex",
  "Planning model — Claude", "Planning model — Codex", "Planning effort — Claude", "Planning effort — Codex",
];

async function mount(width: number) {
  await page.viewport(width, 700);
  const router = createMemoryRouter([{ path: "*", element: <ModelsSettings /> }], { initialEntries: ["/settings/models"] });
  render(
    // AppShell's <main> padding, which the settings tab row's negative margins assume.
    <div className="px-4 sm:px-6">
      <RouterProvider router={router} />
    </div>,
  );
  await screen.findByRole("combobox", { name: "Worker model — Claude" });
  // Make the bar carry a change so it is at its tallest.
  fireEvent.change(screen.getByRole("combobox", { name: "Worker model — Codex" }), { target: { value: "gpt-6-sol" } });
}

const rect = (el: Element) => el.getBoundingClientRect();
const cellOf = (name: string) => screen.getByRole("combobox", { name }).closest("[data-field]")!;
const familyLabel = (name: string) => cellOf(name).querySelector("label")!;

function expectBarClearsLastRow() {
  window.scrollTo(0, document.documentElement.scrollHeight);
  const rows = document.querySelectorAll("[data-row]");
  const last = rows[rows.length - 1];
  const bar = screen.getByTestId("models-save-bar");
  expect(rect(last).bottom).toBeLessThanOrEqual(rect(bar).top + 0.5);
  // Sticky: the bar is inside the viewport.
  expect(rect(bar).bottom).toBeLessThanOrEqual(window.innerHeight + 0.5);
}

beforeEach(() => {
  mockApi.getMySettings.mockResolvedValue({ settings });
  mockApi.getEffectiveSettings.mockResolvedValue(effective);
});
afterEach(() => {
  cleanup();
  window.scrollTo(0, 0);
});

describe("Models grid layout", () => {
  // 700px is inside the 640-768 band where three tracks would squeeze each select to ~163px.
  it.each([390, 700])("stacks the cells below 768px (%ipx), each with a visible family label", async (width) => {
    await mount(width);
    for (const row of ["Worker model", "Planning effort"]) {
      const claude = rect(cellOf(`${row} — Claude`));
      const codex = rect(cellOf(`${row} — Codex`));
      expect(codex.top, row).toBeGreaterThanOrEqual(claude.bottom);
      expect(Math.abs(codex.left - claude.left), row).toBeLessThan(1);
      for (const family of ["Claude", "Codex"]) {
        const label = familyLabel(`${row} — ${family}`);
        expect(rect(label).width, `${row} ${family} label`).toBeGreaterThan(20);
        expect(label.textContent).toContain(family);
      }
    }
    // At the top of a page taller than the viewport, the bar is pinned to the viewport bottom.
    window.scrollTo(0, 0);
    expect(document.documentElement.scrollHeight).toBeGreaterThan(window.innerHeight);
    const bar = rect(screen.getByTestId("models-save-bar"));
    expect(bar.bottom).toBeLessThanOrEqual(window.innerHeight + 0.5);
    expect(bar.top).toBeLessThan(window.innerHeight);
    // Nothing overflows the narrow viewport sideways.
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
    expectBarClearsLastRow();
  });

  it.each([768, 1024])("puts Claude and Codex side by side from 768px (%ipx), labelled by the column headers", async (width) => {
    await mount(width);
    const claude = rect(cellOf("Worker effort — Claude"));
    const codex = rect(cellOf("Worker effort — Codex"));
    expect(Math.abs(codex.top - claude.top)).toBeLessThan(1);
    expect(codex.left).toBeGreaterThanOrEqual(claude.right);
    // The per-cell family label is visually hidden (sr-only) where the headers show.
    expect(rect(familyLabel("Worker effort — Claude")).width).toBeLessThanOrEqual(1);
    expectBarClearsLastRow();
  });

  it("computes every control's accessible name as <Row> — <Family>", async () => {
    await mount(1024);
    for (const name of NAMES) {
      const el = screen.getByRole("combobox", { name });
      expect(el.getAttribute("aria-describedby"), name).toContain("-saved");
    }
  });
});
