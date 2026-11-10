// Issue #3607: the owner-facing wording for a Codex policy hold. A verbatim copy of
// fixtures/policy-hold/wording.json, the single wording table the Go CLI also pins to;
// policyHoldWording.test.ts fails if the two drift. Edit the fixture first, then this.
import type { PolicyHoldResumeMode } from "./apiTypes";

export const POLICY_HOLD_WORDING = {
  causes: {
    refused: "The provider refused a request under its usage policy.",
    attempt_path: "This pickup cannot continue the old conversation on this lane; continuing needs a fresh conversation from the saved work.",
    saved_work_missing: "The required saved work was not found where this pickup ran; nothing was started.",
    session_missing: "The saved conversation is not available where this pickup ran; nothing was started.",
    transcript_unknown: "The saved conversation's transcript could not be confirmed where this pickup ran; nothing was started.",
    provider_resume_rejected: "The provider rejected resuming the saved conversation.",
    pending_conversion: "An earlier refusal recorded on the worker was converted into this hold.",
    fresh_start_uncertain: "A one-claim fresh start was used but never confirmed; a provider conversation may already exist.",
  },
  unknown_cause: "held (cause not recorded)",
  modes: {
    preserve: {
      label: "Resume the preserved conversation",
      consequence: "Continues the same conversation with your guidance. If the conversation is missing or rejected, the run is held again; it never starts a new conversation.",
    },
    fresh_from_saved_work: {
      label: "Fresh conversation from the saved work",
      consequence: "Starts a fresh conversation from the saved work and resolves this refusal. Any earlier requirement to preserve the old conversation ends for the rest of the run, until a new refusal.",
    },
    fresh_one_claim: {
      label: "Fresh conversation for one pickup",
      consequence: "Lets exactly the next pickup start one new conversation from the saved work. Later pickups preserve that new conversation again; a fresh start that is never confirmed is held, not repeated.",
    },
  },
} as const satisfies {
  causes: Record<string, string>;
  unknown_cause: string;
  modes: Record<PolicyHoldResumeMode, { label: string; consequence: string }>;
};

type Cause = keyof typeof POLICY_HOLD_WORDING.causes;

/** The cause sentence, or unknown_cause for a null, absent or unrecognized cause. */
export function policyHoldCauseText(cause: string | null | undefined): string {
  const causes: Record<string, string> = POLICY_HOLD_WORDING.causes;
  return cause != null && Object.prototype.hasOwnProperty.call(causes, cause) ? causes[cause as Cause] : POLICY_HOLD_WORDING.unknown_cause;
}

/** Narrows a server mode string to one this client has wording for; unknown modes get no action. */
export function isPolicyHoldResumeMode(mode: string): mode is PolicyHoldResumeMode {
  return Object.prototype.hasOwnProperty.call(POLICY_HOLD_WORDING.modes, mode);
}
