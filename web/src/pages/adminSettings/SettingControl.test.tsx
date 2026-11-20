// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SettingControl } from "./SettingControl";
import { api, ApiError, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const response = (over: Record<string, string>): SettingsResponse => ({
  settings: { ephemeral_max_per_user: "2", ...over } as AppSettings,
  secrets: {},
  sources: {},
  slack_status: "disabled",
});

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

function renderControl(source: SettingSource) {
  render(
    <SettingControl
      settingKey="ephemeral_max_per_user"
      label="Per-user cap"
      kind="number"
      value="2"
      source={source}
      defaultValue="2"
      helper="Maximum ephemeral workers per user."
      min={0}
      max={20}
      onSaved={() => {}}
    />,
  );
  return screen.getByLabelText("Per-user cap") as HTMLInputElement;
}

describe("SettingControl", () => {
  it("resyncs the draft to the response when a reset leaves the saved value unchanged", async () => {
    mockApi.updateSettings.mockResolvedValue(response({ ephemeral_max_per_user: "2" }));
    const input = renderControl("db");
    fireEvent.change(input, { target: { value: "5" } });
    expect(input.value).toBe("5");
    fireEvent.click(screen.getByRole("button", { name: "Reset to default: Per-user cap" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ ephemeral_max_per_user: "" }));
    expect(await screen.findByText("Per-user cap reset to default.")).toBeTruthy();
    expect(input.value).toBe("2");
  });

  it("passes the response and the sent payload to onSaved", async () => {
    const resp = response({ ephemeral_max_per_user: "5" });
    mockApi.updateSettings.mockResolvedValue(resp);
    const onSaved = vi.fn();
    render(
      <SettingControl
        settingKey="ephemeral_max_per_user"
        label="Per-user cap"
        kind="number"
        value="2"
        source="db"
        defaultValue="2"
        helper="Maximum ephemeral workers per user."
        min={0}
        max={20}
        onSaved={onSaved}
      />,
    );
    fireEvent.change(screen.getByLabelText("Per-user cap"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Per-user cap" }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(onSaved.mock.calls[0][0]).toBe(resp);
    expect(onSaved.mock.calls[0][1]).toBe(mockApi.updateSettings.mock.calls[0][0]);
    expect(onSaved.mock.calls[0][1]).toEqual({ ephemeral_max_per_user: "5" });
  });

  it("keeps the draft and shows the error when a reset fails", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(500, "storage unavailable"));
    const input = renderControl("db");
    fireEvent.change(input, { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Reset to default: Per-user cap" }));
    expect((await screen.findByRole("alert")).textContent).toContain("storage unavailable");
    expect(input.value).toBe("5");
  });

  it("keeps the draft and shows the error when a save fails", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "ephemeral_max_per_user: rejected"));
    const input = renderControl("db");
    fireEvent.change(input, { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Per-user cap" }));
    expect((await screen.findByRole("alert")).textContent).toContain("rejected");
    expect(input.value).toBe("5");
  });

  it("locks an environment-sourced key and links its explanation via aria-describedby", () => {
    const input = renderControl("env");
    expect(input.disabled).toBe(true);
    expect(screen.queryByRole("button", { name: /^Save / })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Reset to default/ })).toBeNull();
    const ids = (input.getAttribute("aria-describedby") ?? "").split(/\s+/);
    expect(ids).toContain("setting-ephemeral_max_per_user-env");
    const el = document.getElementById("setting-ephemeral_max_per_user-env");
    expect(el).not.toBeNull();
    expect(el?.textContent).toContain("environment variable");
  });
});
