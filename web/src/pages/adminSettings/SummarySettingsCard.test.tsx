// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { SummarySettingsCard } from "./SummarySettingsCard";
import { CODEX_ROLE_MODELS } from "../../lib/codexModels";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const DEFAULTS = { summary_model: "haiku", codex_summary_model: "gpt-6-luna" };

// A server-shaped view: a source of "default" reports the built-in as the effective value.
const view = (
  claude: string,
  codex: string,
  now = "true",
): { settings: AppSettings; sources: Record<string, SettingSource> } => ({
  settings: {
    summary_model: claude === "" ? "haiku" : claude,
    codex_summary_model: codex === "" ? "gpt-6-luna" : codex,
    now_summary_enabled: now,
  } as AppSettings,
  sources: {
    summary_model: claude === "" ? "default" : "db",
    codex_summary_model: codex === "" ? "default" : "db",
  },
});
const response = (v: ReturnType<typeof view>): SettingsResponse =>
  ({ ...v, secrets: {}, defaults: DEFAULTS, slack_status: "disabled" }) as SettingsResponse;

function Harness({ claude, codex }: { claude: string; codex: string }) {
  const [v, setV] = useState(view(claude, codex));
  return (
    <SummarySettingsCard
      settings={v.settings}
      sources={v.sources}
      defaults={DEFAULTS}
      onSaved={(r) => setV({ settings: r.settings, sources: r.sources })}
    />
  );
}

const claudePicker = () => screen.getByRole("combobox", { name: "Claude summary model" }) as HTMLSelectElement;
const codexPicker = () => screen.getByRole("combobox", { name: "Codex summary model" }) as HTMLSelectElement;
const save = () => screen.getByRole("button", { name: "Save run summary settings" }) as HTMLButtonElement;
const optionText = (s: HTMLSelectElement) => Array.from(s.options).map((o) => o.text);

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("SummarySettingsCard Claude | Codex pair (#3248)", () => {
  it("renders both pickers with a Default option naming the built-in, selected when unpinned", () => {
    render(<Harness claude="" codex="" />);
    expect(claudePicker().value).toBe("inherit");
    expect(codexPicker().value).toBe("inherit");
    expect(optionText(claudePicker())).toContain("Default · haiku (built-in)");
    expect(optionText(codexPicker())).toContain("Default · gpt-6-luna (built-in)");
    expect(save().disabled).toBe(true);
  });

  it("selects the pinned model, not Default, when the source is not default", () => {
    render(<Harness claude="sonnet" codex="gpt-6-astra" />);
    expect(claudePicker().value).toBe("sonnet");
    expect(codexPicker().value).toBe("gpt-6-astra");
  });

  it("offers exactly the Codex role models in the Codex picker, with no custom entry", () => {
    render(<Harness claude="" codex="" />);
    const values = Array.from(codexPicker().options).map((o) => o.value).filter((v) => v !== "inherit");
    expect(values).toEqual([...CODEX_ROLE_MODELS]);
  });

  it("selects a stored gpt-6-luna as itself, not as a custom ID", () => {
    render(<Harness claude="" codex="gpt-6-luna" />);
    expect(codexPicker().value).toBe("gpt-6-luna");
    expect(screen.queryByRole("textbox", { name: "Custom Codex summary model ID" })).toBeNull();
  });

  it("sends only the changed Codex key", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view("", "gpt-6-astra")));
    render(<Harness claude="" codex="" />);
    fireEvent.change(codexPicker(), { target: { value: "gpt-6-astra" } });
    fireEvent.click(save());
    await screen.findByText("Run summary settings saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ codex_summary_model: "gpt-6-astra" });
    expect(codexPicker().value).toBe("gpt-6-astra");
  });

  it("sends only the changed Claude key", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view("sonnet", "")));
    render(<Harness claude="" codex="" />);
    fireEvent.change(claudePicker(), { target: { value: "sonnet" } });
    fireEvent.click(save());
    await screen.findByText("Run summary settings saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ summary_model: "sonnet" });
    expect(claudePicker().value).toBe("sonnet");
  });

  it("sends an empty string when a pinned lane is reset to Default", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view("sonnet", "")));
    render(<Harness claude="sonnet" codex="gpt-6-astra" />);
    fireEvent.change(codexPicker(), { target: { value: "inherit" } });
    fireEvent.click(save());
    await screen.findByText("Run summary settings saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ codex_summary_model: "" });
    expect(codexPicker().value).toBe("inherit");
    expect(claudePicker().value).toBe("sonnet");
  });

  it("refuses a Codex role id in the Claude lane and blocks Save", () => {
    render(<Harness claude="" codex="" />);
    fireEvent.change(claudePicker(), { target: { value: "custom" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Custom Claude summary model ID" }), {
      target: { value: "gpt-6-astra" },
    });
    expect(screen.getByText(/That is a Codex model/)).toBeTruthy();
    expect(save().disabled).toBe(true);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("warns on a stored Codex id in the Claude lane without locking an unrelated save", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view("gpt-6-astra", "gpt-6-astra")));
    render(<Harness claude="gpt-6-astra" codex="" />);
    expect(screen.getByText(/Not a Claude model/)).toBeTruthy();
    fireEvent.change(codexPicker(), { target: { value: "gpt-6-astra" } });
    expect(save().disabled).toBe(false);
    fireEvent.click(save());
    await screen.findByText("Run summary settings saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ codex_summary_model: "gpt-6-astra" });
  });

  it("warns beneath the picker on an invalid custom ID and blocks Save", () => {
    render(<Harness claude="" codex="" />);
    fireEvent.change(claudePicker(), { target: { value: "custom" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Custom Claude summary model ID" }), {
      target: { value: "bad id" },
    });
    expect(screen.getByText("Model must be a single token with no spaces.")).toBeTruthy();
    expect(save().disabled).toBe(true);
  });

  it("blocks Save when custom mode is empty rather than sending the reset", () => {
    render(<Harness claude="sonnet" codex="" />);
    fireEvent.change(claudePicker(), { target: { value: "custom" } });
    expect(screen.getByText("Enter a model ID, or choose Default.")).toBeTruthy();
    expect(save().disabled).toBe(true);
  });

  it("does not resend an untouched stored custom value when only the Now line changes", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view("claude-x-1", "", "false")));
    render(<Harness claude="claude-x-1" codex="" />);
    expect(claudePicker().value).toBe("custom");
    fireEvent.click(screen.getByRole("checkbox", { name: "Model-written Now line" }));
    fireEvent.click(save());
    await screen.findByText("Run summary settings saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledWith({ now_summary_enabled: "false" });
  });

  it("describes the credential neutrally, not as an Anthropic token", () => {
    render(<Harness claude="" codex="" />);
    const text = document.body.textContent ?? "";
    expect(text).not.toContain("own Anthropic token");
    expect(text).toContain("credential for the run");
  });
});
