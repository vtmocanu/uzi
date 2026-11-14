// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { api, ApiError, type AppSettings, type SettingsResponse } from "../../lib/api";
import { WorkerHibernationCard } from "./WorkerHibernationCard";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const settingsWith = (v: string | undefined) => ({ worker_hibernation_enabled: v }) as unknown as AppSettings;
const response = (v: string) => ({ settings: { worker_hibernation_enabled: v } }) as unknown as SettingsResponse;

describe("WorkerHibernationCard", () => {
  it("loads the current value and keeps Save disabled until it changes", () => {
    render(<WorkerHibernationCard settings={settingsWith("true")} onSaved={vi.fn()} />);
    expect((screen.getByRole("checkbox") as HTMLInputElement).checked).toBe(true);
    expect((screen.getByRole("button", { name: "Save worker hibernation settings" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("treats an absent key as off and saves the toggle", async () => {
    const onSaved = vi.fn();
    mockApi.updateSettings.mockResolvedValue(response("true"));
    render(<WorkerHibernationCard settings={settingsWith(undefined)} onSaved={onSaved} />);
    const box = screen.getByRole("checkbox") as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    fireEvent.click(screen.getByRole("button", { name: "Save worker hibernation settings" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ worker_hibernation_enabled: "true" }));
    expect(await screen.findByText("Worker hibernation settings saved.")).toBeTruthy();
    expect(onSaved).toHaveBeenCalledWith(response("true"));
  });

  it("explains that turning it off wakes every hibernated worker and that waking never depends on it", () => {
    render(<WorkerHibernationCard settings={settingsWith("false")} onSaved={vi.fn()} />);
    expect(screen.getByText(/every hibernated or hibernating worker/)).toBeTruthy();
    expect(screen.getByText(/at most 10 per pass/)).toBeTruthy();
    expect(screen.getByText(/Waking a worker never depends on this setting/)).toBeTruthy();
  });

  it("shows the server error when the save fails", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "nope"));
    render(<WorkerHibernationCard settings={settingsWith("false")} onSaved={vi.fn()} />);
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: "Save worker hibernation settings" }));
    expect(await screen.findByText("nope")).toBeTruthy();
  });
});
