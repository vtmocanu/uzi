// Read-only run-start preview helpers (issue #3261). Pure: the request a surface sends to
// POST /api/me/effective-settings/start-preview, its dedup key, and the render model that
// joins the response with the GET /api/me/effective-settings defaults. The server decides
// every value and every source; nothing here applies precedence or credential logic, and
// no label ever claims a value is "your default" unless the server's own source token says
// so (modelSettingSourceLabel owns the wording).

import type {
  Credential,
  EffectiveEffortCell,
  EffectiveModelCell,
  EffectiveSettings,
  EffortSettingCell,
  ModelSettingCell,
  StartPreview,
  StartPreviewAlternative,
  StartPreviewRequest,
} from "./apiTypes";
import { codexRuntimeDefaultLabel, modelSettingSourceLabel } from "./modelSettings";
import { stripUnsafeChars } from "./safeText";
import { runStartBody, type RunStartFacts, type RunStartOptions } from "./startRun";

type Refusal = NonNullable<StartPreview["refusal"]>;

// startRefusalLabel is the closed map from a server refusal token to user-facing text. It is
// a Record over the contract's union, so a new server token fails the typecheck here.
const REFUSAL_LABELS: Record<Refusal, string> = {
  no_credential_for_harness: "No usable credential for this harness",
  no_usable_credential: "No usable credential",
  credential_disabled: "The chosen token is disabled",
  credential_not_found: "The chosen token was not found",
  credential_override_harness_unsupported: "A token choice does not apply to this harness",
  credential_override_invalid: "The token choice is not valid",
  model_incompatible_with_harness: "This model does not run on this harness",
  provider_source_unusable: "The model provider is not usable",
  provider_lane_unsupported: "The model provider does not support this run",
  provider_model_unmapped: "The model provider has no mapping for this model",
  provider_binding_stale: "The model provider binding is out of date",
  provider_destination_not_allowed: "The model provider destination is not allowed",
  credential_override_provider_source: "A token choice does not apply to this model provider",
};

export function startRefusalLabel(refusal: Refusal): string {
  return REFUSAL_LABELS[refusal] ?? "Refused";
}

// startPreviewRequest builds the request FROM runStartBody, so a preview submits exactly
// what Start would send: hidden rows and Default (empty) model/effort never reach it.
export function startPreviewRequest(options: RunStartOptions, facts: RunStartFacts): StartPreviewRequest {
  const body = runStartBody(options, facts);
  const req: StartPreviewRequest = {
    harness: body.harness ?? null,
    credential_override: body.credential_override ?? null,
  };
  if (body.model !== undefined) req.model = body.model;
  if (body.effort !== undefined) req.effort = body.effort;
  return req;
}

// The one-click Start choice: every row on Default.
export const DEFAULT_START_PREVIEW_REQUEST: StartPreviewRequest = { harness: null, credential_override: null };

// startPreviewKey is the dedup key of a choice, secret_id included.
export function startPreviewKey(req: StartPreviewRequest): string {
  return JSON.stringify([
    req.harness ?? null,
    req.credential_override?.mode ?? null,
    req.credential_override?.secret_id ?? null,
    req.model ?? null,
    req.effort ?? null,
  ]);
}

export interface ValueView {
  /** Full text, e.g. "opus" or the Codex runtime-default sentence. */
  text: string;
  /** Short text for the one-line summary. */
  short: string;
  /** The server's source token as words, null when no cell was available. */
  source: string | null;
}

export interface BranchView {
  harness: string;
  refusal: string | null;
  credential: string | null;
  model: ValueView | null;
  effort: ValueView | null;
}

export interface StartPreviewViewModel {
  /** The decided harness name, null when chosen at creation or refused before a harness. */
  harness: string | null;
  harnessSource: string | null;
  /** True when the harness is picked at creation (alternatives apply). */
  chosenAtCreate: boolean;
  /** Notes the server attached to the harness, as words. */
  notes: string[];
  refusal: string | null;
  credential: string | null;
  model: ValueView | null;
  effort: ValueView | null;
  alternatives: BranchView[];
  /**
   * Whether any shown cell is fixed at claim (a prediction), and whether the request itself
   * carried a choice (override), as opposed to a default that merely happens to be fixed at create.
   */
  fixed: FixedView;
}

export interface FixedView {
  claim: boolean;
  override: boolean;
}

const UNKNOWN: ValueView = { text: "unknown", short: "unknown", source: null };

function modelCell(effective: EffectiveSettings | null, harness: string | null): EffectiveModelCell | undefined {
  if (!effective || !harness) return undefined;
  return effective.models.find((c) => c.setting === "worker_model" && c.harness === harness);
}

function effortCell(effective: EffectiveSettings | null, harness: string | null): EffectiveEffortCell | undefined {
  if (!effective || !harness) return undefined;
  return effective.efforts.find((c) => c.setting === "worker_effort" && c.harness === harness);
}

function modelValue(cell: ModelSettingCell | undefined): ValueView {
  if (!cell) return UNKNOWN;
  // "" means the runtime decides (note sdk_default).
  const text =
    cell.resolved_model === "" ? modelSettingSourceLabel("note", "sdk_default") : stripUnsafeChars(cell.resolved_model);
  return { text, short: text, source: modelSettingSourceLabel("model", cell.source) };
}

function effortValue(cell: EffortSettingCell | undefined): ValueView {
  if (!cell) return UNKNOWN;
  const source = modelSettingSourceLabel("effort", cell.source);
  if (cell.mode === "runtime_default" || cell.resolved_effort === null) {
    const short = "Runtime default";
    const text = cell.harness === "codex" ? codexRuntimeDefaultLabel(cell) : short;
    return { text, short, source };
  }
  return { text: cell.resolved_effort, short: cell.resolved_effort, source };
}

function credentialLabel(c: Credential | null): string | null {
  return c ? modelSettingSourceLabel("credential", c.source) : null;
}

function noteWords(notes: string[]): string[] {
  return notes.map((n) => modelSettingSourceLabel("note", n));
}

function fixedFor(
  fixed: FixedView,
  credential: Credential | null,
  model: { fixed_at?: string; source: string } | undefined,
  effort: { fixed_at?: string; source: string } | undefined,
): void {
  const add = (at: string | undefined, source?: string) => {
    if (source === "run_override" || source === "token_override") fixed.override = true;
    if (source !== "run_override" && at === "claim") fixed.claim = true;
  };
  if (credential) add(credential.fixed_at, credential.source);
  if (model) add(model.fixed_at, model.source);
  if (effort) add(effort.fixed_at, effort.source);
}

function branchView(
  alt: StartPreviewAlternative,
  effective: EffectiveSettings | null,
  fixed: FixedView,
): BranchView {
  if (alt.refusal) {
    return { harness: alt.harness, refusal: startRefusalLabel(alt.refusal), credential: null, model: null, effort: null };
  }
  // The type documents that an alternative carries no worker_model: it is always the GET cell.
  const m = modelCell(effective, alt.harness);
  const e = alt.worker_effort ?? effortCell(effective, alt.harness);
  fixedFor(fixed, alt.credential, m, e);
  return {
    harness: alt.harness,
    refusal: null,
    credential: credentialLabel(alt.credential),
    model: modelValue(m),
    effort: effortValue(e),
  };
}

// startPreviewView joins a start-preview response with the GET defaults. Model and effort come
// from the response's override cells when present, else from the GET cell of the same harness.
// A GET failure (effective null) keeps the response's harness and credential and reads the
// defaults as unknown.
export function startPreviewView(preview: StartPreview, effective: EffectiveSettings | null): StartPreviewViewModel {
  const hv = preview.harness;
  // An explicit harness choice is fixed on the run now; every harness value reports
  // fixed_at "create", so only the run_override source marks a choice made here.
  const explicitHarness = hv?.source === "run_override";
  const fixed: FixedView = { claim: false, override: explicitHarness };
  const base = {
    harness: hv?.value ?? null,
    harnessSource: hv ? modelSettingSourceLabel("harness", hv.source) : null,
    chosenAtCreate: hv !== null && hv.value === null,
    notes: hv ? noteWords(hv.notes) : [],
    refusal: preview.refusal ? startRefusalLabel(preview.refusal) : null,
    credential: credentialLabel(preview.credential),
    model: null as ValueView | null,
    effort: null as ValueView | null,
    alternatives: [] as BranchView[],
    fixed,
  };
  // A top-level refusal renders the refusal and the server's credential: no model or effort rows.
  if (preview.refusal) {
    fixedFor(fixed, preview.credential, undefined, undefined);
    return base;
  }
  if (base.chosenAtCreate) {
    base.alternatives = (preview.alternatives ?? []).map((a) => branchView(a, effective, fixed));
    return base;
  }
  const m = preview.worker_model ?? modelCell(effective, base.harness);
  const e = preview.worker_effort ?? effortCell(effective, base.harness);
  fixedFor(fixed, preview.credential, m, e);
  base.model = modelValue(m);
  base.effort = effortValue(e);
  return base;
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// compactSummary is the one-line text: "Claude · opus · xhigh", "Chosen when started", or the
// refusal.
export function compactSummary(view: StartPreviewViewModel): string {
  if (view.refusal) return view.refusal;
  if (view.chosenAtCreate || view.harness === null) return "Chosen when started";
  return [capitalize(view.harness), view.model?.short ?? "unknown", view.effort?.short ?? "unknown"].join(" · ");
}

export interface DefaultChoiceLabels {
  harness: string;
  model: string;
  effort: string;
}

// defaultChoiceLabels words the three "Default" options of the Run options dialog from server
// data only. The harness option describes the DEFAULT-choice prediction (the default-choice
// preview, else the GET start entry for choice "default"), whatever harness is selected now;
// the model and effort options describe the harness the CURRENT selection decided on
// (selectedPreview), using that harness's GET cells.
export function defaultChoiceLabels(
  effective: EffectiveSettings | null,
  defaultPreview: StartPreview | null,
  selectedPreview: StartPreview | null,
): DefaultChoiceLabels {
  const hv = defaultPreview
    ? defaultPreview.harness
    : (effective?.start.find((c) => c.choice === "default")?.harness ?? null);
  let harness = "Default";
  if (hv) {
    harness =
      hv.value === null
        ? "Default · chosen when started"
        : `Default · ${capitalize(hv.value)} (${modelSettingSourceLabel("harness", hv.source)})`;
  }

  let model = "Default";
  let effort = "Default";
  const sel = selectedPreview?.harness;
  if (sel && sel.value === null) {
    model = effort = "Default · depends on the harness chosen when started";
  } else if (sel && sel.value !== null && effective) {
    const m = modelCell(effective, sel.value);
    const e = effortCell(effective, sel.value);
    if (m) {
      const v = modelValue(m);
      model = `Default · ${v.text} (${v.source})`;
    }
    if (e) {
      const v = effortValue(e);
      effort = `Default · ${v.text} (${v.source})`;
    }
  }
  return { harness, model, effort };
}
