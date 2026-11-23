// The Models tab's draft model (PRD #3263, module C): one stored baseline, one draft, the
// semantic dirty set between them, the one PUT /me/model-settings patch that saves it, and the
// map from a 400's field errors back to the cells that caused them. Pure and React-free, so the
// page owns state and this file owns meaning.
//
// A field is a DESCRIPTOR, not a hand-written branch: the harness, worker and planning rows
// (M1), then judge routing and models, summary models, task-review models and efforts and the
// four cross-check pin cells (M2). A pin member is keyed
// `cross_check_pins:<stage>/<harness>/<member>`, so the patch builder groups members of one cell
// and the error map finds a pin error by stage/harness/member.
//
// Values are strings: "" is Default (a cleared override, sent as null), anything else is the
// override, compared and sent trimmed. "runtime" is an ordinary non-empty value, so Runtime
// default stays distinct from Default. Predictions are not part of the draft and never count.
//
// A field may carry a local `validate` that mirrors its server validator (the Claude worker,
// planning and helper lanes refuse Codex ids, the Codex worker and planning lanes refuse a Claude
// alias, a checker refuses the other family's model): a DIRTY value it flags blocks Save on its
// own cell; a STORED value it flags is shown on the cell as information only.
// The server stays authoritative; this only spares a round trip.

import type { Harness, ModelSettingsFieldError, ModelSettingsPatch, ModelSettingsPinPatch, UserSettings } from "./apiTypes";
import { modelFieldWarning } from "./agentTemplates";
import { CODEX_ROLE_MODELS } from "./codexModels";
import { checkerModelWarning, normalizeCheckerValue } from "./crossCheckSettings";

type ScalarKey = Exclude<keyof ModelSettingsPatch, "cross_check_pins">;
type PinStage = ModelSettingsPinPatch["stage"];
type PinMember = "model" | "effort";

/** "harness" is the default harness, "routing" the judge's Runs on choice. */
export type ModelsFieldKind = "harness" | "routing" | "model" | "effort";

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
  /** The semantic form of a value (trimmed by default; a checker pin trims Unicode White_Space
   *  as the server's strings.TrimSpace does). */
  normalize?: (value: string) => string;
  /** A local problem with a (non-empty) value, or "" when it is fine. */
  validate?: (value: string) => string;
}

export type ModelsValues = Record<string, string>;

function scalar(key: ScalarKey, row: string, family: Harness | null, kind: ModelsFieldKind, validate?: (value: string) => string): ModelsField {
  return {
    key,
    row,
    family,
    kind,
    read: (s) => (s as unknown as Record<string, string | null | undefined>)[key],
    target: { scalar: key },
    validate,
  };
}

// The Claude helper lanes (judge, summary, task review) mirror ValidateClaudeLaneModel: the
// shared model-ID syntax rules, then no Codex role id. The copy follows Run defaults'.
function claudeHelperModel(skips: string): (value: string) => string {
  return (value) => {
    const syntax = modelFieldWarning(value);
    if (syntax) return syntax;
    const m = value.trim();
    return CODEX_ROLE_MODELS.includes(m) ? `Not a Claude model: ${m} is a Codex model, so ${skips}. Choose Default to clear it.` : "";
  };
}

// The worker and planning lanes mirror their server decoders. A Claude lane uses the shared
// model rules (validateModel), then refuses a Codex role id (rejectCodexIDInClaudeLane, or
// validateClaudeJudgeModel for planning). A Codex lane uses the shared rules, then refuses a
// known Claude alias (rejectClaudeAliasInCodexLane). Custom ids pass both lanes.
function claudeLaneModel(effect: string, clear: string): (value: string) => string {
  return (value) => {
    const syntax = modelFieldWarning(value);
    if (syntax) return syntax;
    const m = value.trim();
    return CODEX_ROLE_MODELS.includes(m) ? `Not a Claude model: ${m} is a Codex model, so ${effect}. ${clear}` : "";
  };
}

// KNOWN_CLAUDE_ALIASES is the server's closed knownClaudeAliases set (agenttmpl.KnownClaudeAliases).
// A full Claude id such as claude-opus-4-8 is not in it and passes, as on the server.
const KNOWN_CLAUDE_ALIASES: readonly string[] = ["opus", "sonnet", "haiku", "fable"];

function codexLaneModel(clear: string): (value: string) => string {
  return (value) => {
    const syntax = modelFieldWarning(value);
    if (syntax) return syntax;
    const m = value.trim();
    return KNOWN_CLAUDE_ALIASES.includes(m) ? `Not a Codex model: ${m} is a Claude model alias, so the Codex lane cannot use it. ${clear}` : "";
  };
}

function pinKey(stage: string, harness: string, member: string): string {
  return `cross_check_pins:${stage}/${harness}/${member}`;
}

/** pinField describes one member of a cross-check pin cell. A model member keeps the
 *  checker's own family guard (custom IDs stay valid); efforts are a closed list. */
export function pinField(row: string, stage: PinStage, harness: Harness, member: PinMember): ModelsField {
  return {
    key: pinKey(stage, harness, member),
    row,
    family: harness,
    kind: member,
    read: (s) => s.cross_check_pins?.find((p) => p.stage === stage && p.harness === harness)?.[member],
    target: { pin: { stage, harness, member } },
    normalize: normalizeCheckerValue,
    validate: member === "model" ? (value) => checkerModelWarning(value, harness) : undefined,
  };
}

/** Every Models cell, in display order. */
export const MODELS_FIELDS: readonly ModelsField[] = [
  scalar("default_harness", "Default harness", null, "harness"),
  scalar("default_claude_model", "Worker model", "claude", "model", claudeLaneModel("Claude runs skip it", "Choose Default to clear it.")),
  scalar("default_codex_model", "Worker model", "codex", "model", codexLaneModel("Choose Default to clear it.")),
  scalar("default_effort", "Worker effort", "claude", "effort"),
  scalar("default_codex_effort", "Worker effort", "codex", "effort"),
  scalar("planning_claude_model", "Planning model", "claude", "model", claudeLaneModel("Claude planning skips it", "Choose Same as the run to clear it.")),
  scalar("planning_codex_model", "Planning model", "codex", "model", codexLaneModel("Choose Same as the run to clear it.")),
  scalar("planning_claude_effort", "Planning effort", "claude", "effort"),
  scalar("planning_codex_effort", "Planning effort", "codex", "effort"),
  scalar("judge_runs_on", "Judge runs on", null, "routing"),
  scalar("judge_model", "Judge model", "claude", "model", claudeHelperModel("Claude judges skip it")),
  scalar("codex_judge_model", "Judge model", "codex", "model", modelFieldWarning),
  scalar("summary_model", "Summary model", "claude", "model", claudeHelperModel("Claude summaries skip it")),
  scalar("codex_summary_model", "Summary model", "codex", "model"),
  scalar("task_review_model", "Task review model", "claude", "model", claudeHelperModel("the server refuses this save")),
  scalar("codex_task_review_model", "Task review model", "codex", "model"),
  scalar("task_review_effort", "Task review effort", "claude", "effort"),
  scalar("codex_task_review_effort", "Task review effort", "codex", "effort"),
  pinField("Plan cross-check model", "plan", "claude", "model"),
  pinField("Plan cross-check model", "plan", "codex", "model"),
  pinField("Plan cross-check effort", "plan", "claude", "effort"),
  pinField("Plan cross-check effort", "plan", "codex", "effort"),
  pinField("Code cross-check model", "code", "claude", "model"),
  pinField("Code cross-check model", "code", "codex", "model"),
  pinField("Code cross-check effort", "code", "claude", "effort"),
  pinField("Code cross-check effort", "code", "codex", "effort"),
];

/** normalizeValue is the semantic form of a value: trimmed, with null/undefined as "" (Default).
 *  Pass the field to use its own normalization (a checker pin's Unicode trim). */
export function normalizeValue(value: string | null | undefined, field?: ModelsField): string {
  const v = value ?? "";
  if (field?.kind === "model" && "scalar" in field.target && /\p{Cf}/u.test(v)) return v;
  return field?.normalize ? field.normalize(v) : v.trim();
}

/** fieldLabel is a field's accessible name, "<Row> — <Family>" (just the row without a family). */
export function fieldLabel(field: ModelsField): string {
  if (!field.family) return field.row;
  return `${field.row} — ${field.family === "claude" ? "Claude" : "Codex"}`;
}

/** baselineFromSettings reads every field's stored value, normalized. */
export function baselineFromSettings(settings: UserSettings, fields: readonly ModelsField[] = MODELS_FIELDS): ModelsValues {
  const values: ModelsValues = {};
  for (const f of fields) values[f.key] = normalizeValue(f.read(settings), f);
  return values;
}

/** dirtyFields lists, in field order, every field whose draft differs semantically from the baseline. */
export function dirtyFields(baseline: ModelsValues, draft: ModelsValues, fields: readonly ModelsField[] = MODELS_FIELDS, reaffirmed?: ReadonlySet<string>): ModelsField[] {
  return fields.filter((f) => reaffirmed?.has(f.key) || normalizeValue(draft[f.key], f) !== normalizeValue(baseline[f.key], f));
}

/** localFieldWarnings maps each DIRTY field whose value its own validator refuses to that
 *  warning. A stored value is never blocked here (storedFieldWarnings reports it). Dimmed cells are
 *  validated like any other. */
export function localFieldWarnings(baseline: ModelsValues, draft: ModelsValues, fields: readonly ModelsField[] = MODELS_FIELDS, reaffirmed?: ReadonlySet<string>): Record<string, string> {
  const warnings: Record<string, string> = {};
  for (const f of dirtyFields(baseline, draft, fields, reaffirmed)) {
    const value = normalizeValue(draft[f.key], f);
    const warning = f.kind === "model" && "scalar" in f.target && /\p{Cf}/u.test(value)
      ? "Model contains a format character; remove it before saving."
      : value === "" || !f.validate ? "" : f.validate(draft[f.key] ?? "");
    if (warning) warnings[f.key] = warning;
  }
  return warnings;
}

/** storedFieldWarnings maps each UNCHANGED field whose stored value its own validator refuses
 *  (a legacy Codex id in a Claude helper lane, the other family's model in a checker pin) to
 *  that warning, prefixed so it reads as a fact about what is saved. It is informational: it
 *  never blocks Save, since the user did not make that value and may be saving something else. */
export function storedFieldWarnings(baseline: ModelsValues, draft: ModelsValues, fields: readonly ModelsField[] = MODELS_FIELDS, reaffirmed?: ReadonlySet<string>): Record<string, string> {
  const dirty = new Set(dirtyFields(baseline, draft, fields, reaffirmed).map((f) => f.key));
  const warnings: Record<string, string> = {};
  for (const f of fields) {
    if (dirty.has(f.key) || !f.validate) continue;
    const stored = baseline[f.key] ?? "";
    const warning = normalizeValue(stored, f) === "" ? "" : f.validate(stored);
    if (warning) warnings[f.key] = `The saved value is not valid here. ${warning}`;
  }
  return warnings;
}

/** emptyCustomFields lists every model field whose picker is in "Other (custom model ID)" mode
 *  with an empty box, dirty or not: saving would send Default while the picker still shows Other,
 *  so the page blocks Save on any of them. */
export function emptyCustomFields(draft: ModelsValues, customMode: Readonly<Record<string, boolean>>, fields: readonly ModelsField[] = MODELS_FIELDS): ModelsField[] {
  return fields.filter((f) => f.kind === "model" && customMode[f.key] === true && normalizeValue(draft[f.key], f) === "");
}

/** buildModelsPatch is the one save body: only dirty fields, trimmed, Default as null. Pin
 *  members of one stage/harness cell share one cross_check_pins entry, in field order. */
export function buildModelsPatch(baseline: ModelsValues, draft: ModelsValues, fields: readonly ModelsField[] = MODELS_FIELDS, reaffirmed?: ReadonlySet<string>): ModelSettingsPatch {
  const patch: ModelSettingsPatch = {};
  const pins: ModelSettingsPinPatch[] = [];
  for (const f of dirtyFields(baseline, draft, fields, reaffirmed)) {
    const value = normalizeValue(draft[f.key], f) || null;
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
