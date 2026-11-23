// The Models tab's draft model (PRD #3263, module C): one stored baseline, one draft, the
// semantic dirty set between them, the one PUT /me/model-settings patch that saves it, and the
// map from a 400's field errors back to the cells that caused them. Pure and React-free, so the
// page owns state and this file owns meaning.
//
// A field is a DESCRIPTOR, not a hand-written branch: M1 ships the harness, worker and planning
// rows; M2 adds the judge, summary, task-review and cross-check rows by adding descriptors.
// Cross-check pins already have their shape here (`pin` targets, keyed
// `cross_check_pins:<stage>/<harness>/<member>`), so the patch builder and the error map need no
// change when they arrive.
//
// Values are strings: "" is Default (a cleared override, sent as null), anything else is the
// override, compared and sent trimmed. "runtime" is an ordinary non-empty value, so Runtime
// default stays distinct from Default. Predictions are not part of the draft and never count.

import type { Harness, ModelSettingsFieldError, ModelSettingsPatch, ModelSettingsPinPatch, UserSettings } from "./apiTypes";

type ScalarKey = Exclude<keyof ModelSettingsPatch, "cross_check_pins">;
type PinStage = ModelSettingsPinPatch["stage"];
type PinMember = "model" | "effort";

export type ModelsFieldKind = "harness" | "model" | "effort";

export interface ModelsField {
  /** Stable identity: the patch key for a scalar, `cross_check_pins:<stage>/<harness>/<member>` for a pin. */
  key: string;
  /** The grid row this field belongs to, e.g. "Worker model". */
  row: string;
  /** The family column, or null for a row with a single control (Default harness). */
  family: Harness | null;
  kind: ModelsFieldKind;
  /** The stored value in the settings DTO (null/undefined/"" all read as Default). */
  read: (settings: UserSettings) => string | null | undefined;
  target: { scalar: ScalarKey } | { pin: { stage: PinStage; harness: Harness; member: PinMember } };
}

export type ModelsValues = Record<string, string>;

function scalar(key: ScalarKey, row: string, family: Harness | null, kind: ModelsFieldKind): ModelsField {
  return {
    key,
    row,
    family,
    kind,
    read: (s) => (s as unknown as Record<string, string | null | undefined>)[key],
    target: { scalar: key },
  };
}

function pinKey(stage: string, harness: string, member: string): string {
  return `cross_check_pins:${stage}/${harness}/${member}`;
}

/** pinField describes one member of a cross-check pin cell (the M2 rows). */
export function pinField(row: string, stage: PinStage, harness: Harness, member: PinMember): ModelsField {
  return {
    key: pinKey(stage, harness, member),
    row,
    family: harness,
    kind: member,
    read: (s) => s.cross_check_pins?.find((p) => p.stage === stage && p.harness === harness)?.[member],
    target: { pin: { stage, harness, member } },
  };
}

/** The M1 rows, in display order. */
export const MODELS_FIELDS: readonly ModelsField[] = [
  scalar("default_harness", "Default harness", null, "harness"),
  scalar("default_claude_model", "Worker model", "claude", "model"),
  scalar("default_codex_model", "Worker model", "codex", "model"),
  scalar("default_effort", "Worker effort", "claude", "effort"),
  scalar("default_codex_effort", "Worker effort", "codex", "effort"),
  scalar("planning_claude_model", "Planning model", "claude", "model"),
  scalar("planning_codex_model", "Planning model", "codex", "model"),
  scalar("planning_claude_effort", "Planning effort", "claude", "effort"),
  scalar("planning_codex_effort", "Planning effort", "codex", "effort"),
];

/** normalizeValue is the semantic form of a value: trimmed, with null/undefined as "" (Default). */
export function normalizeValue(value: string | null | undefined): string {
  return (value ?? "").trim();
}

/** fieldLabel is a field's accessible name, "<Row> — <Family>" (just the row without a family). */
export function fieldLabel(field: ModelsField): string {
  if (!field.family) return field.row;
  return `${field.row} — ${field.family === "claude" ? "Claude" : "Codex"}`;
}

/** baselineFromSettings reads every field's stored value, normalized. */
export function baselineFromSettings(settings: UserSettings, fields: readonly ModelsField[] = MODELS_FIELDS): ModelsValues {
  const values: ModelsValues = {};
  for (const f of fields) values[f.key] = normalizeValue(f.read(settings));
  return values;
}

/** dirtyFields lists, in field order, every field whose draft differs semantically from the baseline. */
export function dirtyFields(baseline: ModelsValues, draft: ModelsValues, fields: readonly ModelsField[] = MODELS_FIELDS): ModelsField[] {
  return fields.filter((f) => normalizeValue(draft[f.key]) !== normalizeValue(baseline[f.key]));
}

/** buildModelsPatch is the one save body: only dirty fields, trimmed, Default as null. Pin
 *  members of one stage/harness cell share one cross_check_pins entry, in field order. */
export function buildModelsPatch(baseline: ModelsValues, draft: ModelsValues, fields: readonly ModelsField[] = MODELS_FIELDS): ModelSettingsPatch {
  const patch: ModelSettingsPatch = {};
  const pins: ModelSettingsPinPatch[] = [];
  for (const f of dirtyFields(baseline, draft, fields)) {
    const value = normalizeValue(draft[f.key]) || null;
    if ("scalar" in f.target) {
      patch[f.target.scalar] = value;
      continue;
    }
    const { stage, harness, member } = f.target.pin;
    let cell = pins.find((p) => p.stage === stage && p.harness === harness);
    if (!cell) {
      cell = { stage, harness };
      pins.push(cell);
    }
    cell[member] = value;
  }
  if (pins.length > 0) patch.cross_check_pins = pins;
  return patch;
}

export interface ModelsErrorMap {
  /** Message per field key, for errors that name a known field. */
  byKey: Record<string, string>;
  /** Messages that name no known field (an unknown key, a list-shape defect, a bare 400). */
  unmatched: string[];
}

/** mapFieldErrors pins each server error to the cell it names: a scalar by its field, a
 *  cross-check pin by stage/harness/member. The first message for a cell wins; anything that
 *  names no known cell is returned in `unmatched` so it can still be shown. */
export function mapFieldErrors(errors: readonly ModelSettingsFieldError[], fields: readonly ModelsField[] = MODELS_FIELDS): ModelsErrorMap {
  const byKey: Record<string, string> = {};
  const unmatched: string[] = [];
  for (const e of errors) {
    const key = e.field === "cross_check_pins" && e.stage && e.harness && e.member
      ? pinKey(e.stage, e.harness, e.member)
      : e.field;
    const known = fields.some((f) => f.key === key);
    if (!known) unmatched.push(e.message);
    else if (!(key in byKey)) byKey[key] = e.message;
  }
  return { byKey, unmatched };
}
