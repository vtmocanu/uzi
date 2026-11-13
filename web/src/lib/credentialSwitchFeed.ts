// Feed copy for the run's token-switch messages (PRD #1247 M9, issue #4201).
//
// `credential_switch` is server-written when a claim or an in-place apply spends a different
// token. Its payload is CredentialSwitchMessagePayload; `ordinal` >= 1 marks the nth in-place
// switch inside one claim generation (the run kept its worker and working directory), and is
// absent on a claim-time switch (an older worker released and re-claimed the run).
//
// `credential_switch_uncertain` is posted by the worker itself (through its own message batcher, at a
// seq it owns) when a switch's outcome could not be established: the run is parked as a plain owner
// pause and the owner resumes it. Its copy is fixed; nothing from
// the payload is interpolated, so an unexpected shape cannot change what it says.
//
// The token label is user-authored and goes through sanitizeLabel (React escaping does not
// neutralise a bidi override).

import type { CredentialSwitchMessagePayload } from "./apiTypes";
import { sanitizeLabel } from "./sanitizeLabel";

/** The feed line for a token switch whose outcome could not be established. */
export const CREDENTIAL_SWITCH_UNCERTAIN_TEXT =
  "Could not confirm the token switch. The run is paused until you resume it.";

// selectReasonText maps the server's select_reason enum to short human copy; an unknown
// or null reason falls back to the raw value (or nothing), never invents meaning.
export function selectReasonText(reason: string | null | undefined): string {
  switch (reason) {
    case "override_pinned":
      return "pinned by override";
    case "override_auto":
      return "auto-selected by override";
    case "override_default":
      return "default by override";
    case "auto":
      return "auto-selected";
    case "default":
      return "default token";
    case "pinned":
      return "pinned token";
    case "codex_binding":
      return "Codex binding";
    default:
      return reason ?? "";
  }
}

function asPayload(v: unknown): Partial<CredentialSwitchMessagePayload> {
  return v && typeof v === "object" ? (v as Partial<CredentialSwitchMessagePayload>) : {};
}

/** True when a credential_switch payload names an in-place switch (ordinal >= 1). */
export function isInPlaceSwitch(payload: unknown): boolean {
  const ordinal = asPayload(payload).ordinal;
  return typeof ordinal === "number" && Number.isInteger(ordinal) && ordinal >= 1;
}

/** describeCredentialSwitch renders a credential_switch payload as one feed line, e.g.
 *  `Switched in place to “console-key” (pinned by override)`. */
export function describeCredentialSwitch(payload: unknown): string {
  const p = asPayload(payload);
  const label = typeof p.label === "string" ? sanitizeLabel(p.label).trim() : "";
  const token = label ? `“${label}”` : "an unlabelled token";
  const reason = selectReasonText(typeof p.select_reason === "string" ? p.select_reason : null);
  const head = isInPlaceSwitch(p) ? `Switched in place to ${token}` : `Switched to ${token}`;
  return reason ? `${head} (${reason})` : head;
}
