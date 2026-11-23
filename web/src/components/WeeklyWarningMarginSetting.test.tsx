// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { api, ApiError, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { WeeklyWarningMarginSetting } from "./WeeklyWarningMarginSetting";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return { ...actual, api: { setWeeklyWarningMargin: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function auth(reservePct: number | null) {
  vi.mocked(useAuth).mockReturnValue({ user: { weekly_reserve_pct: reservePct } as User } as ReturnType<typeof useAuth>);
}

const input = () => screen.getByLabelText("Warn this many points before the limit") as HTMLInputElement;
const effect = () => document.querySelector("[data-margin-effect]")?.textContent ?? "";

function mount(saved = 15, onSaved = vi.fn()) {
  render(<WeeklyWarningMarginSetting saved={saved} onSaved={onSaved} />);
  return onSaved;
}

beforeEach(() => {
  auth(null);
  mockApi.setWeeklyWarningMargin.mockImplementation(async (n: number) => ({ weekly_warning_margin_pts: n }));
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("WeeklyWarningMarginSetting (PRD #4123)", () => {
  it("shows the stored margin and what it means, counted back from 100% with no reserve", () => {
    mount(15);
    expect(input().value).toBe("15");
    expect(input().min).toBe("0");
    expect(input().max).toBe("50");
    expect(effect()).toBe("Warnings start at 85% weekly usage, 15 points before the limit.");
    const describedBy = input().getAttribute("aria-describedby") ?? "";
    expect(document.getElementById(describedBy.split(" ")[0])?.textContent).toMatch(/Set 0 to turn early warnings off/);
  });

  it("counts back from the reserve when one is set", () => {
    auth(90);
    mount(15);
    expect(effect()).toBe("Warnings start at 75% weekly usage, 15 points before your 90% reserve.");
  });

  it("saves on blur and reports the stored value to the page", async () => {
    const onSaved = mount(15);
    fireEvent.change(input(), { target: { value: "20" } });
    // The effect line previews the draft before it is saved.
    expect(effect()).toBe("Warnings start at 80% weekly usage, 20 points before the limit.");
    fireEvent.blur(input());
    await waitFor(() => expect(mockApi.setWeeklyWarningMargin).toHaveBeenCalledWith(20));
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith(20));
  });

  it("0 turns early warnings off and says park explanations stay", async () => {
    mount(15);
    fireEvent.change(input(), { target: { value: "0" } });
    fireEvent.keyDown(input(), { key: "Enter" });
    await waitFor(() => expect(mockApi.setWeeklyWarningMargin).toHaveBeenCalledWith(0));
    expect(effect()).toMatch(/^Early warnings are off: no banner and no Overview item before a limit\./);
    expect(effect()).toMatch(/A run parked on its weekly limit still says why/);
  });

  it("refuses values outside 0..50 and non-integers without a request", () => {
    mount(15);
    for (const bad of ["51", "-1", "1.5", "", "1e1", "abc"]) {
      fireEvent.change(input(), { target: { value: bad } });
      fireEvent.blur(input());
      expect(screen.getByRole("alert").textContent, bad).toBe("Enter a whole number from 0 to 50.");
      expect(input().getAttribute("aria-invalid"), bad).toBe("true");
    }
    expect(mockApi.setWeeklyWarningMargin).not.toHaveBeenCalled();
    fireEvent.change(input(), { target: { value: "50" } });
    fireEvent.blur(input());
    expect(screen.queryByRole("alert")).toBeNull();
    expect(mockApi.setWeeklyWarningMargin).toHaveBeenCalledWith(50);
  });

  it("an unchanged value sends nothing, and Escape restores the stored value", () => {
    mount(15);
    fireEvent.blur(input());
    expect(mockApi.setWeeklyWarningMargin).not.toHaveBeenCalled();
    fireEvent.change(input(), { target: { value: "30" } });
    fireEvent.keyDown(input(), { key: "Escape" });
    expect(input().value).toBe("15");
  });

  it("a failed save rolls back to the stored value and names the failure", async () => {
    mockApi.setWeeklyWarningMargin.mockRejectedValue(new ApiError(400, "margin_pts must be an integer from 0 to 50"));
    const onSaved = mount(15);
    fireEvent.change(input(), { target: { value: "25" } });
    fireEvent.blur(input());
    expect(await screen.findByText("margin_pts must be an integer from 0 to 50")).toBeTruthy();
    expect(input().value).toBe("15");
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("a value committed while a save is in flight is sent right after it, and the last wins", async () => {
    let release: () => void = () => {};
    mockApi.setWeeklyWarningMargin.mockImplementationOnce(
      (n: number) => new Promise((resolve) => (release = () => resolve({ weekly_warning_margin_pts: n }))),
    );
    const onSaved = mount(15);
    fireEvent.change(input(), { target: { value: "20" } });
    fireEvent.blur(input());
    fireEvent.change(input(), { target: { value: "30" } });
    fireEvent.blur(input());
    expect(mockApi.setWeeklyWarningMargin).toHaveBeenCalledTimes(1);
    release();
    await waitFor(() => expect(mockApi.setWeeklyWarningMargin).toHaveBeenLastCalledWith(30));
    await waitFor(() => expect(onSaved).toHaveBeenLastCalledWith(30));
    expect(input().value).toBe("30");
  });
});
