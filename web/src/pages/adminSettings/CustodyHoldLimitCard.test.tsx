// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { CustodyHoldLimitCard } from "./CustodyHoldLimitCard";
import { api, ApiError, type AppSettings, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);
const settings = (value: string): AppSettings => ({ custody_hold_limit: value }) as AppSettings;
const response = (value: string): SettingsResponse => ({ settings: settings(value), secrets: {}, sources: {}, slack_status: "disabled" });
function Harness({ initial = "8" }: { initial?: string }) {
  const [saved, setSaved] = useState(settings(initial));
  return <CustodyHoldLimitCard settings={saved} onSaved={(resp) => setSaved(resp.settings)} />;
}
const input = () => screen.getByRole("spinbutton", { name: "Recovery holds per owner" }) as HTMLInputElement;
const button = () => screen.getByRole("button", { name: "Save custody hold limit" }) as HTMLButtonElement;
afterEach(() => { cleanup(); vi.resetAllMocks(); });

describe("CustodyHoldLimitCard", () => {
  it("renders the stored value and keeps Save disabled until changed", () => {
    render(<Harness initial="5" />);
    expect(input().value).toBe("5");
    expect(button().disabled).toBe(true);
  });

  it.each(["0", "-1", "abc", "8.5", "+12", "1001", "", "1e3", "1e2"])("rejects %j client-side without calling the API", (value) => {
    render(<Harness />);
    fireEvent.change(input(), { target: { value } });
    expect(input().getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText("must be a whole number between 1 and 1000")).toBeTruthy();
    expect(button().disabled).toBe(true);
    fireEvent.submit(button().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("saves 12 sending only custody_hold_limit", async () => {
    mockApi.updateSettings.mockResolvedValue(response("12"));
    render(<Harness />);
    fireEvent.change(input(), { target: { value: "12" } });
    fireEvent.click(button());
    expect(await screen.findByText("Custody hold limit saved.")).toBeTruthy();
    expect(mockApi.updateSettings).toHaveBeenCalledTimes(1);
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ custody_hold_limit: "12" });
    expect(input().value).toBe("12");
    expect(button().disabled).toBe(true);
  });

  it("surfaces a server error and keeps the edit", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "custody_hold_limit: rejected by server"));
    render(<Harness />);
    fireEvent.change(input(), { target: { value: "12" } });
    fireEvent.click(button());
    expect(await screen.findByText("custody_hold_limit: rejected by server")).toBeTruthy();
    expect(input().value).toBe("12");
    expect(button().disabled).toBe(false);
  });
});
