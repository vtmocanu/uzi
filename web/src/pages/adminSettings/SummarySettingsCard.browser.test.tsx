import "../../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { SummarySettingsCard } from "./SummarySettingsCard";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const DEFAULTS = { summary_model: "haiku", codex_summary_model: "gpt-6-luna" };

type View = { settings: AppSettings; sources: Record<string, SettingSource> };
const view = (claude: string, codex: string): View => ({
  settings: {
    summary_model: claude === "" ? "haiku" : claude,
    codex_summary_model: codex === "" ? "gpt-6-luna" : codex,
    now_summary_enabled: "true",
  } as AppSettings,
  sources: {
    summary_model: claude === "" ? "default" : "db",
    codex_summary_model: codex === "" ? "default" : "db",
  },
});

function Harness({ claude, codex }: { claude: string; codex: string }) {
  const [v, setV] = useState(view(claude, codex));
  return (
    <SummarySettingsCard
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

it("lays the Claude and Codex pickers side by side on desktop and stacks them on mobile", async () => {
  await page.viewport(1024, 900);
  render(<Harness claude="" codex="" />);
  const claude = await screen.findByLabelText("Claude summary model");
  const codex = screen.getByLabelText("Codex summary model");
  const a = claude.getBoundingClientRect();
  const b = codex.getBoundingClientRect();
  // Same row on a desktop viewport: Codex sits to the right of Claude, tops aligned.
  expect(b.left).toBeGreaterThan(a.right - 1);
  expect(Math.abs(b.top - a.top)).toBeLessThan(2);

  await page.viewport(390, 900);
  await waitFor(() => expect(codex.getBoundingClientRect().top).toBeGreaterThan(claude.getBoundingClientRect().bottom - 1));
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
});

it("resets a pinned Codex lane to Default and saves an empty string", async () => {
  mockApi.updateSettings.mockResolvedValue({
    ...view("", ""),
    secrets: {},
    defaults: DEFAULTS,
    slack_status: "disabled",
  } as SettingsResponse);
  render(<Harness claude="" codex="gpt-6-astra" />);
  const codex = (await screen.findByLabelText("Codex summary model")) as HTMLSelectElement;
  expect(codex.value).toBe("gpt-6-astra");
  await userEvent.selectOptions(codex, "inherit");
  await userEvent.click(screen.getByRole("button", { name: "Save run summary settings" }));
  await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ codex_summary_model: "" }));
  await waitFor(() => expect(codex.value).toBe("inherit"));
  expect(screen.getByText("Default · gpt-6-luna (built-in)")).toBeTruthy();
});
