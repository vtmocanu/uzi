// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { FetchCapsCard } from "./FetchCapsCard";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});

const mockApi = vi.mocked(api);

// Only the four fetch-cap keys are read by the card; the rest of AppSettings is irrelevant.
function settings(over: Partial<AppSettings> = {}): AppSettings {
  return {
    fetch_max_file_bytes: "26214400",
    fetch_max_run_bytes: "209715200",
    fetch_max_run_files: "100",
    fetch_max_concurrent_per_run: "4",
    ...over,
  } as AppSettings;
}

function response(s: AppSettings): SettingsResponse {
  return { settings: s, secrets: {}, sources: {} } as unknown as SettingsResponse;
}

function renderCard(s = settings(), sources: Record<string, SettingSource> = {}, onSaved = vi.fn()) {
  render(
    <MemoryRouter>
      <FetchCapsCard settings={s} sources={sources} onSaved={onSaved} />
    </MemoryRouter>,
  );
  return onSaved;
}

const fileInput = () => screen.getByLabelText("Largest single download (MiB)") as HTMLInputElement;
const runInput = () => screen.getByLabelText("Total download per run (MiB)") as HTMLInputElement;
const filesInput = () => screen.getByLabelText("Downloads per run") as HTMLInputElement;
const concInput = () => screen.getByLabelText("Fetches in flight per run") as HTMLInputElement;
const saveBtn = () => screen.getByRole("button", { name: "Save fetch caps" }) as HTMLButtonElement;

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("FetchCapsCard", () => {
  it("shows the byte caps in MiB and the counts as-is, with nothing to save", () => {
    renderCard();
    expect(fileInput().value).toBe("25");
    expect(runInput().value).toBe("200");
    expect(filesInput().value).toBe("100");
    expect(concInput().value).toBe("4");
    expect(saveBtn().disabled).toBe(true);
  });

  it("saves only the changed cap, converted from MiB to bytes", async () => {
    mockApi.updateSettings.mockResolvedValue(response(settings({ fetch_max_file_bytes: "52428800" })));
    const onSaved = renderCard();
    fireEvent.change(fileInput(), { target: { value: "50" } });
    fireEvent.click(saveBtn());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledTimes(1));
    expect(mockApi.updateSettings.mock.calls[0][0]).toEqual({ fetch_max_file_bytes: "52428800" });
    expect(await screen.findByText("Fetch caps saved.")).toBeTruthy();
    expect(onSaved).toHaveBeenCalledTimes(1);
  });

  it("accepts a fractional MiB and rounds it to whole bytes", async () => {
    mockApi.updateSettings.mockResolvedValue(response(settings()));
    renderCard();
    fireEvent.change(fileInput(), { target: { value: "0.5" } });
    fireEvent.click(saveBtn());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ fetch_max_file_bytes: "524288" }));
  });

  it.each([
    ["zero bytes", fileInput, "0", "Must be more than 0 and at most 1024 MiB"],
    ["above 1 GiB per file", fileInput, "1025", "Must be more than 0 and at most 1024 MiB"],
    ["above 10 GiB per run", runInput, "10241", "Must be more than 0 and at most 10240 MiB"],
    ["not a number", runInput, "lots", "Enter a size in MiB, such as 25 or 0.5"],
    ["zero files", filesInput, "0", "Must be between 1 and 10000"],
    ["too many files", filesInput, "10001", "Must be between 1 and 10000"],
    ["a fractional count", filesInput, "2.5", "Enter a whole number"],
    ["too many in flight", concInput, "33", "Must be between 1 and 32"],
  ])("refuses %s before it reaches the api", (_name, input, value, message) => {
    renderCard();
    fireEvent.change(input(), { target: { value } });
    expect(screen.getByText(message)).toBeTruthy();
    expect(input().getAttribute("aria-invalid")).toBe("true");
    expect(saveBtn().disabled).toBe(true);
    fireEvent.submit(saveBtn().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("accepts each bound exactly", async () => {
    mockApi.updateSettings.mockResolvedValue(response(settings()));
    renderCard();
    fireEvent.change(fileInput(), { target: { value: "1024" } });
    fireEvent.change(runInput(), { target: { value: "10240" } });
    fireEvent.change(filesInput(), { target: { value: "1" } });
    fireEvent.change(concInput(), { target: { value: "32" } });
    expect(saveBtn().disabled).toBe(false);
    fireEvent.click(saveBtn());
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        fetch_max_file_bytes: String(1 << 30),
        fetch_max_run_bytes: String(10 * 2 ** 30),
        fetch_max_run_files: "1",
        fetch_max_concurrent_per_run: "32",
      }),
    );
  });

  it("does not rewrite a stored byte count that is not a whole MiB when it is left untouched", async () => {
    mockApi.updateSettings.mockResolvedValue(response(settings()));
    renderCard(settings({ fetch_max_file_bytes: "1000000" }));
    expect(fileInput().value).toBe("0.954");
    fireEvent.change(concInput(), { target: { value: "8" } });
    fireEvent.click(saveBtn());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ fetch_max_concurrent_per_run: "8" }));
  });

  it("links to the Site lists page", () => {
    renderCard();
    expect(screen.getByRole("link", { name: "site list" }).getAttribute("href")).toBe("/admin/egress-profiles");
  });

  it("shows an env-sourced cap read-only and never sends it", () => {
    renderCard(settings(), { fetch_max_run_files: "env" });
    expect(filesInput().disabled).toBe(true);
    expect(screen.getByText("Set by an environment variable, so it can't be changed here.")).toBeTruthy();
    expect(fileInput().disabled).toBe(false);
  });
});
