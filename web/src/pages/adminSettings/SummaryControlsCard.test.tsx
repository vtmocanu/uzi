// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SummaryControlsCard } from "./SummaryControlsCard";
import { api, type AppSettings, type SettingSource } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

function renderCard(settings: Partial<AppSettings>, sources: Record<string, SettingSource> = {}) {
  render(<SummaryControlsCard settings={settings as AppSettings} sources={sources} onSaved={vi.fn()} />);
}

const nowBox = () => screen.getByRole("checkbox", { name: "Model-written Now line" }) as HTMLInputElement;
const save = () => screen.getByRole("button", { name: "Save run summary controls" }) as HTMLButtonElement;

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("SummaryControlsCard (#3259)", () => {
  it("shows the Now line control with its explanation and a Model defaults link", () => {
    renderCard({ now_summary_enabled: "true" });
    expect(nowBox().checked).toBe(true);
    expect(screen.getByText(/Instance kill-switch\./)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Model defaults" }).getAttribute("href")).toBe("#model-defaults");
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("keeps Save disabled until the checkbox changes", () => {
    renderCard({ now_summary_enabled: "true" });
    expect(save().disabled).toBe(true);
    fireEvent.click(nowBox());
    expect(save().disabled).toBe(false);
    fireEvent.click(nowBox());
    expect(save().disabled).toBe(true);
  });

  it("sends exactly now_summary_enabled when toggled off and saved", async () => {
    mockApi.updateSettings.mockResolvedValue({ settings: { now_summary_enabled: "false" }, sources: {} } as never);
    renderCard({ now_summary_enabled: "true" });
    fireEvent.click(nowBox());
    fireEvent.click(save());
    await screen.findByText("Run summary controls saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledTimes(1);
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ now_summary_enabled: "false" });
    expect(nowBox().checked).toBe(false);
  });

  it("reads an absent setting as on", () => {
    renderCard({});
    expect(nowBox().checked).toBe(true);
    expect(save().disabled).toBe(true);
  });

  it("disables the control and never sends when set from the environment", () => {
    renderCard({ now_summary_enabled: "false" }, { now_summary_enabled: "env" });
    expect(nowBox().disabled).toBe(true);
    expect(nowBox().checked).toBe(false);
    expect(screen.getByText("Set from the environment")).toBeTruthy();
    expect(save().disabled).toBe(true);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("shows the server error and keeps the control editable on a failed save", async () => {
    mockApi.updateSettings.mockRejectedValue(new Error("boom"));
    renderCard({ now_summary_enabled: "true" });
    fireEvent.click(nowBox());
    fireEvent.click(save());
    await screen.findByText("Failed to save run summary controls");
    expect(save().disabled).toBe(false);
  });
});
