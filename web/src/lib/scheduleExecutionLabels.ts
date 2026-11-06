// Label-only helpers for the schedule modal's "Where it runs" fieldset. They decide
// what the "Default" options SAY about harness, model and token; none of them feeds a
// request body, the Anthropic-token gate or the model vocabulary. The server resolves
// the real harness when the schedule fires: with no usable default it may pick one by
// weekly limits (automatic, eligible issue runs only), and with an explicit default it may
// overflow to the other harness, so every claim here is worded as a hint, not a promise.

import type { Harness, ScheduleTarget } from "./api";
import type { CredentialMode } from "./credentialOverride";
import { effectiveHarnessIsCodex, type HarnessSelection } from "./harnessSelection";

export interface ExecutionLabelInput {
  target: ScheduleTarget;
  harnessSel: HarnessSelection;
  /** The schedule's model pin; "" (or whitespace) means Default. */
  model: string;
  credentialMode: CredentialMode;
  claudeUsable: boolean;
  codexUsable: boolean;
  ownerDefaultHarness: Harness | null;
  overflowEnabled: boolean;
  defaultClaudeModel?: string | null;
  defaultCodexModel?: string | null;
}

function ownerDefaultUsable(i: ExecutionLabelInput): boolean {
  return (
    (i.ownerDefaultHarness === "claude" && i.claudeUsable) ||
    (i.ownerDefaultHarness === "codex" && i.codexUsable)
  );
}

/**
 * True only when a fired run may be moved to the other harness by the server's
 * creation-time overflow: toggle on, both harnesses usable, an issue-kind target
 * (issue or sweep), and nothing pinned (harness, model, token) with a usable explicit
 * owner default. Label-only: it is never used to build a payload.
 */
export function overflowMayApply(i: ExecutionLabelInput): boolean {
  return (
    i.overflowEnabled &&
    i.claudeUsable &&
    i.codexUsable &&
    issueOriginMayChoose(i) &&
    ownerDefaultUsable(i)
  );
}

// The server's eligibility for both automatic choice and overflow (harnessCreationOrigin
// .eligible): a fire that creates an issue run (issue or sweep target) with no harness,
// model or token pin. Cross-check settings also disqualify it, but the modal does not load
// them, so an eligible-looking form keeps the hedged wording.
function issueOriginMayChoose(i: ExecutionLabelInput): boolean {
  return (
    (i.target === "issue" || i.target === "sweep") &&
    i.harnessSel === "inherit" &&
    i.model.trim() === "" &&
    i.credentialMode === "inherit"
  );
}

/**
 * True when the server may pick the harness by weekly limits (#3845): no usable explicit
 * default, both harnesses usable and an issue-run fire with nothing pinned. Otherwise a
 * both-usable form with no usable default resolves to Claude. Label-only.
 */
export function automaticMayApply(i: ExecutionLabelInput): boolean {
  return !ownerDefaultUsable(i) && i.claudeUsable && i.codexUsable && issueOriginMayChoose(i);
}

/** Text of the harness picker's inherit option. */
export function harnessInheritLabel(i: ExecutionLabelInput): string {
  if (ownerDefaultUsable(i) && i.ownerDefaultHarness) {
    const name = i.ownerDefaultHarness === "claude" ? "Claude" : "Codex";
    if (overflowMayApply(i)) {
      const other = i.ownerDefaultHarness === "claude" ? "Codex" : "Claude";
      return `Default · ${name} (your default; may overflow to ${other} when it fires)`;
    }
    return `Default · ${name} (your default)`;
  }
  if (automaticMayApply(i)) return "Default · automatic (chosen when it fires)";
  if (i.claudeUsable) return "Default · Claude (no usable default set)";
  return "Default · Codex (no usable default set)";
}

/** The harness the model Default option can name, or undefined when unknowable. */
export function modelLabelHarness(input: ExecutionLabelInput): Harness | undefined {
  // The model Default option describes the form with Default chosen, i.e. no model pin,
  // so a model the form currently pins must not make overflow or automatic look impossible.
  const i = { ...input, model: "" };
  if (i.harnessSel !== "inherit") return i.harnessSel;
  // Must precede the Codex check: effectiveHarnessIsCodex answers true for an inherited
  // usable Codex default, which overflow may still move to Claude.
  if (overflowMayApply(i)) return undefined;
  if (effectiveHarnessIsCodex(i.harnessSel, i.claudeUsable, i.codexUsable, i.ownerDefaultHarness)) {
    return "codex";
  }
  if (i.ownerDefaultHarness === "claude" && i.claudeUsable) return "claude";
  if (i.claudeUsable && !automaticMayApply(i)) return "claude";
  return undefined;
}

/** Text of the model select's Default option. */
export function modelDefaultLabel(i: ExecutionLabelInput): string {
  const harness = modelLabelHarness(i);
  if (!harness) return "Default · your worker model";
  const name = harness === "claude" ? "Claude" : "Codex";
  const lane = (harness === "claude" ? i.defaultClaudeModel : i.defaultCodexModel)?.trim();
  return lane
    ? `Default · ${lane} (your ${name} default)`
    : `Default · your ${name} worker model`;
}

/** Text of the auto-rework select's inherit option. */
export function mrReworkInheritLabel(loaded: boolean, mrReworkEnabled: boolean | null | undefined): string {
  if (!loaded) return "Default · your account setting";
  return mrReworkEnabled === false ? "Default · off (your setting)" : "Default · on (your setting)";
}
