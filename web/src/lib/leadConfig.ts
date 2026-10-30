// PRD #3244 section 8: the run-level lead-agent configuration chip.
//
// One display rule, two implementations pinned together by
// fixtures/lead-config/contract.json (`labels`, `display_cases`): this module and the Go
// runpresentation.LeadConfigView. The label tables below are the TS twin of Go
// leadcfg.SourceLabel / EvidenceLabel. Used by the run header only; settings surfaces use the
// model-settings label table, so the same token may read differently there.

import type { LeadPhaseConfig, Run, RunLeadConfig } from "./apiTypes";
import { stripUnsafeChars } from "./safeText";

export interface LeadConfigViewInput {
  status: string;
  harness: string | null | undefined;
  preClaim: { model?: string | null; modelSource: "schedule_pin" };
  leadConfig?: RunLeadConfig | null;
}

export interface LeadConfigView {
  chip: string;
  lines: string[];
}

const MODEL_LABELS: Record<string, string> = {
  run_override: "this run",
  schedule_pin: "fixed by schedule",
  user_planning_default: "your {H} planning default",
  user_default: "your {H} default",
  template: "lead template",
  builtin: "built-in",
};
const MODEL_SDK_DEFAULT_LABEL = "Claude SDK default";
const EFFORT_LABELS: Record<string, string> = {
  run_override: "this run",
  schedule_pin: "",
  user_planning_default: "your {H} planning effort",
  user_default: "your {H} effort",
  template: "agent template",
  builtin: "built-in",
};
const EFFORT_RUNTIME_DEFAULT_LABEL = "runtime default";
const EVIDENCE_LABELS: Record<string, string> = {
  claude_init: "Claude SDK init",
  claude_get_settings: "Claude SDK settings",
  codex_thread_response: "Codex thread config",
  codex_sent: "sent to Codex",
  codex_runtime_default: "Codex thread config",
  unknown: "not reported by the runtime",
};
const NOT_OBSERVED_LABEL = "not reported yet";
const EFFORT_NOT_SENT_LABEL = "not sent";
const OBSERVED_EFFORT_OTHER_LABEL = "an unrecognized level";

const FOOTER = "This is the lead agent; subagents with their own pin may differ.";
const NOT_RECORDED_NOTE =
  "the worker did not record it (an older worker, or the run ended before its first lead turn)";
const EARLIER_ATTEMPT_NOTE = "recorded by an earlier attempt; this attempt has not recorded yet";
const TRUNCATED_NOTE = "(later changes not recorded)";

// Evidence kinds whose observed effort is a runtime report that may differ from the request.
const RUNTIME_EFFORT_EVIDENCE = new Set(["claude_init", "claude_get_settings", "codex_runtime_default"]);

function harnessName(harness: string | null | undefined): string {
  return harness === "codex" ? "Codex" : "Claude";
}

/** TS twin of Go leadcfg.SourceLabel: the run-level label for a model or effort source token. */
export function sourceLabel(
  field: "model" | "effort",
  source: string,
  harness: string | null | undefined,
  opts?: { sdkDefault?: boolean },
): string {
  if (field === "model" && opts?.sdkDefault && source === "builtin") return MODEL_SDK_DEFAULT_LABEL;
  const table = field === "model" ? MODEL_LABELS : EFFORT_LABELS;
  return (table[source] ?? "").replace("{H}", harnessName(harness));
}

/** TS twin of Go leadcfg.EvidenceLabel. `null` is "not yet observed". */
export function evidenceLabel(evidence: string | null | undefined): string {
  if (evidence == null) return NOT_OBSERVED_LABEL;
  return EVIDENCE_LABELS[evidence] ?? EVIDENCE_LABELS.unknown;
}

function concreteObserved(level: string | null): string | null {
  return level != null && level !== "other" ? level : null;
}

function modelText(p: LeadPhaseConfig): string {
  if (p.requested_model) return stripUnsafeChars(p.requested_model);
  if (p.requested_model_source === "builtin" && p.observed_model) return stripUnsafeChars(p.observed_model);
  return "default";
}

function effortText(p: LeadPhaseConfig): string {
  const observed = concreteObserved(p.observed_effort);
  if (p.requested_effort_mode === "runtime_default") {
    return observed ? `${EFFORT_RUNTIME_DEFAULT_LABEL}: ${observed}` : EFFORT_RUNTIME_DEFAULT_LABEL;
  }
  const requested = p.requested_effort;
  if (!requested) return EFFORT_RUNTIME_DEFAULT_LABEL;
  if (
    observed &&
    observed !== requested &&
    p.observed_effort_evidence != null &&
    RUNTIME_EFFORT_EVIDENCE.has(p.observed_effort_evidence)
  ) {
    return `${observed} (requested ${requested})`;
  }
  return requested;
}

function phaseSummary(p: LeadPhaseConfig): string {
  return `${modelText(p)} · ${effortText(p)}`;
}

function runtimeModelLine(p: LeadPhaseConfig): string {
  if (p.observed_model) return `${stripUnsafeChars(p.observed_model)} (${evidenceLabel(p.observed_model_evidence)})`;
  return evidenceLabel(p.observed_model_evidence);
}

function runtimeEffortLine(p: LeadPhaseConfig): string {
  const ev = p.observed_effort_evidence;
  if (p.observed_effort == null) {
    if (ev === "claude_init" || ev === "claude_get_settings") return EFFORT_NOT_SENT_LABEL;
    return evidenceLabel(ev);
  }
  const level = p.observed_effort === "other" ? OBSERVED_EFFORT_OTHER_LABEL : p.observed_effort;
  return `${level} (${evidenceLabel(ev)})`;
}

function phaseLines(p: LeadPhaseConfig, harness: string | null | undefined, prefix: string): string[] {
  const modelLabel = sourceLabel("model", p.requested_model_source, harness, { sdkDefault: !p.requested_model });
  const out = [`${prefix}${modelText(p)}: ${modelLabel}`];
  if (p.requested_effort_mode === "runtime_default") {
    out.push(`${prefix}${EFFORT_RUNTIME_DEFAULT_LABEL}`);
  } else {
    out.push(`${prefix}${p.requested_effort ?? EFFORT_RUNTIME_DEFAULT_LABEL}: ${sourceLabel("effort", p.requested_effort_source, harness)}`);
  }
  out.push(`${prefix}runtime: ${runtimeModelLine(p)}`);
  out.push(`${prefix}effort: ${runtimeEffortLine(p)}`);
  return out;
}

function preClaimLines(input: LeadConfigViewInput): string[] {
  const model = input.preClaim.model;
  if (!model) return [];
  return [`${stripUnsafeChars(model)}: ${sourceLabel("model", input.preClaim.modelSource, input.harness)}`];
}

/** Rules 1-7 of PRD #3244 section 8. An empty chip means no chip. */
export function leadConfigView(input: LeadConfigViewInput): LeadConfigView {
  const lc = input.leadConfig;
  if (!lc || !lc.applicable) return { chip: "", lines: [] };

  const phases = lc.phases ?? [];
  const plan = phases.find((p) => p.phase === "plan");
  const impl = phases.find((p) => p.phase === "implement");
  const rows = [plan, impl].filter((p): p is LeadPhaseConfig => p !== undefined);
  const differ = plan !== undefined && impl !== undefined && phaseSummary(plan) !== phaseSummary(impl);
  const shown = differ ? rows : rows.slice(-1);
  const summary = differ && plan && impl
    ? `plan ${phaseSummary(plan)} → implement ${phaseSummary(impl)}`
    : shown.length > 0
      ? phaseSummary(shown[0])
      : "";

  // Rule 2: pre-claim.
  if (input.status === "queued") {
    const model = input.preClaim.model;
    const chip = model
      ? `lead: ${stripUnsafeChars(model)} · effort resolves at claim`
      : "lead: model and effort resolve at claim";
    const lines = preClaimLines(input);
    if (summary) lines.push(`previous attempt: ${summary}`);
    lines.push(FOOTER);
    return { chip, lines };
  }

  // Rule 3: claimed, lead lane, no rows.
  if (shown.length === 0) {
    return { chip: "lead: not recorded", lines: [NOT_RECORDED_NOTE, ...preClaimLines(input), FOOTER] };
  }

  // Rules 4-7.
  const lines: string[] = [];
  for (const p of shown) {
    lines.push(...phaseLines(p, input.harness, differ ? `${p.phase} ` : ""));
  }
  if (!lc.current) lines.push(EARLIER_ATTEMPT_NOTE);
  if (shown.some((p) => p.truncated)) lines.push(TRUNCATED_NOTE);
  lines.push(FOOTER);
  return { chip: `lead: ${summary}`, lines };
}

/** H's adapter: runs.model_source does not exist yet, so the pre-claim model is run.model, whose
 *  only writer is a schedule pin. */
export function leadConfigViewInput(run: Run): LeadConfigViewInput {
  return {
    status: run.status,
    harness: run.harness,
    preClaim: { model: run.model, modelSource: "schedule_pin" },
    leadConfig: run.lead_config,
  };
}
