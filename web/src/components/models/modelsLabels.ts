// Copy for the Models grid (PRD #3263): each select's Default option, each cell's "Saved
// effective value" line, and the judge's routing and Not used copy. Every word about where a
// value comes from is the server's source token rendered through lib/modelSettings.ts; nothing
// here resolves precedence. Some Default labels and the judge copy keep the wording the retired
// Run defaults model editors used (judgeRunsOnHelp, the helper and task-review Default labels) and
// the retired CrossCheckDefaults grid's checker labels; the worker and planning Default labels are
// read from the server cell's default_model / default_source. Run defaults' read-only judge
// summary reuses harnessName, and judgeRunsOnHelp for an explicit Claude or Codex choice.

import type {
  CrossCheckPinDTO,
  EffectiveEffortCell,
  EffectiveModelCell,
  EffectiveSettings,
  EffortSettingCell,
  Harness,
  ModelSettingCell,
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
// harness"); sdk_default is already the value itself, and not_used is dropped when the cell
// already says so in its own "Not used" line (omitNotUsed), so it is announced once.
function noteSuffix(notes: readonly string[] | undefined, model: string, omitNotUsed: boolean): string {
  const shown = (notes ?? []).filter((n) => !(n === "sdk_default" && model === "") && !(omitNotUsed && n === "not_used"));
  return shown.map((n) => ` · ${modelSettingSourceLabel("note", n)}`).join("");
}

// The helper model rows show the server's notes after the source (the judge's not_used); the
// worker and planning rows show the value and source alone.
const NOTED_MODEL_ROWS = new Set(["Judge model", "Summary model", "Task review model"]);

function judgeRoutingText(value: string | null | undefined, source: string): string {
  if (value === "claude" || value === "codex") return `${HARNESS_NAME[value]} · ${modelSettingSourceLabel("judge_harness", source)}`;
  return "Same as the run";
}

// A cross-check cell's prediction: the effective view's cross_check cell for its stage and
// family, with the server's own source token. The stored pin is never read as a prediction.
function pinPrediction(field: ModelsField, effective: EffectiveSettings): string | null {
  if (!("pin" in field.target)) return null;
  const { stage, harness, member } = field.target.pin;
  const setting = stage === "plan" ? "cross_check_plan" : "cross_check_code";
  if (member === "model") {
    const cell = effective.cross_check?.models?.find((c) => c.setting === setting && c.harness === harness);
    return cell ? `${modelValue(cell.resolved_model)} · ${modelSettingSourceLabel("model", cell.source)}` : null;
  }
  const cell = effective.cross_check?.efforts?.find((c) => c.setting === setting && c.harness === harness);
  return cell ? `${effortValue(cell)} · ${modelSettingSourceLabel("effort", cell.source)}` : null;
}

// predictionText is the saved prediction for one cell, without the "Saved effective value"
// prefix. It reads ONLY the server's effective view (buildEffectiveSettings, from PredictLead and
// the stored pins' server resolution): with no view (a failed read) or no cell for this row in
// it, the answer is null and the cell reads Unavailable, never a guess from /me/settings.
// omitNotUsed drops the server's not_used note, for a cell that shows its own Not used reason.
export function predictionText(
  field: ModelsField,
  effective: EffectiveSettings | null,
  { omitNotUsed = false }: { omitNotUsed?: boolean } = {},
): string | null {
  if (!effective) return null;
  if ("pin" in field.target) return pinPrediction(field, effective);
  if (field.kind === "routing") {
    return effective.judge_harness ? judgeRoutingText(effective.judge_harness.value, effective.judge_harness.source) : null;
  }
  if (field.kind === "harness") {
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
    const cell = effective.models?.find((c) => c.setting === MODEL_SETTING[field.row] && c.harness === field.family);
    if (!cell) return null;
    const notes = NOTED_MODEL_ROWS.has(field.row) ? noteSuffix(cell.notes, cell.resolved_model, omitNotUsed) : "";
    return `${modelValue(cell.resolved_model)} · ${modelSettingSourceLabel(sourceField(field), cell.source)}${notes}`;
  }
  const cell = effective.efforts?.find((c) => c.setting === EFFORT_SETTING[field.row] && c.harness === field.family);
  if (!cell) return null;
  return `${effortValue(cell)} · ${modelSettingSourceLabel(sourceField(field), cell.source)}`;
}

// The server's source token for a value the user stored in THIS field: a checker pin, a
// planning choice, or any other user default. A source from another setting never confirms it.
function ownSource(field: ModelsField): string {
  if ("pin" in field.target) return "cross_check_pin";
  return field.row.startsWith("Planning") ? "user_planning_default" : "user_default";
}

// predictionRedundant is true only when the control ALREADY shows everything the saved
// prediction would (#4671), so the cell may hide its visible line (the accessible description
// keeps it). It is proven against what is rendered, never re-resolved:
// - an explicit saved value: the server resolves exactly that value from the user's own stored
//   choice in this very field (an explicit Runtime default only where the control offers one);
// - Default: the rendered Default label ends with the prediction's own " · <value> (<source>)";
// - Judge runs on "Same as the run": the select reads exactly what the prediction would.
// No view, no cell, a shown note, an ignored value, a source the label lacks, or a hidden value
// (Default harness "automatic") returns false, so the cell shows its line. `saved` is the
// field's normalized SAVED value ("" = Default); `defaultLabel` is the select's Default option.
export function predictionRedundant(
  field: ModelsField,
  effective: EffectiveSettings | null,
  saved: string,
  { defaultLabel = "", runtimeOffered = false, omitNotUsed = false }: { defaultLabel?: string; runtimeOffered?: boolean; omitNotUsed?: boolean } = {},
): boolean {
  if (!effective) return false;
  if (field.kind === "routing") {
    const jh = effective.judge_harness;
    if (!jh) return false;
    if ((jh.notes ?? []).length > 0) return false;
    return saved === "" ? jh.value == null && jh.source === "same_as_run" : jh.value === saved && jh.source === "user_default";
  }
  if (field.kind === "harness") {
    const value = effective.start?.find((c) => c.choice === "default")?.harness;
    return saved !== "" && !!value && value.value === saved && value.source === "user_default" && (value.notes ?? []).length === 0;
  }
  const pin = "pin" in field.target ? field.target.pin : null;
  const setting = pin ? (pin.stage === "plan" ? "cross_check_plan" : "cross_check_code") : null;
  const isModel = pin ? pin.member === "model" : field.kind === "model";
  const srcField = pin ? (isModel ? "model" : "effort") : sourceField(field);
  if (isModel) {
    const cell = pin
      ? effective.cross_check?.models?.find((c) => c.setting === setting && c.harness === pin.harness)
      : effective.models?.find((c) => c.setting === MODEL_SETTING[field.row] && c.harness === field.family);
    if (!cell || cell.ignored) return false;
    if ((cell.notes ?? []).some((n) => !(omitNotUsed && n === "not_used"))) return false;
    if (saved !== "") return cell.resolved_model === saved && cell.source === ownSource(field);
    return labelShows(defaultLabel, modelValue(cell.resolved_model), modelSettingSourceLabel(srcField, cell.source));
  }
  const cell = pin
    ? effective.cross_check?.efforts?.find((c) => c.setting === setting && c.harness === pin.harness)
    : effective.efforts?.find((c) => c.setting === EFFORT_SETTING[field.row] && c.harness === field.family);
  if (!cell || (cell.notes ?? []).length > 0) return false;
  if (saved === "runtime") return runtimeOffered && cell.mode === "runtime_default" && cell.source === ownSource(field);
  if (saved !== "") return cell.mode === "explicit" && cell.resolved_effort === saved && cell.source === ownSource(field);
  return labelShows(defaultLabel, effortValue(cell), modelSettingSourceLabel(srcField, cell.source));
}

// A Default label shows a prediction only when its whole value-and-provenance part is the same
// " · <value> (<source>)" the line would show ("claude-custom-opus" never proves "opus").
function labelShows(label: string, value: string, source: string): boolean {
  return source !== "unknown" && label.endsWith(` · ${value} (${source})`);
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
  if (worker === undefined || (worker === null && harness === "codex")) return "Default · Unavailable (worker default)";
  // The SDK/account default IS the Claude worker default, so the source is not repeated: the
  // shorter label fits the select without truncating at desktop widths.
  return worker === null ? "Default · SDK/account default" : `Default · ${worker} (worker default)`;
}

export const CHECKER_EFFORT_DEFAULT_LABEL = "Default · high (cross-check default)";

// Judge runs on: what each choice does to a judged run. Models passes the DRAFT choice, so the
// copy follows the select before it is saved; Run defaults' judge summary passes the saved one.
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
