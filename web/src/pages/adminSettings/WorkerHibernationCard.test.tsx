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

type Keys = {
  worker_hibernation_enabled?: string;
  worker_hibernation_auto?: string;
  worker_hibernation_idle_after?: string;
};
const settingsWith = (v: string | undefined, rest: Keys = {}) =>
  ({ worker_hibernation_enabled: v, ...rest }) as unknown as AppSettings;
const response = (keys: Keys) => ({ settings: keys }) as unknown as SettingsResponse;

const SAVE = "Save worker hibernation settings";
const allowBox = () => screen.getByRole("checkbox", { name: "Allow hibernating persistent hosted workers" }) as HTMLInputElement;
const autoBox = () => screen.getByRole("checkbox", { name: "Automatically hibernate idle workers" }) as HTMLInputElement;
const threshold = () => screen.getByLabelText("Idle threshold") as HTMLInputElement;
const saveButton = () => screen.getByRole("button", { name: SAVE }) as HTMLButtonElement;

describe("WorkerHibernationCard", () => {
  it("loads the current value and keeps Save disabled until it changes", () => {
    render(<WorkerHibernationCard settings={settingsWith("true")} onSaved={vi.fn()} />);
    expect(allowBox().checked).toBe(true);
    expect(saveButton().disabled).toBe(true);
  });

  it("renders the automatic switch and idle threshold from settings", () => {
    render(
      <WorkerHibernationCard
        settings={settingsWith("true", { worker_hibernation_auto: "true", worker_hibernation_idle_after: "48h" })}
        onSaved={vi.fn()}
      />,
    );
    expect(autoBox().checked).toBe(true);
    expect(threshold().value).toBe("48h");
    expect(screen.getByText("2 days without run activity")).toBeTruthy();
    expect(screen.getByText(/Go duration, e\.g\. 336h = 14 days; 1h–2160h/)).toBeTruthy();
    expect(screen.getByText(/At most 10 workers per minute/)).toBeTruthy();
    expect(screen.getByText(/including a user's last one/)).toBeTruthy();
    // Hibernation is allowed, so the "only while allowed" hint is absent.
    expect(screen.queryByText(/Takes effect only while hibernation is allowed/)).toBeNull();
  });

  it("defaults absent keys to off and 336h, and hints that auto needs hibernation allowed", () => {
    render(<WorkerHibernationCard settings={settingsWith(undefined)} onSaved={vi.fn()} />);
    expect(autoBox().checked).toBe(false);
    expect(threshold().value).toBe("336h");
    expect(screen.getByText("14 days without run activity")).toBeTruthy();
    expect(screen.getByText("Takes effect only while hibernation is allowed above.")).toBeTruthy();
    fireEvent.click(allowBox());
    expect(screen.queryByText(/Takes effect only while hibernation is allowed/)).toBeNull();
  });

  it("enables Save when any one of the three keys differs", () => {
    render(<WorkerHibernationCard settings={settingsWith("false")} onSaved={vi.fn()} />);
    expect(saveButton().disabled).toBe(true);
    fireEvent.click(autoBox());
    expect(saveButton().disabled).toBe(false);
    fireEvent.click(autoBox());
    expect(saveButton().disabled).toBe(true);
    fireEvent.change(threshold(), { target: { value: "72h" } });
    expect(saveButton().disabled).toBe(false);
    fireEvent.change(threshold(), { target: { value: "336h" } });
    expect(saveButton().disabled).toBe(true);
  });

  it("saves all three keys in one call and re-seeds from the response", async () => {
    const onSaved = vi.fn();
    const resp = response({
      worker_hibernation_enabled: "true",
      worker_hibernation_auto: "true",
      worker_hibernation_idle_after: "720h",
    });
    mockApi.updateSettings.mockResolvedValue(resp);
    render(<WorkerHibernationCard settings={settingsWith(undefined)} onSaved={onSaved} />);
    fireEvent.click(allowBox());
    fireEvent.click(autoBox());
    fireEvent.change(threshold(), { target: { value: "30d" } });
    fireEvent.click(saveButton());
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        worker_hibernation_enabled: "true",
        worker_hibernation_auto: "true",
        worker_hibernation_idle_after: "30d",
      }),
    );
    expect(mockApi.updateSettings).toHaveBeenCalledTimes(1);
    expect(await screen.findByText("Worker hibernation settings saved.")).toBeTruthy();
    expect(onSaved).toHaveBeenCalledWith(resp);
    expect(threshold().value).toBe("720h");
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
    fireEvent.click(allowBox());
    fireEvent.click(saveButton());
    expect(await screen.findByText("nope")).toBeTruthy();
  });

  it("shows a threshold range rejection and keeps the typed value", async () => {
    const onSaved = vi.fn();
    mockApi.updateSettings.mockRejectedValue(
      new ApiError(400, "worker_hibernation_idle_after: must be between 1h and 2160h"),
    );
    render(<WorkerHibernationCard settings={settingsWith("true")} onSaved={onSaved} />);
    fireEvent.change(threshold(), { target: { value: "3000h" } });
    fireEvent.click(saveButton());
    expect(await screen.findByText(/must be between 1h and 2160h/)).toBeTruthy();
    expect(threshold().value).toBe("3000h");
    expect(onSaved).not.toHaveBeenCalled();
    expect(saveButton().disabled).toBe(false);
  });
});
