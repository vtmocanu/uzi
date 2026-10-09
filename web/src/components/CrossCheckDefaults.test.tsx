// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { CrossCheckDefaults } from "./CrossCheckDefaults";
import { ApiError } from "../lib/apiError";
import { api } from "../lib/api";
import type { CrossCheckPinDTO, UserSettings } from "../lib/apiTypes";

vi.mock("../lib/api", () => ({ api: {
  putMySettings: vi.fn(), listAgentTemplates: vi.fn(), getTemplateAllocations: vi.fn(),
} }));
const mocked = vi.mocked(api);
const pin = (harness: "claude" | "codex", over: Partial<CrossCheckPinDTO> = {}): CrossCheckPinDTO => ({
  worker_default_model: harness === "claude" ? "custom-template-model" : "gpt-6.1-sol",
  stage: "plan", harness, model: null, effort: null, resolved_model: harness === "codex" ? "gpt-6.1-sol" : "custom-template-model",
  resolved_effort: "medium", model_source: "worker default", effort_source: "worker default", active: harness === "codex", ...over,
});
const settings = (over: Partial<UserSettings> = {}): UserSettings => ({
  default_model: null, default_harness: null, default_claude_model: null, default_codex_model: null,
  default_effort: null, default_codex_effort: null, judge_model: null, summary_model: null, theme: null,
  appearance_mode: null, light_theme: null, dark_theme: null, typeface: null,
  cross_check_pins: [pin("claude"), pin("codex")], ...over,
});
const model = (family: string) => screen.getByLabelText(`${family} checker model`) as HTMLSelectElement;
const effort = (family: string) => screen.getByLabelText(`${family} checker effort`) as HTMLSelectElement;
const save = () => screen.getByRole("button", { name: "Save cross-check defaults" }) as HTMLButtonElement;
beforeEach(() => vi.resetAllMocks());
afterEach(cleanup);

describe("checker defaults", () => {
  it("has one accessible Plan row, exact headings, independent Defaults and editable Claude", () => {
    render(<CrossCheckDefaults settings={settings()} onSaved={vi.fn()} />);
    const table = screen.getByRole("table", { name: "Cross-check defaults" });
    expect(within(table).getAllByRole("row")).toHaveLength(2);
    expect(within(table).getByRole("columnheader", { name: "Claude cross-checker" })).toBeTruthy();
    expect(within(table).getByRole("columnheader", { name: "Codex cross-checker" })).toBeTruthy();
    expect(screen.getByText("Checks leads running on the other model family")).toBeTruthy();
    expect(screen.queryByText(/Used once Codex-lead runs are cross-checked/)).toBeNull();
    expect(within(model("Claude")).getByRole("option", { name: "Default · custom-template-model (worker default)" })).toBeTruthy();
    expect(within(model("Codex")).getByRole("option", { name: "Default · gpt-6.1-sol (worker default)" })).toBeTruthy();
    for (const family of ["Claude", "Codex"]) {
      expect(effort(family).disabled).toBe(false);
      for (const value of ["low", "medium", "high", "xhigh", "max"]) expect(within(effort(family)).getByRole("option", { name: new RegExp(`^${value}`) })).toBeTruthy();
    }
    expect(mocked.listAgentTemplates).not.toHaveBeenCalled();
  });

  it("saves both changed cells atomically, retains failed drafts, and commits response normalization", async () => {
    const onSaved = vi.fn();
    render(<CrossCheckDefaults settings={settings()} onSaved={onSaved} />);
    fireEvent.change(model("Claude"), { target: { value: "sonnet" } });
    fireEvent.change(model("Codex"), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("Custom codex checker model ID"), { target: { value: "  arbitrary-codex-id  " } });
    fireEvent.change(effort("Codex"), { target: { value: "max" } });
    mocked.putMySettings.mockRejectedValueOnce(new ApiError(500, "write failed"));
    fireEvent.click(save());
    await screen.findByText("write failed");
    expect(model("Claude").value).toBe("sonnet");
    expect((screen.getByLabelText("Custom codex checker model ID") as HTMLInputElement).value).toBe("  arbitrary-codex-id  ");
    expect(mocked.putMySettings).toHaveBeenCalledTimes(1);
    expect(mocked.putMySettings).toHaveBeenCalledWith({ cross_check_pins: [
      { stage: "plan", harness: "claude", model: "sonnet" },
      { stage: "plan", harness: "codex", model: "arbitrary-codex-id", effort: "max" },
    ] });
    const response = settings({ cross_check_pins: [pin("claude", { model: "sonnet" }), pin("codex", { model: "arbitrary-codex-id", effort: "max" })] });
    mocked.putMySettings.mockResolvedValueOnce({ settings: response });
    fireEvent.click(save());
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith());
    expect(save().disabled).toBe(true);
    await waitFor(() => expect((screen.getByLabelText("Custom codex checker model ID") as HTMLInputElement).value).toBe("arbitrary-codex-id"));
  });

  it("refreshes committed worker defaults without pinning resolved values or overwriting drafts, then resets only model", async () => {
    const initial = settings({ default_claude_model: "opus", cross_check_pins: [pin("claude", { model: "sonnet", effort: "high" }), pin("codex")] });
    const r = render(<CrossCheckDefaults settings={initial} onSaved={vi.fn()} />);
    fireEvent.change(model("Codex"), { target: { value: "gpt-6-sol" } });
    r.rerender(<CrossCheckDefaults settings={{ ...initial, default_claude_model: "haiku", default_effort: "max", default_codex_model: "custom-worker", cross_check_pins: [pin("claude", { worker_default_model: "haiku" }), pin("codex", { worker_default_model: "custom-worker" })] }} onSaved={vi.fn()} />);
    expect(within(model("Claude")).getByRole("option", { name: "Default · haiku (worker default)" })).toBeTruthy();
    expect(within(effort("Claude")).getByRole("option", { name: "Default · max (worker default)" })).toBeTruthy();
    expect(model("Codex").value).toBe("gpt-6-sol");
    fireEvent.change(model("Codex"), { target: { value: "inherit" } });
    fireEvent.change(model("Claude"), { target: { value: "inherit" } });
    mocked.putMySettings.mockResolvedValueOnce({ settings: settings() });
    fireEvent.click(save());
    await waitFor(() => expect(mocked.putMySettings).toHaveBeenCalledWith({ cross_check_pins: [{ stage: "plan", harness: "claude", model: null }] }));
  });

  it("resets only effort to Default while preserving the model pin", async () => {
    const initial = settings({ default_claude_model: "opus", cross_check_pins: [
      pin("claude"), pin("codex", { model: "gpt-6-sol", effort: "max" }),
    ] });
    render(<CrossCheckDefaults settings={initial} onSaved={vi.fn()} />);
    fireEvent.change(effort("Codex"), { target: { value: "" } });
    mocked.putMySettings.mockResolvedValueOnce({ settings: settings({ cross_check_pins: [
      pin("claude"), pin("codex", { model: "gpt-6-sol" }),
    ] }) });
    fireEvent.click(save());
    await waitFor(() => expect(mocked.putMySettings).toHaveBeenCalledWith({
      cross_check_pins: [{ stage: "plan", harness: "codex", effort: null }],
    }));
    expect(model("Codex").value).toBe("gpt-6-sol");
    expect(effort("Codex").value).toBe("");
    expect(save().disabled).toBe(true);
  });

  it("B1 worker reset ignores the initial explicit-model hint and retains normalized saved pins", async () => {
    const initial = settings({ default_claude_model: "opus", cross_check_pins: [pin("claude", { resolved_model: "opus" }), pin("codex")] });
    const onSaved = vi.fn();
    const r = render(<CrossCheckDefaults settings={initial} onSaved={onSaved} />);
    let reply!: (v: { settings: UserSettings }) => void;
    mocked.putMySettings.mockImplementationOnce(() => new Promise(resolve => { reply = resolve; }));
    fireEvent.change(model("Claude"), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("Custom claude checker model ID"), { target: { value: "  normalized-pin  " } });
    fireEvent.click(save());
    expect(mocked.putMySettings).toHaveBeenCalledWith({ cross_check_pins: [{ stage: "plan", harness: "claude", model: "normalized-pin" }] });
    const reset = { ...initial, default_claude_model: null, cross_check_pins: [pin("claude", { worker_default_model: "template-inherited" }), pin("codex")] };
    r.rerender(<CrossCheckDefaults settings={reset} onSaved={onSaved} />);
    await waitFor(() => expect(within(model("Claude")).getByRole("option", { name: "Default · template-inherited (worker default)" })).toBeTruthy());
    const response = settings({ default_claude_model: "opus", cross_check_pins: [pin("claude", { model: "normalized-pin", resolved_model: "normalized-pin" }), pin("codex")] });
    reply({ settings: response });
    await waitFor(() => expect(save().disabled).toBe(true));
    expect(onSaved).toHaveBeenCalledWith();
    // ModelSelect synchronizes external normalized values in its own effect.
    await waitFor(() => expect((screen.getByLabelText("Custom claude checker model ID") as HTMLInputElement).value).toBe("normalized-pin"));
    expect(within(model("Claude")).getByRole("option", { name: "Default · template-inherited (worker default)" })).toBeTruthy();
    fireEvent.change(effort("Claude"), { target: { value: "high" } });
    mocked.putMySettings.mockResolvedValueOnce({ settings: settings({ cross_check_pins: [pin("claude", { model: "normalized-pin", effort: "high" }), pin("codex")] }) });
    fireEvent.click(save());
    await waitFor(() => expect(save().disabled).toBe(true));
    expect(mocked.putMySettings).toHaveBeenLastCalledWith({ cross_check_pins: [{ stage: "plan", harness: "claude", effort: "high" }] });
  });

  it.each(["opus", "two words", "x".repeat(101), "bad\u202e", "\uFFFD", "é".repeat(51), "bad\uFEFF", "a\u0085b", "a\u0001b"])("blocks invalid Codex model %s", async value => {
    render(<CrossCheckDefaults settings={settings()} onSaved={vi.fn()} />);
    fireEvent.change(model("Codex"), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("Custom codex checker model ID"), { target: { value } });
    expect(save().disabled).toBe(true);
    expect(mocked.putMySettings).not.toHaveBeenCalled();
  });

  it.each(["allocated-custom", "custom-orchestrator", null])("uses counterfactual metadata with initial pins: %s", value => {
    render(<CrossCheckDefaults settings={settings({ cross_check_pins: [
      pin("claude", { model: "sonnet", resolved_model: "sonnet", worker_default_model: value }), pin("codex"),
    ] })} />);
    expect(within(model("Claude")).getByRole("option", { name: `Default · ${value ?? "SDK/account default"} (worker default)` })).toBeTruthy();
    expect(model("Claude").value).toBe("sonnet");
    expect(mocked.listAgentTemplates).not.toHaveBeenCalled();
    expect(mocked.getTemplateAllocations).not.toHaveBeenCalled();
  });

  it("shows unavailable for missing metadata and allows saving without catalog reads", () => {
    const legacy = pin("claude");
    delete (legacy as Partial<CrossCheckPinDTO>).worker_default_model;
    render(<CrossCheckDefaults settings={settings({ cross_check_pins: [legacy] })} />);
    for (const family of ["Claude", "Codex"]) {
      expect(within(model(family)).getByRole("option", { name: "Default · Unavailable (worker default)" })).toBeTruthy();
    }
    expect(screen.queryByText(/SDK\/account default/)).toBeNull();
    fireEvent.change(effort("Codex"), { target: { value: "high" } });
    expect(save().disabled).toBe(false);
    expect(mocked.listAgentTemplates).not.toHaveBeenCalled();
    expect(mocked.getTemplateAllocations).not.toHaveBeenCalled();
  });
});
