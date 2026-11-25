import { describe, expect, it } from "vitest";
import type { EffectiveSettings } from "../../lib/api";
import { MODELS_FIELDS } from "../../lib/modelsDraft";
import { predictionRedundant, predictionText } from "./modelsLabels";

const harnessField = MODELS_FIELDS.find((f) => f.kind === "harness")!;
const withStart = (harness: unknown) => ({ start: [{ choice: "default", harness }] }) as unknown as EffectiveSettings;

describe("Default harness prediction", () => {
  it("names an automatic harness once, then how it is chosen", () => {
    expect(predictionText(harnessField, withStart({ value: null, source: "chosen_at_create", notes: ["automatic_weekly_reset"] })))
      .toBe("Automatic · chosen by the soonest weekly reset");
    expect(predictionText(harnessField, withStart({ value: null, source: "chosen_at_create", notes: [] })))
      .toBe("Automatic · chosen when the run is created");
  });
});

// #4671: a clean cell hides its visible saved line only when the control provably shows the
// same thing; every unproven case shows it.
describe("predictionRedundant", () => {
  const field = (row: string, family: "claude" | "codex" | null = "claude") => MODELS_FIELDS.find((f) => f.row === row && f.family === family)!;
  const mcell = (setting: string, over: object = {}) => ({
    harness: "claude", setting, model: null, default_model: "opus", default_source: "template",
    resolved_model: "opus", source: "template", fixed_at: "claim", ignored: null, notes: [], ...over,
  });
  const ecell = (setting: string, over: object = {}) => ({
    harness: "claude", setting, effort: null, default_effort: "medium", default_source: "builtin",
    resolved_effort: "medium", source: "builtin", mode: "explicit", fixed_at: "claim", notes: [], ...over,
  });
  const view = (over: object = {}) => ({
    start: [{ choice: "default", harness: { value: "claude", source: "automatic", fixed_at: "create", notes: [] } }],
    models: [], efforts: [], cross_check: { models: [], efforts: [] },
    judge_harness: { value: null, source: "same_as_run", fixed_at: "create", notes: [] },
    ...over,
  }) as unknown as EffectiveSettings;
  const worker = field("Worker model");
  const effort = field("Worker effort");

  it("hides a Default whose label already carries the prediction's value and source", () => {
    const eff = view({ models: [mcell("worker_model")] });
    expect(predictionRedundant(worker, eff, "", { defaultLabel: "Default · opus (lead template)" })).toBe(true);
    // A label without provenance, or naming another value or source, keeps the line.
    expect(predictionRedundant(worker, eff, "", { defaultLabel: "Default · lead template" })).toBe(false);
    expect(predictionRedundant(worker, eff, "", { defaultLabel: "Default · sonnet (lead template)" })).toBe(false);
    expect(predictionRedundant(worker, eff, "", { defaultLabel: "Default · opus (built-in)" })).toBe(false);
    // A model ID that merely ends in the predicted one proves nothing.
    expect(predictionRedundant(worker, eff, "", { defaultLabel: "Default · claude-custom-opus (lead template)" })).toBe(false);
  });

  it("hides an explicit value only when the server resolves it from the user's own choice", () => {
    expect(predictionRedundant(worker, view({ models: [mcell("worker_model", { resolved_model: "sonnet", source: "user_default" })] }), "sonnet")).toBe(true);
    expect(predictionRedundant(worker, view({ models: [mcell("worker_model", { resolved_model: "opus", source: "user_default" })] }), "sonnet")).toBe(false);
    expect(predictionRedundant(worker, view({ models: [mcell("worker_model", { resolved_model: "sonnet", source: "template" })] }), "sonnet")).toBe(false);
    const planning = field("Planning model");
    expect(predictionRedundant(planning, view({ models: [mcell("planning_model", { resolved_model: "sonnet", source: "user_planning_default" })] }), "sonnet")).toBe(true);
    // A source belonging to another setting never confirms this field's override.
    expect(predictionRedundant(worker, view({ models: [mcell("worker_model", { resolved_model: "sonnet", source: "cross_check_pin" })] }), "sonnet")).toBe(false);
    expect(predictionRedundant(worker, view({ models: [mcell("worker_model", { resolved_model: "sonnet", source: "user_planning_default" })] }), "sonnet")).toBe(false);
    expect(predictionRedundant(planning, view({ models: [mcell("planning_model", { resolved_model: "sonnet", source: "user_default" })] }), "sonnet")).toBe(false);
  });

  it("shows the line for an ignored value, a note, a missing cell or no view", () => {
    const label = { defaultLabel: "Default · opus (lead template)" };
    expect(predictionRedundant(worker, view({ models: [mcell("worker_model", { ignored: { reason: "x" } })] }), "", label)).toBe(false);
    expect(predictionRedundant(worker, view({ models: [mcell("worker_model", { notes: ["schedule_pin_ignored"] })] }), "", label)).toBe(false);
    expect(predictionRedundant(worker, view(), "", label)).toBe(false);
    expect(predictionRedundant(worker, null, "", label)).toBe(false);
    // The judge's not_used note is dropped only when the cell shows its own Not used line.
    const judge = field("Judge model");
    const judged = view({ models: [mcell("judge_model", { default_source: "builtin", source: "builtin", notes: ["not_used"] })] });
    expect(predictionRedundant(judge, judged, "", { defaultLabel: "Default · opus (built-in)" })).toBe(false);
    expect(predictionRedundant(judge, judged, "", { defaultLabel: "Default · opus (built-in)", omitNotUsed: true })).toBe(true);
  });

  it("treats Runtime default as a value: explicit only where offered, Default through the label", () => {
    const runtime = view({ efforts: [ecell("worker_effort", { resolved_effort: null, mode: "runtime_default", source: "user_default" })] });
    expect(predictionRedundant(effort, runtime, "runtime", { runtimeOffered: true })).toBe(true);
    expect(predictionRedundant(effort, runtime, "runtime", { runtimeOffered: false })).toBe(false);
    expect(predictionRedundant(effort, runtime, "high", { runtimeOffered: true })).toBe(false);
    const dflt = view({ efforts: [ecell("worker_effort")] });
    expect(predictionRedundant(effort, dflt, "", { defaultLabel: "Default · medium (built-in)" })).toBe(true);
    // Task review's "follows worker effort (medium)" names no source.
    const tr = field("Task review effort");
    expect(predictionRedundant(tr, view({ efforts: [ecell("task_review_effort")] }), "", { defaultLabel: "Default · follows worker effort (medium)" })).toBe(false);
  });

  it("proves a checker pin against its own label and source", () => {
    const pin = MODELS_FIELDS.find((f) => f.row === "Plan cross-check model" && f.family === "claude")!;
    const pinEffort = MODELS_FIELDS.find((f) => f.row === "Plan cross-check effort" && f.family === "claude")!;
    const cc = (models: object[], efforts: object[] = []) => view({ cross_check: { models, efforts } });
    const workerDefault = mcell("cross_check_plan", { source: "cross_check_worker_default" });
    expect(predictionRedundant(pin, cc([workerDefault]), "", { defaultLabel: "Default · opus (worker default)" })).toBe(true);
    expect(predictionRedundant(pin, cc([workerDefault]), "", { defaultLabel: "Default · Unavailable (worker default)" })).toBe(false);
    expect(predictionRedundant(pin, cc([mcell("cross_check_plan", { resolved_model: "sonnet", source: "cross_check_pin" })]), "sonnet")).toBe(true);
    const ccDefault = ecell("cross_check_plan", { resolved_effort: "high", source: "cross_check_default" });
    expect(predictionRedundant(pinEffort, cc([], [ccDefault]), "", { defaultLabel: "Default · high (cross-check default)" })).toBe(true);
  });

  it("keeps the line for Default · automatic and for a judge routing it cannot confirm", () => {
    const harness = field("Default harness", null);
    expect(predictionRedundant(harness, view(), "")).toBe(false);
    const fixed = view({ start: [{ choice: "default", harness: { value: "codex", source: "user_default", fixed_at: "create", notes: [] } }] });
    expect(predictionRedundant(harness, fixed, "codex")).toBe(true);
    expect(predictionRedundant(harness, fixed, "claude")).toBe(false);
    const routing = field("Judge runs on", null);
    expect(predictionRedundant(routing, view(), "")).toBe(true);
    expect(predictionRedundant(routing, view({ judge_harness: { value: null, source: "user_default", fixed_at: "create", notes: [] } }), "")).toBe(false);
    expect(predictionRedundant(routing, view({ judge_harness: { value: null, source: "same_as_run", fixed_at: "create", notes: ["x"] } }), "")).toBe(false);
    expect(predictionRedundant(routing, view({ judge_harness: { value: "codex", source: "user_default", fixed_at: "create", notes: [] } }), "codex")).toBe(true);
  });
});
