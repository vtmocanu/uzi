import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen, within } from "@testing-library/react";
import { CrossCheckDefaults } from "./CrossCheckDefaults";
import type { UserSettings } from "../lib/apiTypes";

vi.mock("../lib/api", () => ({ api: {} }));
afterEach(cleanup);
const settings: UserSettings = {
  default_model: null, default_harness: null, default_claude_model: "allocated-custom-model",
  default_codex_model: null, default_effort: null, judge_model: null, summary_model: null,
  theme: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null,
  cross_check_pins: [
    { stage: "plan", harness: "claude", model: "custom-claude-checker", effort: "max",
      worker_default_model: "allocated-custom-model", resolved_model: "custom-claude-checker", resolved_effort: "max", model_source: "pin", effort_source: "pin", active: false },
    { stage: "plan", harness: "codex", model: "custom-codex-checker", effort: null,
      worker_default_model: "gpt-6.1-sol", resolved_model: "custom-codex-checker", resolved_effort: "medium", model_source: "pin", effort_source: "worker default", active: true },
  ],
};

it.each([390, 1100])("keeps one accessible Plan row in aligned checker columns at %ipx", async width => {
  await page.viewport(width, 900);
  render(<div className="p-4"><CrossCheckDefaults settings={settings} onSaved={() => {}} /></div>);
  const table = screen.getByRole("table", { name: "Cross-check defaults" });
  expect(within(table).getAllByRole("row")).toHaveLength(2);
  const claude = screen.getByLabelText("Claude checker model");
  const codex = screen.getByLabelText("Codex checker model");
  expect(Math.abs(claude.getBoundingClientRect().top - codex.getBoundingClientRect().top)).toBeLessThan(1);
  expect(claude.getBoundingClientRect().right).toBeLessThanOrEqual(codex.getBoundingClientRect().left);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
  expect(getComputedStyle(table.parentElement!).overflowX).toBe("auto");
  const custom = screen.getByLabelText("Custom claude checker model ID") as HTMLInputElement;
  expect(custom.disabled).toBe(false);
  custom.focus();
  expect(document.activeElement).toBe(custom);
  expect(custom.getBoundingClientRect().width).toBeGreaterThan(150);
});
