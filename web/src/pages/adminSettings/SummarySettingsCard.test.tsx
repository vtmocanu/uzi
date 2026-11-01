// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { SummarySettingsCard } from "./SummarySettingsCard";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const settings = (summary: string, now = "true"): AppSettings =>
  ({ summary_model: summary, now_summary_enabled: now }) as AppSettings;
const response = (s: AppSettings): SettingsResponse => ({ settings: s, secrets: {}, sources: {}, slack_status: "disabled" });

function Harness({ initial }: { initial: string }) {
  const [saved, setSaved] = useState(settings(initial));
  return <SummarySettingsCard settings={saved} onSaved={(r) => setSaved(r.settings)} />;
}

const picker = () => screen.getByRole("combobox", { name: "Summary model" }) as HTMLSelectElement;
const customInput = () => screen.getByRole("textbox", { name: "Custom summary model ID" }) as HTMLInputElement;
const save = () => screen.getByRole("button", { name: "Save run summary settings" }) as HTMLButtonElement;
const optionValues = () => Array.from(picker().options).map((o) => o.value);

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("SummarySettingsCard model picker (#3255)", () => {
  it("renders a curated stored alias selected, with no Default option", () => {
    render(<Harness initial="haiku" />);
    expect(picker().value).toBe("haiku");
    expect(optionValues()).not.toContain("inherit");
    expect(Array.from(picker().options).some((o) => /default/i.test(o.text))).toBe(false);
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("opens a stored custom value in custom mode and an unrelated save does not send it", async () => {
    mockApi.updateSettings.mockResolvedValue(response(settings("claude-x-1", "false")));
    render(<Harness initial="claude-x-1" />);
    expect(picker().value).toBe("custom");
    expect(customInput().value).toBe("claude-x-1");
    fireEvent.click(screen.getByRole("checkbox", { name: "Model-written Now line" }));
    fireEvent.click(save());
    await screen.findByText("Run summary settings saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ now_summary_enabled: "false" });
  });

  it("warns beneath the picker on an invalid custom ID and blocks Save", () => {
    render(<Harness initial="haiku" />);
    fireEvent.change(picker(), { target: { value: "custom" } });
    fireEvent.change(customInput(), { target: { value: "bad id" } });
    expect(screen.getByText("Model must be a single token with no spaces.")).toBeTruthy();
    expect(save().disabled).toBe(true);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("blocks Save and explains when custom mode is empty", () => {
    render(<Harness initial="haiku" />);
    fireEvent.change(picker(), { target: { value: "custom" } });
    expect(screen.getByRole("alert").textContent).toBe("Enter a model ID, or choose a listed model.");
    expect(save().disabled).toBe(true);
  });

  it("sends only summary_model when a curated alias is chosen", async () => {
    mockApi.updateSettings.mockResolvedValue(response(settings("sonnet")));
    render(<Harness initial="haiku" />);
    fireEvent.change(picker(), { target: { value: "sonnet" } });
    expect(save().disabled).toBe(false);
    fireEvent.click(save());
    await screen.findByText("Run summary settings saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ summary_model: "sonnet" });
    expect(picker().value).toBe("sonnet");
  });

  it("keeps the accessible names while in custom mode", () => {
    render(<Harness initial="claude-x-1" />);
    expect(picker()).toBeTruthy();
    expect(customInput()).toBeTruthy();
  });
});
