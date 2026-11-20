// Read-only schedule preview helpers (issue #3260). Pure: the request the schedule modal sends to
// POST /api/me/effective-settings/schedule-preview, its dedup key, and the render model of the
// response. The server decides every value, source and timing; nothing here applies precedence,
// so there is no second resolver in the browser. Wording of source tokens is owned by
// modelSettingSourceLabel.

import type {
  Credential,
  EffectiveEffortCell,
  EffectiveModelCell,
  EffectiveSubagents,
  Harness,
  ScheduleAlternative,
  ScheduleSettingsPreview,
  ScheduleSettingsPreviewRequest,
  StartPreviewCredentialOverride,
} from "./apiTypes";
import { codexRuntimeDefaultLabel, modelSettingSourceLabel } from "./modelSettings";
import { stripUnsafeChars } from "./safeText";
import { REFUSAL_LABELS } from "./startPreview";

type ScheduleRefusal = NonNullable<ScheduleSettingsPreview["refusal"]>;

// Closed map from a server refusal token to user-facing text. A Record over the contract's
// union, so a new server token fails the typecheck here.
const SCHEDULE_REFUSAL_LABELS: Record<ScheduleRefusal, string> = {
  ...REFUSAL_LABELS,
  invalid_model: "This model is not valid for a schedule",
  credential_override_lane_not_switchable: "This kind of run cannot use a different token",
  codex_override_conflict: "The token choice conflicts with the Codex credential",
  codex_pool_unavailable: "No Codex credential is available right now",
};

export function scheduleRefusalLabel(refusal: ScheduleRefusal): string {
  return SCHEDULE_REFUSAL_LABELS[refusal] ?? "Refused";
}

export interface SchedulePreviewDraft {
  /** Set when editing an existing schedule. */
  scheduleId?: string;
  target: string;
  /** The modal's harness patch: undefined untouched, null cleared, else a pin. */
  harnessPatch: Harness | null | undefined;
  /** The modal's credential patch: undefined untouched (always so for self_improve). */
  credentialPatch: StartPreviewCredentialOverride | undefined;
  /** The raw model input; blank means no pin. */
  model: string;
  overrideSubagentModel: boolean;
}

// schedulePreviewRequest mirrors what saving the schedule would send: model and
// override_subagent_model always, harness and credential_override only when the patch is
// defined (omitted means "as stored" when editing).
export function schedulePreviewRequest(draft: SchedulePreviewDraft): ScheduleSettingsPreviewRequest {
  const req: ScheduleSettingsPreviewRequest = { target: draft.target };
  if (draft.scheduleId) req.schedule_id = draft.scheduleId;
  if (draft.harnessPatch !== undefined) req.harness = draft.harnessPatch;
  if (draft.credentialPatch !== undefined && draft.target !== "self_improve") {
    req.credential_override = draft.credentialPatch;
  }
  req.model = draft.model.trim() === "" ? null : draft.model;
  req.override_subagent_model = draft.overrideSubagentModel;
  return req;
}

// schedulePreviewKey is the dedup key of a request. Omitted and null are distinct for harness
// and credential_override.
export function schedulePreviewKey(req: ScheduleSettingsPreviewRequest): string {
  const tri = <T,>(v: T | null | undefined, f: (x: T) => unknown): unknown =>
    v === undefined ? ["omit"] : v === null ? ["null"] : ["set", f(v)];
  return JSON.stringify([
    req.schedule_id ?? null,
    req.target ?? null,
    tri(req.harness, (h) => h),
    tri(req.credential_override, (c) => [c.mode, c.secret_id ?? null]),
    req.model ?? null,
    req.override_subagent_model ?? null,
  ]);
}

export interface ValueRow {
  value: string;
  /** The server's source token as words. */
  source: string | null;
  /** Whether the value is pinned or predicted, and when it is fixed. */
  timing: string;
}

export interface HarnessRow {
  /** Capitalized decided harness, null when chosen at creation. */
  value: string | null;
  chosenAtCreate: boolean;
  source: string;
  timing: string;
}

export interface SubagentsView {
  mode: string;
  lead: string;
  pins: string[];
  more: boolean;
  notes: string[];
}

export interface IgnoredPinView {
  pin: string;
  harness: string | null;
  fallback: string;
  fallbackSource: string | null;
}

export interface ScheduleBranchView {
  harness: string;
  refusal: string | null;
  model: ValueRow | null;
  effort: ValueRow | null;
  credential: ValueRow | null;
  subagents: SubagentsView | null;
}

export interface SchedulePreviewView {
  harness: HarnessRow | null;
  model: ValueRow | null;
  effort: ValueRow | null;
  credential: ValueRow | null;
  subagents: SubagentsView | null;
  notes: string[];
  refusal: string | null;
  ignoredPin: IgnoredPinView | null;
  alternatives: ScheduleBranchView[];
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function timingFor(source: string, fixedAt: string): string {
  if (source === "schedule_pin") return "pinned · fixed when it fires";
  if (fixedAt === "create") return "predicted · fixed when it fires";
  if (fixedAt === "claim") return "predicted · read when a worker picks the run up";
  return "predicted";
}

function noteWords(notes: string[]): string[] {
  return notes.map((n) => modelSettingSourceLabel("note", n));
}

function modelText(cell: EffectiveModelCell): string {
  return cell.resolved_model === ""
    ? modelSettingSourceLabel("note", "sdk_default")
    : stripUnsafeChars(cell.resolved_model);
}

function modelRow(cell: EffectiveModelCell | null): ValueRow | null {
  if (!cell) return null;
  return {
    value: modelText(cell),
    source: modelSettingSourceLabel("model", cell.source),
    timing: timingFor(cell.source, cell.fixed_at),
  };
}

function effortRow(cell: EffectiveEffortCell | null): ValueRow | null {
  if (!cell) return null;
  let value: string;
  if (cell.mode === "runtime_default" || cell.resolved_effort === null) {
    value = cell.harness === "codex" ? codexRuntimeDefaultLabel(cell) : "Runtime default";
  } else {
    value = cell.resolved_effort;
  }
  return {
    value,
    source: modelSettingSourceLabel("effort", cell.source),
    timing: timingFor(cell.source, cell.fixed_at),
  };
}

function credentialRow(c: Credential | null): ValueRow | null {
  if (!c) return null;
  const kind = c.kind === "anthropic" ? "Anthropic" : c.kind === "codex" ? "Codex" : capitalize(c.kind);
  const mode = c.auth_mode === "subscription" ? " · subscription" : c.auth_mode === "api_key" ? " · API key" : "";
  return {
    value: `${kind}${mode}`,
    source: modelSettingSourceLabel("credential", c.source),
    timing: timingFor(c.source, c.fixed_at),
  };
}

function subagentsView(s: EffectiveSubagents | null): SubagentsView | null {
  if (!s) return null;
  return {
    mode: s.mode === "follow_lead" ? "follow the lead" : "own pins, else the lead",
    lead: s.lead_value === "" ? modelSettingSourceLabel("note", "sdk_default") : stripUnsafeChars(s.lead_value),
    pins: s.pinned.map((p) => `${stripUnsafeChars(p.name)} → ${stripUnsafeChars(p.model)}`),
    more: s.pinned_truncated,
    notes: noteWords(s.notes),
  };
}

function branchView(a: ScheduleAlternative): ScheduleBranchView {
  const harness = capitalize(a.harness);
  if (a.refusal) {
    return { harness, refusal: scheduleRefusalLabel(a.refusal), model: null, effort: null, credential: null, subagents: null };
  }
  return {
    harness,
    refusal: null,
    model: modelRow(a.worker_model),
    effort: effortRow(a.worker_effort),
    credential: credentialRow(a.credential),
    subagents: subagentsView(a.subagents),
  };
}

// schedulePreviewView turns the response into rows. Every value, source and timing is read from
// the response.
export function schedulePreviewView(preview: ScheduleSettingsPreview): SchedulePreviewView {
  const hv = preview.harness;
  const harness: HarnessRow | null = hv
    ? {
        value: hv.value === null ? null : capitalize(hv.value),
        chosenAtCreate: hv.value === null,
        source: modelSettingSourceLabel("harness", hv.source),
        timing: timingFor(hv.source, hv.fixed_at),
      }
    : null;
  const model = modelRow(preview.worker_model);
  const ignored = preview.worker_model?.ignored ?? null;
  const ignoredPin: IgnoredPinView | null =
    ignored && preview.worker_model
      ? {
          pin: stripUnsafeChars(ignored.value),
          harness: hv?.value ?? null,
          fallback: model?.value ?? "",
          fallbackSource: model?.source ?? null,
        }
      : null;
  return {
    harness,
    model,
    effort: effortRow(preview.worker_effort),
    credential: credentialRow(preview.credential),
    subagents: subagentsView(preview.subagents),
    notes: noteWords([...(hv?.notes ?? []), ...preview.notes]),
    refusal: preview.refusal ? scheduleRefusalLabel(preview.refusal) : null,
    ignoredPin,
    alternatives: (preview.alternatives ?? []).map(branchView),
  };
}

// defaultModelChipSuffix is the prediction appended to the "Default" model option, or null when
// there is nothing to say (refused, or no model to show).
export function defaultModelChipSuffix(preview: ScheduleSettingsPreview): string | null {
  if (preview.refusal || !preview.harness) return null;
  if (preview.harness.value !== null) {
    const model = modelRow(preview.worker_model);
    if (!model) return null;
    return `now ${model.value} on ${capitalize(preview.harness.value)} (predicted)`;
  }
  const parts: string[] = [];
  for (const a of preview.alternatives ?? []) {
    if (a.refusal) continue;
    const model = modelRow(a.worker_model);
    if (model) parts.push(`${model.value} on ${capitalize(a.harness)}`);
  }
  if (parts.length === 0) return null;
  return `now ${parts.join(" or ")} (predicted, harness chosen when it fires)`;
}
