import "../../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { InstanceModelDefaultsCard } from "./InstanceModelDefaultsCard";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const DEFAULTS = {
  judge_model: "opus",
  codex_judge_model: "gpt-6.2-sol",
  summary_model: "haiku",
  codex_summary_model: "gpt-6-luna",
};

type View = { settings: AppSettings; sources: Record<string, SettingSource> };
const view = (codexJudge = "", codexSummary = ""): View => ({
  settings: {
    ...DEFAULTS,
    codex_judge_model: codexJudge === "" ? DEFAULTS.codex_judge_model : codexJudge,
    codex_summary_model: codexSummary === "" ? DEFAULTS.codex_summary_model : codexSummary,
  } as unknown as AppSettings,
  sources: {
    judge_model: "default",
    codex_judge_model: codexJudge === "" ? "default" : "db",
    summary_model: "default",
    codex_summary_model: codexSummary === "" ? "default" : "db",
  },
});

function Harness({ codexJudge = "", codexSummary = "" }: { codexJudge?: string; codexSummary?: string }) {
  const [v, setV] = useState(view(codexJudge, codexSummary));
  return (
    <InstanceModelDefaultsCard
      settings={v.settings}
      sources={v.sources}
      defaults={DEFAULTS}
      onSaved={(r) => setV({ settings: r.settings, sources: r.sources })}
    />
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

it("lays Claude and Codex side by side on desktop with the header row visible", async () => {
  await page.viewport(1024, 900);
  render(<Harness />);
  for (const row of ["Judge", "Summary"]) {
    const claude = await screen.findByLabelText(`${row} model, Claude`);
    const codex = screen.getByLabelText(`${row} model, Codex`);
    const a = claude.getBoundingClientRect();
    const b = codex.getBoundingClientRect();
    expect(b.left).toBeGreaterThan(a.right - 1);
    expect(Math.abs(b.top - a.top)).toBeLessThan(2);
  }
  // The header row names the setting column and both families.
  expect(screen.getByText("Setting").getBoundingClientRect().width).toBeGreaterThan(0);
  await expect.element(page.getByRole("combobox", { name: "Judge model, Codex" })).toBeVisible();
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(1024);
});

it("stacks to one column on mobile, each cell naming its family, without horizontal overflow", async () => {
  await page.viewport(390, 900);
  render(<Harness />);
  const claude = await screen.findByLabelText("Judge model, Claude");
  const codex = screen.getByLabelText("Judge model, Codex");
  await waitFor(() => expect(codex.getBoundingClientRect().top).toBeGreaterThan(claude.getBoundingClientRect().bottom - 1));

  // The header row is hidden here, so every cell shows its own visible family label.
  const visibleLabels = (family: string) =>
    screen.getAllByText(family).filter((el) => el.getBoundingClientRect().width > 0 && getComputedStyle(el).display !== "none");
  expect(visibleLabels("Claude")).toHaveLength(2);
  expect(visibleLabels("Codex")).toHaveLength(2);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
});

it("pins a Codex judge model through the real select and saves one key", async () => {
  await page.viewport(1024, 900);
  mockApi.updateSettings.mockResolvedValue({
    ...view("gpt-6-sol"),
    secrets: {},
    defaults: DEFAULTS,
    slack_status: "disabled",
  } as SettingsResponse);
  render(<Harness />);
  const codex = (await screen.findByLabelText("Judge model, Codex")) as HTMLSelectElement;
  await userEvent.selectOptions(codex, "gpt-6-sol");
  await userEvent.click(screen.getByRole("button", { name: "Save model defaults" }));
  await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ codex_judge_model: "gpt-6-sol" }));
  await screen.findByText("Model defaults saved.");
  expect(codex.value).toBe("gpt-6-sol");
});

// Ported from the retired SummarySettingsCard browser suite: the Codex summary lane's reset
// through the real select sends "" and lands back on Default after the response.
it("resets a pinned Codex summary lane to Default through the real select and saves an empty string", async () => {
  mockApi.updateSettings.mockResolvedValue({
    ...view(),
    secrets: {},
    defaults: DEFAULTS,
    slack_status: "disabled",
  } as unknown as SettingsResponse);
  render(<Harness codexSummary="gpt-6-astra" />);
  const codex = (await screen.findByLabelText("Summary model, Codex")) as HTMLSelectElement;
  expect(codex.value).toBe("gpt-6-astra");
  await userEvent.selectOptions(codex, "inherit");
  await userEvent.click(screen.getByRole("button", { name: "Save model defaults" }));
  await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ codex_summary_model: "" }));
  await waitFor(() => expect(codex.value).toBe("inherit"));
  expect(screen.getAllByText("Default · gpt-6-luna (built-in)").length).toBeGreaterThan(0);
});
