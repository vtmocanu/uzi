// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { CrossCheckDefaults } from "./CrossCheckDefaults";
import { ApiError } from "../lib/apiError";
import { api } from "../lib/api";
import type { AgentTemplate, CrossCheckPinDTO, UserSettings } from "../lib/apiTypes";
import { mockTemplates } from "../mocks/data";

vi.mock("../lib/api", () => ({ api: {
  putMySettings: vi.fn(), listAgentTemplates: vi.fn(), getTemplateAllocations: vi.fn(),
} }));
const mocked = vi.mocked(api);
const pin = (harness: "claude" | "codex", over: Partial<CrossCheckPinDTO> = {}): CrossCheckPinDTO => ({
  stage: "plan", harness, model: null, effort: null, resolved_model: harness === "codex" ? "gpt-6.1-sol" : "custom-template-model",
  resolved_effort: "medium", model_source: "worker default", effort_source: "worker default", active: harness === "codex", ...over,
});
const settings = (over: Partial<UserSettings> = {}): UserSettings => ({
  default_model: null, default_harness: null, default_claude_model: null, default_codex_model: null,
  default_effort: null, default_codex_effort: null, judge_model: null, summary_model: null, theme: null,
  appearance_mode: null, light_theme: null, dark_theme: null, typeface: null,
  cross_check_pins: [pin("claude"), pin("codex")], ...over,
});
const template = (id: string, name: string, model: string | null, over: Partial<AgentTemplate> = {}): AgentTemplate =>
  ({ ...mockTemplates[0], id, name, model, scope: "global", user_id: null, ...over });
const model = (family: string) => screen.getByLabelText(`${family} checker model`) as HTMLSelectElement;
const effort = (family: string) => screen.getByLabelText(`${family} checker effort`) as HTMLSelectElement;
const save = () => screen.getByRole("button", { name: "Save cross-check defaults" }) as HTMLButtonElement;
beforeEach(() => vi.resetAllMocks());
afterEach(cleanup);

describe("checker defaults", () => {
  it("has one accessible Plan row, exact headings, independent Defaults and editable dormant Claude", () => {
    render(<CrossCheckDefaults settings={settings()} userId="me" onSaved={vi.fn()} />);
    const table = screen.getByRole("table", { name: "Cross-check defaults" });
    expect(within(table).getAllByRole("row")).toHaveLength(2);
    expect(within(table).getByRole("columnheader", { name: "Claude cross-checker" })).toBeTruthy();
    expect(within(table).getByRole("columnheader", { name: "Codex cross-checker" })).toBeTruthy();
    expect(screen.getByText("Checks leads running on the other model family")).toBeTruthy();
    expect(screen.getByText("Used once Codex-lead runs are cross-checked")).toBeTruthy();
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
    render(<CrossCheckDefaults settings={settings()} userId="me" onSaved={onSaved} />);
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
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith(response));
    expect(save().disabled).toBe(true);
    expect((screen.getByLabelText("Custom codex checker model ID") as HTMLInputElement).value).toBe("arbitrary-codex-id");
  });

  it("refreshes committed worker defaults without pinning resolved values or overwriting drafts, then resets only model", async () => {
    const initial = settings({ default_claude_model: "opus", cross_check_pins: [pin("claude", { model: "sonnet", effort: "high" }), pin("codex")] });
    const r = render(<CrossCheckDefaults settings={initial} userId="me" onSaved={vi.fn()} />);
    fireEvent.change(model("Codex"), { target: { value: "gpt-6-sol" } });
    r.rerender(<CrossCheckDefaults settings={{ ...initial, default_claude_model: "haiku", default_effort: "max", default_codex_model: "custom-worker" }} userId="me" onSaved={vi.fn()} />);
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
    render(<CrossCheckDefaults settings={initial} userId="me" onSaved={vi.fn()} />);
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

  it("ignores a stale lookup, reuses it after a worker reset to null, and preserves grid drafts", async () => {
    let resolve!: (value: { templates: AgentTemplate[] }) => void;
    mocked.listAgentTemplates.mockReturnValue(new Promise(r => { resolve = r; }));
    mocked.getTemplateAllocations.mockResolvedValue({ templates: [] });
    const initial = settings({ cross_check_pins: [pin("claude", { model: "sonnet", resolved_model: "sonnet" }), pin("codex")] });
    const r = render(<CrossCheckDefaults settings={initial} userId="me" onSaved={vi.fn()} />);
    fireEvent.change(effort("Codex"), { target: { value: "max" } });
    r.rerender(<CrossCheckDefaults settings={{ ...initial, default_claude_model: "haiku" }} userId="me" onSaved={vi.fn()} />);
    resolve({ templates: [] });
    await waitFor(() => expect(within(model("Claude")).getByRole("option", { name: "Default · haiku (worker default)" })).toBeTruthy());
    r.rerender(<CrossCheckDefaults settings={{ ...initial, default_effort: null }} userId="me" onSaved={vi.fn()} />);
    await waitFor(() => expect(within(model("Claude")).getByRole("option", { name: "Default · SDK/account default (worker default)" })).toBeTruthy());
    expect(within(effort("Claude")).getByRole("option", { name: "Default · medium (worker default)" })).toBeTruthy();
    expect(model("Claude").value).toBe("sonnet");
    expect(effort("Codex").value).toBe("max");
    expect(mocked.listAgentTemplates).toHaveBeenCalledTimes(1);
    expect(mocked.getTemplateAllocations).toHaveBeenCalledTimes(1);
  });

  it("does not let a stale template response replace a newer unpinned resolved default", async () => {
    let resolve!: (value: { templates: AgentTemplate[] }) => void;
    mocked.listAgentTemplates.mockReturnValue(new Promise(r => { resolve = r; }));
    mocked.getTemplateAllocations.mockResolvedValue({ templates: [] });
    const initial = settings({ cross_check_pins: [pin("claude", { model: "sonnet", resolved_model: "sonnet" }), pin("codex")] });
    const r = render(<CrossCheckDefaults settings={initial} userId="me" onSaved={vi.fn()} />);
    fireEvent.change(effort("Codex"), { target: { value: "high" } });
    r.rerender(<CrossCheckDefaults settings={settings({ cross_check_pins: [
      pin("claude", { resolved_model: "new-delivered-template" }), pin("codex"),
    ] })} userId="me" onSaved={vi.fn()} />);
    resolve({ templates: [] });
    await waitFor(() => expect(within(model("Claude")).getByRole("option", { name: "Default · new-delivered-template (worker default)" })).toBeTruthy());
    // Flush the old response before checking it cannot replace the known default.
    await waitFor(() => expect(mocked.listAgentTemplates.mock.results[0].value).resolves.toEqual({ templates: [] }));
    expect(within(model("Claude")).getByRole("option", { name: "Default · new-delivered-template (worker default)" })).toBeTruthy();
    expect(model("Claude").value).toBe("sonnet");
    expect(effort("Codex").value).toBe("high");
  });

  it.each(["opus", "two words", "x".repeat(101), "bad\u202e", "\uFFFD", "é".repeat(51), "bad\uFEFF", "a\u0085b", "a\u0001b"])("blocks invalid Codex model %s", async value => {
    render(<CrossCheckDefaults settings={settings()} userId="me" onSaved={vi.fn()} />);
    fireEvent.change(model("Codex"), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("Custom codex checker model ID"), { target: { value } });
    expect(save().disabled).toBe(true);
    expect(mocked.putMySettings).not.toHaveBeenCalled();
  });

  it.each([
    { name: "allocated custom", rows: [template("own", "lead", "wrong", { scope: "user", user_id: "me" }), template("lead", "lead", "allocated-custom")], allocated: ["own", "lead"], expected: "allocated-custom" },
    { name: "unallocated shared shadows own and foreign admin row excluded", rows: [template("own", "lead", "wrong", { scope: "user", user_id: "me" }), template("shared", "lead", "not-delivered"), template("foreign", "LEAD", "wrong", { scope: "user", user_id: "other" }), template("orchestrator", "orchestrator", "custom-orchestrator")], allocated: ["own", "foreign", "orchestrator"], expected: "custom-orchestrator" },
    { name: "model-less first stops", rows: [template("later", "orchestrator", "wrong"), template("first", "LEAD", null)], allocated: ["first", "later"], expected: "SDK/account default" },
    { name: "none delivered", rows: [template("lead", "lead", "wrong")], allocated: [], expected: "SDK/account default" },
  ])("recovers initial Claude pin's true default: $name", async ({ rows, allocated, expected }) => {
    mocked.listAgentTemplates.mockResolvedValue({ templates: rows });
    mocked.getTemplateAllocations.mockResolvedValue({ templates: rows.map(t => ({ id: t.id, name: t.name, description: "", scope: t.scope, is_builtin: false, global_default: false, my_override: null, effective: allocated.includes(t.id) })) });
    render(<CrossCheckDefaults settings={settings({ cross_check_pins: [pin("claude", { model: "sonnet", resolved_model: "sonnet", model_source: "pin" }), pin("codex")] })} userId="me" onSaved={vi.fn()} />);
    await waitFor(() => expect(within(model("Claude")).getByRole("option", { name: `Default · ${expected} (worker default)` })).toBeTruthy());
    fireEvent.change(model("Claude"), { target: { value: "haiku" } });
    expect(mocked.listAgentTemplates).toHaveBeenCalledTimes(1);
  });

  it("reports required template lookup failure and blocks save", async () => {
    mocked.listAgentTemplates.mockRejectedValue(new ApiError(500, "templates unavailable"));
    mocked.getTemplateAllocations.mockResolvedValue({ templates: [] });
    render(<CrossCheckDefaults settings={settings({ cross_check_pins: [pin("claude", { model: "sonnet" }), pin("codex")] })} userId="me" onSaved={vi.fn()} />);
    fireEvent.change(model("Claude"), { target: { value: "haiku" } });
    await screen.findByText("templates unavailable");
    expect(save().disabled).toBe(true);
    fireEvent.change(effort("Codex"), { target: { value: "high" } });
    expect(mocked.listAgentTemplates).toHaveBeenCalledTimes(1);
  });
});
