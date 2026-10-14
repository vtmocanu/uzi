// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { HostedWorkerQuotaCard } from "./HostedWorkerQuotaCard";
import { api, ApiError, type AppSettings, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);
const settings = (value: string): AppSettings => ({ hosted_worker_quota: value }) as AppSettings;
const response = (value: string): SettingsResponse => ({ settings: settings(value), secrets: {}, sources: {}, slack_status: "disabled" });
function Harness({ initial = "2" }: { initial?: string }) {
  const [saved, setSaved] = useState(settings(initial));
  return <HostedWorkerQuotaCard settings={saved} onSaved={(resp) => setSaved(resp.settings)} />;
}
const input = () => screen.getByRole("spinbutton", { name: "Hosted workers per user" }) as HTMLInputElement;
const button = () => screen.getByRole("button", { name: "Save hosted worker quota" }) as HTMLButtonElement;
afterEach(() => { cleanup(); vi.resetAllMocks(); });

describe("HostedWorkerQuotaCard", () => {
  it("shows the saved quota and only enables Save for a change", () => {
    render(<Harness initial="3" />);
    expect(input().value).toBe("3");
    expect(button().disabled).toBe(true);
    fireEvent.change(input(), { target: { value: "4" } });
    expect(button().disabled).toBe(false);
    fireEvent.change(input(), { target: { value: "3" } });
    expect(button().disabled).toBe(true);
    fireEvent.submit(button().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it.each(["+3", " 3 ", "3"])("normalizes saved quota %j for display and dirty comparison", (initial) => {
    render(<Harness initial={initial} />);
    expect(input().value).toBe("3");
    expect(button().disabled).toBe(true);
    fireEvent.change(input(), { target: { value: "4" } });
    expect(button().disabled).toBe(false);
    fireEvent.change(input(), { target: { value: "3" } });
    expect(button().disabled).toBe(true);
    fireEvent.submit(button().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it.each(["+3", " 3 "])("normalizes save-response quota %j", async (saved) => {
    mockApi.updateSettings.mockResolvedValue(response(saved));
    render(<Harness />);
    fireEvent.change(input(), { target: { value: "3" } });
    fireEvent.click(button());
    expect(await screen.findByText("Hosted worker quota saved.")).toBeTruthy();
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ hosted_worker_quota: "3" });
    expect(input().value).toBe("3");
    expect(button().disabled).toBe(true);
  });

  it.each(["0", "3", "20"])("saves %s independently and becomes clean", async (value) => {
    mockApi.updateSettings.mockResolvedValue(response(value));
    render(<Harness />);
    fireEvent.change(input(), { target: { value } });
    fireEvent.click(button());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ hosted_worker_quota: value }));
    expect(await screen.findByText("Hosted worker quota saved.")).toBeTruthy();
    expect(input().value).toBe(value);
    expect(button().disabled).toBe(true);
  });

  it.each(["-1", "21", "1.5", ""])("refuses invalid value %j without a request", (value) => {
    render(<Harness />);
    fireEvent.change(input(), { target: { value } });
    expect(input().getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText(/must be/i)).toBeTruthy();
    expect(button().disabled).toBe(true);
    fireEvent.submit(button().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("surfaces a server 400 and preserves the edit for retry", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "hosted_worker_quota: rejected by server"));
    render(<Harness />);
    fireEvent.change(input(), { target: { value: "3" } });
    fireEvent.click(button());
    expect(await screen.findByText("hosted_worker_quota: rejected by server")).toBeTruthy();
    expect(input().value).toBe("3");
    expect(button().disabled).toBe(false);
  });
});
