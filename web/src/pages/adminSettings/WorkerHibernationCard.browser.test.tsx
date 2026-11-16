import "../../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { WorkerHibernationCard } from "./WorkerHibernationCard";
import { api, ApiError, type AppSettings, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const SEED = {
  worker_hibernation_enabled: "true",
  worker_hibernation_auto: "false",
  worker_hibernation_idle_after: "336h",
};

// Harness mirrors AdminSettings: a successful save replaces the settings the card is
// rendered from, so a re-seed and the dirty check are exercised end to end.
function Harness() {
  const [settings, setSettings] = useState(SEED as unknown as AppSettings);
  return <WorkerHibernationCard settings={settings} onSaved={(r) => setSettings(r.settings)} />;
}

const save = () => screen.getByRole("button", { name: "Save worker hibernation settings" }) as HTMLButtonElement;

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

it("saves the switch, automatic hibernation and threshold together and confirms", async () => {
  await page.viewport(390, 900);
  const saved = { ...SEED, worker_hibernation_auto: "true", worker_hibernation_idle_after: "72h" };
  mockApi.updateSettings.mockResolvedValue({ settings: saved } as unknown as SettingsResponse);
  render(<Harness />);
  await userEvent.click(await screen.findByRole("checkbox", { name: "Automatically hibernate idle workers" }));
  const threshold = screen.getByLabelText("Idle threshold") as HTMLInputElement;
  await userEvent.clear(threshold);
  await userEvent.type(threshold, "72h");
  expect(screen.getByText("3 days without run activity")).toBeTruthy();
  await userEvent.click(save());
  await waitFor(() =>
    expect(mockApi.updateSettings).toHaveBeenCalledWith({
      worker_hibernation_enabled: "true",
      worker_hibernation_auto: "true",
      worker_hibernation_idle_after: "72h",
    }),
  );
  expect(await screen.findByText("Worker hibernation settings saved.")).toBeTruthy();
  // Re-seeded from the response: nothing left to save.
  await waitFor(() => expect(save().disabled).toBe(true));
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
});

it("shows a rejected threshold and keeps the typed value", async () => {
  mockApi.updateSettings.mockRejectedValue(
    new ApiError(400, "worker_hibernation_idle_after: must be between 1h and 2160h"),
  );
  render(<Harness />);
  const threshold = (await screen.findByLabelText("Idle threshold")) as HTMLInputElement;
  await userEvent.clear(threshold);
  await userEvent.type(threshold, "3000h");
  await userEvent.click(save());
  expect(await screen.findByText(/must be between 1h and 2160h/)).toBeTruthy();
  expect(threshold.value).toBe("3000h");
  expect(save().disabled).toBe(false);
});

it("keeps Save disabled until a field changes, and the threshold is keyboard reachable", async () => {
  render(<Harness />);
  expect((await screen.findByRole("button", { name: "Save worker hibernation settings" }) as HTMLButtonElement).disabled).toBe(true);
  const allow = screen.getByRole("checkbox", { name: "Allow hibernating persistent hosted workers" });
  allow.focus();
  await userEvent.tab();
  expect(document.activeElement).toBe(screen.getByRole("checkbox", { name: "Automatically hibernate idle workers" }));
  await userEvent.tab();
  expect(document.activeElement).toBe(screen.getByLabelText("Idle threshold"));
  await userEvent.keyboard("{End}{Backspace}");
  expect(save().disabled).toBe(false);
  await userEvent.keyboard("h");
  expect(save().disabled).toBe(true);
});
