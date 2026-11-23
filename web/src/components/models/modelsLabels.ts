// Copy for the Models grid (PRD #3263): each select's Default option, and each cell's
// "Saved effective value" line. Every word about where a value comes from is the server's
// source token rendered through lib/modelSettings.ts; nothing here resolves precedence. The
// Default labels follow Run defaults' wording (RunDefaults.tsx modelDefaultLabel and its
// planning labels), so the two pages name the same default the same way.

import type {
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
};
const EFFORT_SETTING: Record<string, EffectiveEffortCell["setting"]> = {
  "Worker effort": "worker_effort",
  "Planning effort": "planning_effort",
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

// predictionText is the saved prediction for one cell, without the "Saved effective value"
// prefix, or null when the effective settings are unavailable or carry no cell for it.
export function predictionText(field: ModelsField, effective: EffectiveSettings | null): string | null {
  if (!effective) return null;
  if (field.kind === "harness") {
    const value = effective.start?.find((c) => c.choice === "default")?.harness;
    if (!value) return null;
    const notes = (value.notes ?? []).map((n) => modelSettingSourceLabel("note", n));
    // No fixed harness reads "Automatic", matching the select's "Default · automatic", so the
    // source label ("chosen when the run is created", or a note such as "chosen by the soonest
    // weekly reset") says how it is chosen without repeating "chosen".
    const shown = value.value === "claude" || value.value === "codex" ? HARNESS_NAME[value.value] : "Automatic";
    const source = notes.length > 0 ? notes.join(", ") : modelSettingSourceLabel("harness", value.source);
    return shown === source ? shown : `${shown} · ${source}`;
  }
  if (field.kind === "model") {
    const setting = MODEL_SETTING[field.row];
    const cell = effective.models?.find((c) => c.setting === setting && c.harness === field.family);
    if (!cell) return null;
    return `${modelValue(cell.resolved_model)} · ${modelSettingSourceLabel(sourceField(field), cell.source)}`;
  }
  const setting = EFFORT_SETTING[field.row];
  const cell = effective.efforts?.find((c) => c.setting === setting && c.harness === field.family);
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
