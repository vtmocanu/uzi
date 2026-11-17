// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { EphemeralWorkersCard } from "./EphemeralWorkersCard";
import { api, ApiError, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const settings = {
  ephemeral_workers_enabled: "false",
  ephemeral_max_per_user: "2",
  ephemeral_default_size: "m",
  ephemeral_lease: "2h",
} as AppSettings;
const response = (over: Record<string, string>): SettingsResponse => ({
  settings: { ...settings, ...over } as AppSettings,
  secrets: {},
  sources: {},
  slack_status: "disabled",
});
const noop = () => {};
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

function renderCard(sources: Record<string, SettingSource> = {}) {
  render(<EphemeralWorkersCard settings={settings} sources={sources} defaults={{ ephemeral_lease: "2h" }} onSaved={noop} />);
}

describe("EphemeralWorkersCard tuning controls", () => {
  it("shows each effective value with its source label", () => {
    renderCard({ ephemeral_max_per_user: "db", ephemeral_default_size: "env" });
    expect((screen.getByLabelText("Per-user cap") as HTMLInputElement).value).toBe("2");
    expect((screen.getByLabelText("Ephemeral default size") as HTMLSelectElement).value).toBe("m");
    expect((screen.getByLabelText("Idle lease") as HTMLInputElement).value).toBe("2h");
    expect(screen.getByText("Saved")).toBeTruthy();
    expect(screen.getByText("Set by environment")).toBeTruthy();
    expect(screen.getByText(/^Default · built-in default 2h$/)).toBeTruthy();
    expect(screen.getByText(/0s disables; maximum 2h/)).toBeTruthy();
  });

  it("saves only the edited key", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ ephemeral_lease: "30m" }));
    renderCard();
    fireEvent.change(screen.getByLabelText("Idle lease"), { target: { value: "30m" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Idle lease" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ ephemeral_lease: "30m" }));
    expect(await screen.findByText("Idle lease saved.")).toBeTruthy();
  });

  it("saves the default size and the cap by their own keys", async () => {
    mockApi.updateSettings.mockResolvedValue(response({}));
    renderCard();
    fireEvent.change(screen.getByLabelText("Ephemeral default size"), { target: { value: "l-highmem" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Ephemeral default size" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ ephemeral_default_size: "l-highmem" }));
    fireEvent.change(screen.getByLabelText("Per-user cap"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Per-user cap" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ ephemeral_max_per_user: "5" }));
  });

  it("blocks an out-of-range cap before any request", () => {
    renderCard();
    fireEvent.change(screen.getByLabelText("Per-user cap"), { target: { value: "21" } });
    expect(screen.getByText("Must be a whole number from 0 to 20")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save Per-user cap" }) as HTMLButtonElement).disabled).toBe(true);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("resets a saved key by PUTting an empty string", async () => {
    mockApi.updateSettings.mockResolvedValue(response({}));
    renderCard({ ephemeral_lease: "db" });
    fireEvent.click(screen.getByRole("button", { name: "Reset to default: Idle lease" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ ephemeral_lease: "" }));
  });

  it("offers no reset for a default-sourced key", () => {
    renderCard({});
    expect(screen.queryByRole("button", { name: /^Reset / })).toBeNull();
  });

  it("locks an environment-sourced key: disabled, no save, no reset", () => {
    renderCard({ ephemeral_lease: "env" });
    expect((screen.getByLabelText("Idle lease") as HTMLInputElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Save Idle lease" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Reset to default: Idle lease" })).toBeNull();
  });

  it("shows the server's message inline when the PUT fails", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "ephemeral_lease: must be between 0s and 2h"));
    renderCard();
    fireEvent.change(screen.getByLabelText("Idle lease"), { target: { value: "3h" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Idle lease" }));
    expect((await screen.findByRole("alert")).textContent).toContain("must be between 0s and 2h");
  });
});
