// The owner-facing text of a fresh resume (issue #4628), in one place for the web surface. The CLI
// carries the same substance in api/cmd/uzi/fresh_resume.go (freshResumeCutoffText) and the server in
// freshNotes (api/internal/workersvc/fresh_resume_select.go); keep the three in step. A Go test checks
// that each CLI line appears in the server copy; the web copy is checked by freshResumeText.test.ts.

/** The cutoff text the owner confirms against. */
export const FRESH_RESUME_CUTOFF_TEXT: readonly string[] = [
  "Edits made after the saved evidence may be missing from the resumed run.",
  "The original worker's source and custody are kept; starting a fresh resume does not delete them.",
  "Unsaved edits may be lost if the original worker's volume is gone.",
  "Each attempt keeps its own custody hold, and a healthy seeded run counts one extra admission unit until its new hold is released or the run ends.",
  "While the original worker and the new worker both hold custody, only a third eligible hosted worker can take a repeat fresh resume. After seeding, the original worker never takes this run again, so an owner with only two workers whose new worker is lost must add a worker or cancel the run.",
];

export function freshResumeUncoveredText(generations: readonly number[]): string {
  return `No saved evidence exists for generation ${generations.join(", ")}; any edits made in it are not included.`;
}

/** Explains each status that is not "available", and each 409 reason a start can add. */
const FRESH_RESUME_STATUS_TEXT: Record<string, string> = {
  disabled: "Fresh resume is not enabled on this instance.",
  no_destination: "No other persistent hosted worker is online with fresh resume support. Add or start a worker, then try again.",
  custody_limit: "Starting would take you past your custody hold limit. Release or discard a hold you no longer need, then try again.",
  no_evidence: "This run has no saved evidence to resume from. The original worker's source is kept; nothing was started.",
  expired: "The saved evidence has expired or is about to. Nothing was started.",
  unbound: "The run's checkpoint cannot be tied to a generation, so it might hold newer work than the evidence. Nothing was started.",
  prereq_unproven: "The saved evidence's base commits could not be proven on the forge. Nothing was started.",
  ambiguous: "Several saved copies of this run's work exist and none contains the others. Nothing was started.",
  in_progress: "A fresh resume of this run is already in progress. Wait for it to finish.",
  ineligible: "This run is not held in a state a fresh resume can start from.",
  stale_selection: "The selection changed since the preview. Review the new preview and confirm again.",
  request_completed: "This request already finished. Review a new preview and confirm again.",
  fresh_resume_in_progress: "A fresh resume of this run is already in progress. Wait for it to finish.",
};

const FRESH_RESUME_GENERIC_REFUSAL = "Fresh resume was refused for a reason this page does not know; nothing was started.";

export function freshResumeStatusText(status: string | undefined): string {
  return (status !== undefined && Object.prototype.hasOwnProperty.call(FRESH_RESUME_STATUS_TEXT, status) && FRESH_RESUME_STATUS_TEXT[status]) || FRESH_RESUME_GENERIC_REFUSAL;
}

const FRESH_RESUME_CAUSES: readonly string[] = [
  "worker_requeue_exhausted",
  "retained_recovery_exhausted",
  "retained_recovery_adoption_failed",
  "retained_continuation_unwitnessed",
];

/**
 * Mirrors the server's eligibility base (fn_fresh_resume_run_base in
 * api/internal/store/migrations/00535_run_fresh_resumes.sql) on the fields the page has: a
 * recovery_wait with one of four causes, no secret_remediation_active or source_end_declined detail,
 * no retry stamp, a run kind that can seed, not a claimed Codex run (harness codex and
 * claim_generation >= 1), no codex_policy_hold, and no pending reserve pause. Clauses only the
 * server can check, because the page has no such field: codex_material_revision and codex_secret_id
 * in the Codex provenance test, an active publication-resume row, policy_pending_fresh, and the wall
 * budget. The server stays the authority; this only keeps the offer off holds it always refuses.
 */
export function freshResumeEligible(run: {
  status: string;
  kind: string;
  harness?: string | null;
  claim_generation?: number | null;
  hold_reason?: string | null;
  pause_mode?: string | null;
  recovery_wait_cause?: string | null;
  recovery_wait_detail?: string | null;
  recovery_retry_not_before?: string | null;
}): boolean {
  return run.status === "recovery_wait"
    && !(run.harness === "codex" && (run.claim_generation ?? 0) >= 1)
    && run.hold_reason !== "codex_policy_hold"
    && run.pause_mode !== "reserve"
    && run.recovery_wait_cause != null && FRESH_RESUME_CAUSES.includes(run.recovery_wait_cause)
    && run.recovery_wait_detail !== "secret_remediation_active"
    && run.recovery_wait_detail !== "source_end_declined"
    && run.recovery_retry_not_before === null
    && ["issue", "mr_rework", "self_improve"].includes(run.kind);
}
