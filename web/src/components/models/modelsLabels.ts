// Copy for the Models grid (PRD #3263): each select's Default option, each cell's "Saved
// effective value" line, and the judge's routing and Not used copy. Every word about where a
// value comes from is the server's source token rendered through lib/modelSettings.ts; nothing
// here resolves precedence. The Default labels follow Run defaults' wording (RunDefaults.tsx
// modelDefaultLabel, judgeRunsOnHelp and the task-review labels; the checker labels of the
// retired CrossCheckDefaults grid), so both pages name a default the same way until M3 retires
// Run defaults' model editors.

import type {
  CrossCheckPinDTO,
  EffectiveEffortCell,
  EffectiveModelCell,
  EffectiveSettings,
  EffortSettingCell,
  Harness,
  ModelSettingCell,
  UserSettings,
} from "../../lib/apiTypes";
import { modelSettingSourceLabel } from "../../lib/modelSettings";
import type { ModelsField } from "../../lib/modelsDraft";
import { DEFAULT_CODEX_MODEL } from "../ModelSelect";

const HARNESS_NAME: Record<Harness, string> = { claude: "Claude", codex: "Codex" };

export function harnessName(harness: Harness): string {
  return HARNESS_NAME[harness];
}

// The effective-settings `setting` token each Models row predicts.
const MODEL_SETTING: Record<string, EffectiveModelCell["setting"]> = {
  "Worker model": "worker_model",
  "Planning model": "planning_model",
  "Judge model": "judge_model",
  "Summary model": "summary_model",
  "Task review model": "task_review_model",
};
const EFFORT_SETTING: Record<string, EffectiveEffortCell["setting"]> = {
  "Worker effort": "worker_effort",
  "Planning effort": "planning_effort",
  "Task review effort": "task_review_effort",
};

// The /me/settings cells a helper row falls back to when the effective view lacks its cell
// (an older server or a failed prediction read): the same server resolution, already saved.
const MODEL_CELLS: Record<string, (s: UserSettings) => ModelSettingCell[] | null | undefined> = {
  "Judge model": (s) => s.judge_models,
  "Summary model": (s) => s.summary_models,
  "Task review model": (s) => s.task_review_models,
};
const EFFORT_CELLS: Record<string, (s: UserSettings) => EffortSettingCell[] | null | undefined> = {
  "Task review effort": (s) => s.task_review_efforts,
};

// The source-label field the server's token belongs to: planning cells carry their own
// vocabulary (user_planning_default reads "your choice").
function sourceField(field: ModelsField): string {
  if (field.row.startsWith("Planning")) return field.kind === "model" ? "planning_model" : "planning_effort";
  return field.kind;
}

function modelValue(model: string): string {
  return model === "" ? modelSettingSourceLabel("note", "sdk_default") : model;
}

function effortValue(cell: Pick<EffortSettingCell, "mode" | "resolved_effort">): string {
  if (cell.mode === "runtime_default") return "Runtime default";
  return cell.resolved_effort ?? "unknown";
}

// A server note worth showing after the source ("not used: the judge runs on the other
// harness"); sdk_default is already the value itself.
function noteSuffix(notes: readonly string[] | undefined, model: string): string {
  const shown = (notes ?? []).filter((n) => !(n === "sdk_default" && model === ""));
  return shown.map((n) => ` · ${modelSettingSourceLabel("note", n)}`).join("");
}

function judgeRoutingText(value: string | null | undefined, source: string): string {
  if (value === "claude" || value === "codex") return `${HARNESS_NAME[value]} · ${modelSettingSourceLabel("judge_harness", source)}`;
  return "Same as the run";
}

// A cross-check cell's prediction: the effective view's cross_check cell, else the stored pin's
// own resolution (model_source/effort_source read as the matching provenance token).
function pinPrediction(field: ModelsField, effective: EffectiveSettings | null, settings: UserSettings | null): string | null {
  if (!("pin" in field.target)) return null;
  const { stage, harness, member } = field.target.pin;
  const setting = stage === "plan" ? "cross_check_plan" : "cross_check_code";
  if (member === "model") {
    const cell = effective?.cross_check?.models?.find((c) => c.setting === setting && c.harness === harness);
    if (cell) return `${modelValue(cell.resolved_model)} · ${modelSettingSourceLabel("model", cell.source)}`;
    const pin = settings?.cross_check_pins?.find((p) => p.stage === stage && p.harness === harness);
    if (!pin) return null;
    const token = pin.model_source === "pin" ? "cross_check_pin" : "cross_check_worker_default";
    return `${modelValue(pin.resolved_model ?? "")} · ${modelSettingSourceLabel("model", token)}`;
  }
  const cell = effective?.cross_check?.efforts?.find((c) => c.setting === setting && c.harness === harness);
  if (cell) return `${effortValue(cell)} · ${modelSettingSourceLabel("effort", cell.source)}`;
  const pin = settings?.cross_check_pins?.find((p) => p.stage === stage && p.harness === harness);
  if (!pin) return null;
  const token = pin.effort_source === "pin" ? "cross_check_pin" : "cross_check_default";
  return `${pin.resolved_effort} · ${modelSettingSourceLabel("effort", token)}`;
}

// Rows added with the helper and cross-check groups read the effective view first, then fall
// back to the saved /me/settings cell; the worker and planning rows read the effective view only.
const SETTINGS_FALLBACK = new Set(["Judge runs on", ...Object.keys(MODEL_CELLS), ...Object.keys(EFFORT_CELLS)]);

// predictionText is the saved prediction for one cell, without the "Saved effective value"
// prefix, or null when neither the effective settings nor (for a helper or cross-check row)
// the saved settings cell carry one.
export function predictionText(field: ModelsField, effective: EffectiveSettings | null, settings: UserSettings | null = null): string | null {
  if ("pin" in field.target) return pinPrediction(field, effective, settings);
  const fallback = SETTINGS_FALLBACK.has(field.row) ? settings : null;
  if (field.kind === "routing") {
    if (effective?.judge_harness) return judgeRoutingText(effective.judge_harness.value, effective.judge_harness.source);
    return fallback ? judgeRoutingText(fallback.judge_runs_on, "user_default") : null;
  }
  if (!effective && !fallback) return null;
  if (field.kind === "harness") {
    if (!effective) return null;
    const value = effective.start?.find((c) => c.choice === "default")?.harness;
    if (!value) return null;
    const notes = (value.notes ?? []).map((n) => modelSettingSourceLabel("note", n));
    // No fixed harness reads "Automatic", matching the select's "Default · automatic", so the
    // source label ("chosen when the run is created", or a note such as "chosen by the soonest
    // weekly reset") says how it is chosen without repeating "chosen".
    const shown = value.value === "claude" || value.value === "codex" ? HARNESS_NAME[value.value] : "Automatic";
    const source = notes.length > 0 ? notes.join(", ") : modelSettingSourceLabel("harness", value.source);
    return `${shown} · ${source}`;
  }
  if (field.kind === "model") {
    const setting = MODEL_SETTING[field.row];
    const live = effective?.models?.find((c) => c.setting === setting && c.harness === field.family);
    const cell = live ?? (fallback ? MODEL_CELLS[field.row]?.(fallback)?.find((c) => c.harness === field.family) : undefined);
    if (!cell) return null;
    return `${modelValue(cell.resolved_model)} · ${modelSettingSourceLabel(sourceField(field), cell.source)}${fallback ? noteSuffix(live?.notes, cell.resolved_model) : ""}`;
  }
  const setting = EFFORT_SETTING[field.row];
  const cell =
    effective?.efforts?.find((c) => c.setting === setting && c.harness === field.family) ??
    (fallback ? EFFORT_CELLS[field.row]?.(fallback)?.find((c) => c.harness === field.family) : undefined);
  if (!cell) return null;
  return `${effortValue(cell)} · ${modelSettingSourceLabel(sourceField(field), cell.source)}`;
}

// Worker model Default: what the cleared override resolves to, from the saved prediction's
// default_model / default_source; an older server or a missing prediction keeps Run defaults' copy.
export function workerModelDefaultLabel(harness: Harness, cell: ModelSettingCell | undefined): string {
  if (!cell) return harness === "claude" ? "Default · lead template" : `Default · ${DEFAULT_CODEX_MODEL} (built-in)`;
  return `Default · ${modelValue(cell.default_model)} (${modelSettingSourceLabel("model", cell.default_source)})`;
}

export function workerEffortDefaultLabel(cell: EffortSettingCell | undefined): string {
  if (!cell) return "Default · lead template's effort, else medium (built-in)";
  const level = cell.default_effort === "runtime" ? "Runtime default" : cell.default_effort;
  return `Default · ${level} (${modelSettingSourceLabel("effort", cell.default_source)})`;
}

// Planning Default is "Same as the run" (PRD #3252): the run's own worker model and effort,
// including the lead template's tier, as the server-resolved planning cells name it.
export function planningModelDefaultLabel(cell: ModelSettingCell | undefined): string {
  if (!cell) return "Same as the run";
  return cell.default_model === ""
    ? `Same as the run · ${modelSettingSourceLabel("note", "sdk_default")}`
    : `Same as the run · ${cell.default_model} (${modelSettingSourceLabel("planning_model", cell.default_source)})`;
}

export function planningEffortDefaultLabel(cell: EffortSettingCell | undefined): string {
  if (!cell) return "Same as the run";
  const level = cell.default_effort === "runtime" ? modelSettingSourceLabel("effort", "runtime_default") : cell.default_effort;
  return `Same as the run · ${level} (${modelSettingSourceLabel("planning_effort", cell.default_source)})`;
}

// harnessReason is the credential fact for a family column: refusal is data, never a reason to
// disable editing, so the copy says the choice still saves.
export function harnessReason(harness: Harness, effective: EffectiveSettings | null): string | null {
  const h = effective?.harnesses?.find((x) => x.harness === harness);
  if (!h || h.usable) return null;
  return `No usable ${HARNESS_NAME[harness]} credential right now. You can still save this choice; runs use it once one is added.`;
}

// The helper models' Default (judge, summary): the server-resolved cell's default and source, as
// Run defaults' modelDefaultLabel; an older server without cells reads plain "Default".
export function helperModelDefaultLabel(cell: ModelSettingCell | undefined): string {
  return cell ? `Default · ${modelValue(cell.default_model)} (${modelSettingSourceLabel("model", cell.default_source)})` : "Default";
}

// Task review: the Claude built-in is the SDK default (an empty model), the Codex built-in
// gpt-6-sol; with a cell both read the server's default and source.
const TASK_REVIEW_BUILTIN: Record<Harness, string> = { claude: "SDK default", codex: "gpt-6-sol" };

export function taskReviewModelDefaultLabel(harness: Harness, cell: ModelSettingCell | undefined): string {
  if (!cell) return `Default · ${TASK_REVIEW_BUILTIN[harness]} (${modelSettingSourceLabel("model", "builtin")})`;
  const model = cell.default_model === "" ? TASK_REVIEW_BUILTIN.claude : cell.default_model;
  return `Default · ${model} (${modelSettingSourceLabel("model", cell.default_source)})`;
}

// An unset task-review effort follows the ordinary worker effort for its family, including
// Runtime default, as the server's task_review_efforts cell names it (no lead-template tier).
export function taskReviewEffortDefaultLabel(cell: EffortSettingCell | undefined): string {
  if (!cell) return "Default · follows worker effort";
  return `Default · follows worker effort (${cell.default_effort === "runtime" ? "Runtime default" : cell.default_effort})`;
}

// A checker's Default model is the worker default of its family, from the stored pin's
// counterfactual worker_default_model: absent (an older server) or a Codex null reads
// Unavailable; a Claude null is the SDK/account default. Effort never follows the worker: an
// unpinned checker runs at the cross-check default (agenttmpl.CrossCheckDefaultEffort).
export function checkerModelDefaultLabel(harness: Harness, pin: CrossCheckPinDTO | undefined): string {
  const worker = pin?.worker_default_model;
  const shown = worker === undefined || (worker === null && harness === "codex") ? "Unavailable" : worker ?? "SDK/account default";
  return `Default · ${shown} (worker default)`;
}

export const CHECKER_EFFORT_DEFAULT_LABEL = "Default · high (cross-check default)";

// Judge runs on: what each choice does to a judged run (Run defaults' judgeRunsOnHelp), for the
// DRAFT choice, so the copy follows the select before it is saved.
export function judgeRunsOnHelp(choice: string): string {
  if (choice !== "claude" && choice !== "codex") {
    return "Claude runs use the Claude judge model, Codex runs the Codex model. Pick Claude or Codex to judge every run on one harness.";
  }
  const on = HARNESS_NAME[choice];
  const other = HARNESS_NAME[choice === "claude" ? "codex" : "claude"];
  return `Every run is judged on ${on}, so ${other} runs get a ${on} judge. If ${on} can't run, the judge is skipped, never charged to ${other}.`;
}

// The judge lane the DRAFT routing leaves unused, with the reason shown on (and linked to) its
// cell; null when both lanes are in play (Same as the run) or for a non-judge field.
export function judgeNotUsedReason(field: ModelsField, judgeRunsOn: string): string | null {
  if (field.row !== "Judge model" || (judgeRunsOn !== "claude" && judgeRunsOn !== "codex")) return null;
  return field.family === judgeRunsOn ? null : `Not used: the judge runs on ${HARNESS_NAME[judgeRunsOn]}`;
}
