// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { InstanceModelDefaultsCard } from "./InstanceModelDefaultsCard";
import { CODEX_CURATED_MODELS, CODEX_ROLE_MODELS } from "../../lib/codexModels";
import {
  api,
  type AppSettings,
  type SettingSource,
  type SettingsResponse,
  type UpdateSettingsPayload,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

const DEFAULTS = {
  judge_model: "opus",
  codex_judge_model: "gpt-6.1-sol",
  summary_model: "haiku",
  codex_summary_model: "gpt-6-luna",
};
const MODEL_KEYS = Object.keys(DEFAULTS);
const BUILTIN: Record<string, string> = { ...DEFAULTS };

type Pins = Partial<Record<keyof typeof DEFAULTS, string>>;
type View = { settings: AppSettings; sources: Record<string, SettingSource> };

// A server-shaped view: a key with no pin reports the built-in as the effective value with
// source "default"; a pin reports source "db" (or "env" when listed in `env`).
const view = (pins: Pins = {}, env: string[] = []): View => {
  const settings: Record<string, string> = {};
  const sources: Record<string, SettingSource> = {};
  for (const k of MODEL_KEYS) {
    const pin = pins[k as keyof typeof DEFAULTS] ?? "";
    settings[k] = pin === "" ? BUILTIN[k]! : pin;
    sources[k] = env.includes(k) ? "env" : pin === "" ? "default" : "db";
  }
  return { settings: settings as unknown as AppSettings, sources };
};
const response = (v: View): SettingsResponse =>
  ({ ...v, secrets: {}, defaults: DEFAULTS, slack_status: "disabled" }) as SettingsResponse;

function Harness({ pins, env, onSaved }: { pins?: Pins; env?: string[]; onSaved?: (r: SettingsResponse, sent: UpdateSettingsPayload) => void }) {
  const [v, setV] = useState(view(pins, env));
  return (
    <InstanceModelDefaultsCard
      settings={v.settings}
      sources={v.sources}
      defaults={DEFAULTS}
      onSaved={(r, sent) => {
        onSaved?.(r, sent);
        setV({ settings: r.settings, sources: r.sources });
      }}
    />
  );
}

const pick = (name: string) => screen.getByRole("combobox", { name }) as HTMLSelectElement;
const judgeClaude = () => pick("Judge model, Claude");
const judgeCodex = () => pick("Judge model, Codex");
const summaryClaude = () => pick("Summary model, Claude");
const summaryCodex = () => pick("Summary model, Codex");
const save = () => screen.getByRole("button", { name: "Save model defaults" }) as HTMLButtonElement;
const choose = (el: HTMLSelectElement, v: string) => fireEvent.change(el, { target: { value: v } });
const optionValues = (s: HTMLSelectElement) => Array.from(s.options).map((o) => o.value);
const optionText = (s: HTMLSelectElement) => Array.from(s.options).map((o) => o.text);

afterEach(() => {
  // Every payload ever sent carries only the four model keys.
  for (const call of mockApi.updateSettings.mock.calls) {
    for (const k of Object.keys(call[0] ?? {})) expect(MODEL_KEYS).toContain(k);
  }
  cleanup();
  vi.resetAllMocks();
});

describe("InstanceModelDefaultsCard grid (#3259)", () => {
  it("renders four selects by accessible name with a Default naming the built-in, selected when unpinned", () => {
    render(<Harness />);
    expect(judgeClaude().value).toBe("inherit");
    expect(judgeCodex().value).toBe("inherit");
    expect(summaryClaude().value).toBe("inherit");
    expect(summaryCodex().value).toBe("inherit");
    expect(optionText(judgeClaude())).toContain("Default · opus (built-in)");
    expect(optionText(judgeCodex())).toContain("Default · gpt-6.1-sol (built-in)");
    expect(optionText(summaryClaude())).toContain("Default · haiku (built-in)");
    expect(optionText(summaryCodex())).toContain("Default · gpt-6-luna (built-in)");
    expect(save().disabled).toBe(true);
    expect(screen.getAllByText("Follows the built-in model.")).toHaveLength(4);
  });

  it("selects a pinned value and explains the pin against the built-in", () => {
    render(<Harness pins={{ judge_model: "sonnet", codex_summary_model: "gpt-6-astra" }} />);
    expect(judgeClaude().value).toBe("sonnet");
    expect(summaryCodex().value).toBe("gpt-6-astra");
    expect(judgeCodex().value).toBe("inherit");
    expect(
      screen.getByText("Pinned · built-in is opus; choose Default to follow it again."),
    ).toBeTruthy();
  });

  it("names no model in the row rationale copy", () => {
    render(<Harness />);
    expect(screen.getByText("Judge advice feeds self-improvement, so its built-in is a strong model.")).toBeTruthy();
    expect(screen.getByText("Two short summaries on every run, so its built-in is fast and near-free.")).toBeTruthy();
  });

  it.each([
    ["Judge model, Claude", "judge_model", "sonnet"],
    ["Judge model, Codex", "codex_judge_model", "gpt-6-astra"],
    ["Summary model, Claude", "summary_model", "opus"],
    ["Summary model, Codex", "codex_summary_model", "gpt-6-luna"],
  ])("pinning %s sends exactly that one key in one call", async (name, key, model) => {
    mockApi.updateSettings.mockResolvedValue(response(view({ [key]: model })));
    render(<Harness />);
    choose(pick(name), model);
    fireEvent.click(save());
    await screen.findByText("Model defaults saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledTimes(1);
    expect(mockApi.updateSettings.mock.calls[0]?.[0]).toEqual({ [key]: model });
    expect(pick(name).value).toBe(model);
  });

  it("sends an empty string when a pinned lane is reset to Default", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view({ judge_model: "sonnet" })));
    render(<Harness pins={{ judge_model: "sonnet", codex_judge_model: "gpt-6-sol" }} />);
    choose(judgeCodex(), "inherit");
    fireEvent.click(save());
    await screen.findByText("Model defaults saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledTimes(1);
    expect(mockApi.updateSettings.mock.calls[0]?.[0]).toEqual({ codex_judge_model: "" });
    expect(judgeCodex().value).toBe("inherit");
    expect(judgeClaude().value).toBe("sonnet");
  });

  it("carries two changed keys in one call and reports success once", async () => {
    const onSaved = vi.fn();
    const next = response(view({ summary_model: "sonnet", codex_judge_model: "gpt-6-sol" }));
    mockApi.updateSettings.mockResolvedValue(next);
    render(<Harness onSaved={onSaved} />);
    choose(summaryClaude(), "sonnet");
    choose(judgeCodex(), "gpt-6-sol");
    fireEvent.click(save());
    await screen.findByText("Model defaults saved.");
    expect(mockApi.updateSettings).toHaveBeenCalledTimes(1);
    expect(mockApi.updateSettings.mock.calls[0]?.[0]).toEqual({
      summary_model: "sonnet",
      codex_judge_model: "gpt-6-sol",
    });
    expect(onSaved).toHaveBeenCalledWith(next, { summary_model: "sonnet", codex_judge_model: "gpt-6-sol" });
    expect(summaryClaude().value).toBe("sonnet");
    expect(judgeCodex().value).toBe("gpt-6-sol");
    expect(save().disabled).toBe(true);
  });

  it("locks the fields while a save is in flight and unlocks them when it settles", async () => {
    let resolve!: (r: SettingsResponse) => void;
    mockApi.updateSettings.mockReturnValue(new Promise<SettingsResponse>((r) => (resolve = r)));
    render(<Harness />);
    choose(judgeClaude(), "sonnet");
    fireEvent.click(save());
    // A disabled <fieldset> disables its controls (the :disabled state), not their own attribute.
    await waitFor(() => expect(judgeClaude().matches(":disabled")).toBe(true));
    expect(summaryCodex().matches(":disabled")).toBe(true);
    resolve(response(view({ judge_model: "sonnet" })));
    await screen.findByText("Model defaults saved.");
    expect(judgeClaude().matches(":disabled")).toBe(false);
    expect(summaryCodex().matches(":disabled")).toBe(false);
  });

  it("renders an env-sourced key read-only and never sends it", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view({ judge_model: "sonnet" }, ["summary_model"])));
    render(<Harness env={["summary_model"]} pins={{ summary_model: "haiku" }} />);
    expect(screen.queryByRole("combobox", { name: "Summary model, Claude" })).toBeNull();
    const ro = screen.getByRole("textbox", { name: "Summary model, Claude" }) as HTMLInputElement;
    expect(ro.readOnly).toBe(true);
    expect(ro.value).toBe("haiku");
    expect(screen.getByText("Set from the environment")).toBeTruthy();
    choose(judgeClaude(), "sonnet");
    fireEvent.click(save());
    await screen.findByText("Model defaults saved.");
    expect(mockApi.updateSettings.mock.calls[0]?.[0]).toEqual({ judge_model: "sonnet" });
  });

  it("shows a 400 field error under that field, keeps both edits, and clears it on edit", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "codex_judge_model: is not a Codex judge model"));
    render(<Harness />);
    choose(summaryClaude(), "sonnet");
    choose(judgeCodex(), "gpt-6-sol");
    fireEvent.click(save());
    const alert = await screen.findByText("is not a Codex judge model");
    expect(alert.getAttribute("role")).toBe("alert");
    expect(judgeCodex().getAttribute("aria-invalid")).toBe("true");
    expect(judgeCodex().getAttribute("aria-describedby")).toContain(alert.id);
    expect(summaryClaude().getAttribute("aria-invalid")).toBeNull();
    expect(summaryClaude().value).toBe("sonnet");
    expect(judgeCodex().value).toBe("gpt-6-sol");
    expect(save().disabled).toBe(false);
    expect(screen.queryByText("Model defaults saved.")).toBeNull();

    choose(judgeCodex(), "gpt-6-astra");
    expect(screen.queryByText("is not a Codex judge model")).toBeNull();
    expect(judgeCodex().getAttribute("aria-invalid")).toBeNull();
  });

  it("shows a 409 environment conflict at card level and keeps the edit", async () => {
    mockApi.updateSettings.mockRejectedValue(
      new ApiError(409, "summary_model is set from the environment and cannot be changed here"),
    );
    render(<Harness />);
    choose(summaryClaude(), "sonnet");
    fireEvent.click(save());
    const alert = await screen.findByText("summary_model is set from the environment and cannot be changed here");
    expect(alert.getAttribute("role")).toBe("alert");
    expect(summaryClaude().value).toBe("sonnet");
    expect(screen.queryByText("Model defaults saved.")).toBeNull();
  });

  it("treats a 500 as an ambiguous outcome: warns it may have applied and keeps the edit", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(500, "internal error"));
    render(<Harness />);
    choose(judgeClaude(), "sonnet");
    fireEvent.click(save());
    const alert = await screen.findByText(/may or may not have been applied/);
    expect(alert.textContent).toContain("internal error");
    expect(alert.textContent).toContain("Reload the page to see the current values.");
    expect(judgeClaude().value).toBe("sonnet");
    // The baseline did not advance: the edit is still pending, so Save stays enabled.
    expect(save().disabled).toBe(false);
    expect(screen.queryByText("Model defaults saved.")).toBeNull();
  });

  it("shows a 400 without a field prefix at card level, without the ambiguous-outcome copy", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "no settings provided"));
    render(<Harness />);
    choose(judgeClaude(), "sonnet");
    fireEvent.click(save());
    const alert = await screen.findByText("no settings provided");
    expect(alert.getAttribute("role")).toBe("alert");
    expect(screen.getByText(/no settings provided/).textContent).not.toContain("may or may not");
    expect(screen.getAllByRole("combobox").some((el) => el.getAttribute("aria-invalid") === "true")).toBe(false);
    expect(save().disabled).toBe(false);
  });

  it("treats a network failure like a 500", async () => {
    mockApi.updateSettings.mockRejectedValue(new TypeError("Failed to fetch"));
    render(<Harness />);
    choose(judgeClaude(), "sonnet");
    fireEvent.click(save());
    const alert = await screen.findByText(/may or may not have been applied/);
    expect(alert.textContent).toContain("Failed to save model defaults");
    expect(judgeClaude().value).toBe("sonnet");
    expect(save().disabled).toBe(false);
  });
});

describe("InstanceModelDefaultsCard lane validation (ported from the judge and summary cards)", () => {
  it("offers exactly the Codex role models in the Codex summary lane, with no custom entry", () => {
    render(<Harness />);
    expect(optionValues(summaryCodex()).filter((v) => v !== "inherit")).toEqual([...CODEX_ROLE_MODELS]);
  });

  it("offers exactly the curated Codex models in the Codex judge lane, with no custom entry", () => {
    render(<Harness />);
    expect(optionValues(judgeCodex()).filter((v) => v !== "inherit")).toEqual([...CODEX_CURATED_MODELS]);
  });

  it("selects a stored gpt-6-luna in the Codex summary lane as itself, not as custom", () => {
    render(<Harness pins={{ codex_summary_model: "gpt-6-luna" }} />);
    expect(summaryCodex().value).toBe("gpt-6-luna");
  });

  it("warns and blocks Save for an invalid custom Claude ID", () => {
    render(<Harness />);
    choose(judgeClaude(), "custom");
    fireEvent.change(screen.getByRole("textbox", { name: "Custom judge model ID, Claude" }), {
      target: { value: "bad id" },
    });
    expect(screen.getByText("Model must be a single token with no spaces.")).toBeTruthy();
    expect(save().disabled).toBe(true);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("blocks Save when custom mode is empty rather than sending the reset", () => {
    render(<Harness pins={{ summary_model: "sonnet" }} />);
    choose(summaryClaude(), "custom");
    expect(screen.getByText("Enter a model ID, or choose Default.")).toBeTruthy();
    expect(save().disabled).toBe(true);
  });

  it("refuses a Codex id typed into a Claude lane with the alert and blocks Save", () => {
    render(<Harness />);
    choose(summaryClaude(), "custom");
    fireEvent.change(screen.getByRole("textbox", { name: "Custom summary model ID, Claude" }), {
      target: { value: "gpt-6-astra" },
    });
    const alert = screen.getByText("That is a Codex model. Choose it under the Codex column instead.");
    expect(alert.getAttribute("role")).toBe("alert");
    expect(save().disabled).toBe(true);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("warns on a stored Codex id in a Claude lane without locking an unrelated save", async () => {
    mockApi.updateSettings.mockResolvedValue(
      response(view({ judge_model: "gpt-6-astra", codex_summary_model: "gpt-6-astra" })),
    );
    render(<Harness pins={{ judge_model: "gpt-6-astra" }} />);
    const warn = screen.getByText(/Not a Claude model/);
    expect(warn.getAttribute("role")).toBe("status");
    expect(warn.textContent).toContain("Claude judges use the built-in opus");
    choose(summaryCodex(), "gpt-6-astra");
    expect(save().disabled).toBe(false);
    fireEvent.click(save());
    await screen.findByText("Model defaults saved.");
    expect(mockApi.updateSettings.mock.calls[0]?.[0]).toEqual({ codex_summary_model: "gpt-6-astra" });
  });

  it("warns for a stored invalid Codex judge ID without locking an unrelated save", () => {
    mockApi.updateSettings.mockResolvedValue(response(view({ summary_model: "sonnet" })));
    render(<Harness pins={{ codex_judge_model: "bad id" }} />);
    expect(screen.getByText("Model must be a single token with no spaces.")).toBeTruthy();
    choose(summaryClaude(), "sonnet");
    expect(save().disabled).toBe(false);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).toHaveBeenCalledTimes(1);
    const payload = mockApi.updateSettings.mock.calls[0]?.[0] ?? {};
    expect(payload).toEqual({ summary_model: "sonnet" });
  });

  it("flags a stored non-curated Codex judge pin without locking an unrelated save, and blocks saving another", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view({ codex_judge_model: "gpt-6-luna", judge_model: "sonnet" })));
    render(<Harness pins={{ codex_judge_model: "gpt-6-luna" }} />);
    // A syntactically valid id outside the curated list: shown as itself, with the membership warning.
    expect(screen.getByDisplayValue("gpt-6-luna")).toBeTruthy();
    expect(screen.getByText(/Not a listed Codex judge model/)).toBeTruthy();
    choose(judgeClaude(), "sonnet");
    expect(save().disabled).toBe(false);
    fireEvent.click(save());
    await screen.findByText("Model defaults saved.");
    expect(mockApi.updateSettings.mock.calls[0]?.[0]).toEqual({ judge_model: "sonnet" });
  });

  it("refuses to save an edited non-curated Codex judge id", () => {
    render(<Harness pins={{ codex_judge_model: "gpt-6-luna" }} />);
    fireEvent.change(screen.getByDisplayValue("gpt-6-luna"), { target: { value: "gpt-7-nova" } });
    expect(screen.getByText(/Not a listed Codex judge model/)).toBeTruthy();
    expect(save().disabled).toBe(true);
    fireEvent.submit(save().closest("form")!);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });

  it("does not resend an untouched stored custom value when another lane changes", async () => {
    mockApi.updateSettings.mockResolvedValue(response(view({ summary_model: "claude-x-1", judge_model: "sonnet" })));
    render(<Harness pins={{ summary_model: "claude-x-1" }} />);
    expect(summaryClaude().value).toBe("custom");
    choose(judgeClaude(), "sonnet");
    fireEvent.click(save());
    await screen.findByText("Model defaults saved.");
    expect(mockApi.updateSettings.mock.calls[0]?.[0]).toEqual({ judge_model: "sonnet" });
  });
});
