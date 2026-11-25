package main

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const retainedRecoveryExhaustedCause = "retained_recovery_exhausted"
const retainedRecoveryAdoptionFailedCause = "retained_recovery_adoption_failed"

// Closed-set recovery_wait_detail values (issue #4177). A client renders only these and never
// prints the raw string; nil or any other value selects the generic text and permits nothing new.
const (
	recoveryDetailSourceEndDeclined       = "source_end_declined"
	recoveryDetailSecretRemediationActive = "secret_remediation_active"
	recoveryDetailRemediationProofUnknown = "remediation_proof_unknown"
)

const sourceEndDeclinedGuidance = "An earlier flight declined source-use end. The agent did not start; local work and custody are protected. Resume with the unchanged worker registration is refused (409). A different worker registration only enables a guarded attempt; it does not establish that all retained sources are usable. Independently restore only from verified sources with prerequisite commits present; an archive may omit the latest work. Operator discard is separate and requires a verified backup. Cancel remains available."

// adoptionFailedDetail returns the known detail of an adoption-failed park, else "".
func adoptionFailedDetail(r apitypes.RunDTO) string {
	if strOr(r.RecoveryWaitCause, "") != retainedRecoveryAdoptionFailedCause {
		return ""
	}
	switch d := strOr(r.RecoveryWaitDetail, ""); d {
	case recoveryDetailSourceEndDeclined, recoveryDetailSecretRemediationActive, recoveryDetailRemediationProofUnknown:
		return d
	}
	return ""
}

// retainedContinuationUnwitnessedCause is the server-only park for a retained
// continuation whose claimed flight was never witnessed starting or departing
// within a bounded window. The claim is released, local work and custody are
// kept, and only the owner can Resume or Cancel; no retry is ever scheduled.
const retainedContinuationUnwitnessedCause = "retained_continuation_unwitnessed"

// isAlwaysHeldRecoveryCause reports causes that are owner-held regardless of
// any retry timing the response carries (malformed or partial included).
func isAlwaysHeldRecoveryCause(cause string) bool {
	return cause == retainedRecoveryAdoptionFailedCause || cause == retainedContinuationUnwitnessedCause
}

// Recovery decisions require timing presence to distinguish an explicit hold
// from a partial response with unknown timing. Adoption failure is always held,
// even when a malformed or partial response supplies another timing shape.
func isRecoveryDecisionPark(r apitypes.RunDTO) bool {
	return r.Status == statusRecoveryWait && (strOr(r.RecoveryWaitCause, "") == retainedRecoveryExhaustedCause || strOr(r.RecoveryWaitCause, "") == "codex_idle_stall" || isAlwaysHeldRecoveryCause(strOr(r.RecoveryWaitCause, "")))
}

func isRecoveryDecisionHold(r apitypes.RunDTO) bool {
	return isRecoveryDecisionPark(r) && (isAlwaysHeldRecoveryCause(strOr(r.RecoveryWaitCause, "")) || (r.RecoveryRetryNotBeforePresent && r.RecoveryRetryNotBefore == nil))
}

func recoveryDecisionWord(r apitypes.RunDTO) string {
	if isRecoveryDecisionHold(r) {
		return "recovery held"
	}
	if !isAlwaysHeldRecoveryCause(strOr(r.RecoveryWaitCause, "")) && r.RecoveryRetryNotBefore != nil {
		return "recovery retry"
	}
	return "recovery unknown"
}

func recoveryDecisionLine(r apitypes.RunDTO) string {
	state := recoveryDecisionName(r) + " · retry timing unknown"
	if isRecoveryDecisionHold(r) {
		state = recoveryDecisionName(r) + " · owner-held; no automatic resume"
	}
	if !isAlwaysHeldRecoveryCause(strOr(r.RecoveryWaitCause, "")) && r.RecoveryRetryNotBefore != nil {
		state = recoveryDecisionName(r) + " · timed retry not before " + r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339) + "; retry may still be pending"
	}
	guidance := idleStallResumeGuidance(r)
	if strOr(r.RecoveryWaitCause, "") == retainedRecoveryAdoptionFailedCause {
		switch adoptionFailedDetail(r) {
		case recoveryDetailSourceEndDeclined:
			guidance = sourceEndDeclinedGuidance
		case recoveryDetailSecretRemediationActive:
			guidance = "The retained source is held under an active secret-scan remediation. Resume cannot succeed unchanged and is refused. Export an available archive (`uzi run export " + cellText(r.ID) + " --output <path>`), which may omit later work kept on the worker; an operator can recover the retained work from worker storage. Cancel ends the run."
		case recoveryDetailRemediationProofUnknown:
			guidance = "The remediation proof could not be verified. Resume may help; if the block persists the run holds again and each new claim adds a custody hold."
		default:
			guidance = "Resume starts another recovery episode. If the block persists, the run can hold again. Each new claim adds a custody hold. Resolve the cause before resuming."
		}
	}
	if guidance != "" {
		guidance = " · " + guidance
	}
	if secretRemediationActiveDetail(r) {
		// Issue #4177: the server refuses Resume (409) for this detail, so the CLI never offers it.
		return state + guidance + " · Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)"
	}
	if strOr(r.RecoveryWaitCause, "") == retainedRecoveryAdoptionFailedCause && adoptionFailedDetail(r) == recoveryDetailSourceEndDeclined {
		return state + guidance + " · Only after a different worker registration, owner guarded attempt: uzi run resume " + cellText(r.ID) +
			"; Cancel remains available: uzi run cancel " + cellText(r.ID) + " (terminal)"
	}
	return state + guidance + " · owner Resume now: uzi run resume " + cellText(r.ID) +
		"; Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)" +
		" · Resume requests an attempt; success is not assured · source/archive availability and latest edits unverified" +
		freshResumeHintSuffix(r)
}

// secretRemediationActiveDetail reports an adoption-failed park classified secret_remediation_active,
// for which the server refuses Resume.
func secretRemediationActiveDetail(r apitypes.RunDTO) bool {
	return strOr(r.RecoveryWaitCause, "") == retainedRecoveryAdoptionFailedCause && adoptionFailedDetail(r) == recoveryDetailSecretRemediationActive
}

func recoveryDecisionTUILines(r apitypes.RunDTO) []string {
	if strOr(r.RecoveryWaitCause, "") == retainedRecoveryAdoptionFailedCause {
		var guidance []string
		switch adoptionFailedDetail(r) {
		case recoveryDetailSourceEndDeclined:
			guidance = []string{
				"An earlier flight declined source-use end.",
				"Unchanged worker registration: Resume refused (409).",
				"A different registration enables a guarded attempt;",
				"not all retained sources may be usable.",
				"Independent restore requires verified sources and",
				"prerequisite commits. Archives may omit latest work.",
				"Operator discard is separate: verify a backup first.",
			}
		case recoveryDetailSecretRemediationActive:
			guidance = []string{
				"The retained source is held under an active",
				"secret-scan remediation. Resume cannot succeed",
				"unchanged and is refused.",
				"Export an available archive",
				"(`uzi run export " + cellText(r.ID) + " --output <path>`),",
				"which may omit later work kept on the worker;",
				"an operator can recover the retained work from",
				"worker storage. Cancel ends the run.",
			}
		case recoveryDetailRemediationProofUnknown:
			guidance = []string{
				"The remediation proof could not be verified.",
				"Resume may help; if the block persists the run",
				"holds again and each new claim adds a custody hold.",
			}
		default:
			guidance = []string{
				"Resume starts another recovery episode.",
				"If the block persists, the run can hold again.",
				"Each new claim adds a custody hold.",
				"Resolve the cause before resuming.",
			}
		}
		lines := []string{
			"Integrity hold: the agent did not start.",
			"Adoption failed, a predecessor needs remediation review,",
			"or this release does not support the predecessor.",
			"Local work and custody preserved.",
			"Owner-held · no automatic resume.",
		}
		lines = append(lines, guidance...)
		if secretRemediationActiveDetail(r) {
			lines = append(lines, "Cancel: uzi run cancel "+cellText(r.ID)+" (terminal)")
			return append(lines, workerExhaustionEvidence(r)...)
		}
		if adoptionFailedDetail(r) == recoveryDetailSourceEndDeclined {
			lines = append(lines,
				"Only after a different worker registration:",
				"Owner guarded attempt: uzi run resume "+cellText(r.ID),
				"Cancel remains available: uzi run cancel "+cellText(r.ID)+" (terminal)",
			)
			return append(lines, workerExhaustionEvidence(r)...)
		}
		lines = append(lines,
			"Owner Resume or Cancel:",
			"Resume: uzi run resume "+cellText(r.ID),
			"Cancel: uzi run cancel "+cellText(r.ID)+" (terminal)",
			"Resume requests an attempt; success is not assured.",
			"Source/archive availability and latest edits unverified.",
		)
		return append(lines, workerExhaustionEvidence(r)...)
	}
	if strOr(r.RecoveryWaitCause, "") == retainedContinuationUnwitnessedCause {
		return append([]string{
			"Retained continuation was never witnessed.",
			"Claim released; no retry scheduled.",
			"Local work and custody kept.",
			"Owner decision needed · no automatic resume.",
			"Owner Resume or Cancel:",
			"Resume: uzi run resume " + cellText(r.ID),
			"Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)",
			"Resume requests an attempt; success is not assured.",
			"Source/archive availability and latest edits unverified.",
		}, workerExhaustionEvidence(r)...)
	}
	state := "Retry timing unknown."
	if isRecoveryDecisionHold(r) {
		state = "Owner-held · no automatic resume."
	}
	if !isAlwaysHeldRecoveryCause(strOr(r.RecoveryWaitCause, "")) && r.RecoveryRetryNotBefore != nil {
		state = "Timed retry not before " + r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339)
	}
	lines := []string{
		recoveryDecisionName(r) + " · owner Resume now or Cancel",
		state,
		"Resume: uzi run resume " + cellText(r.ID),
		"Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)",
		"Resume requests an attempt; success is not assured.",
		"Source/archive availability and latest edits unverified.",
	}
	if guidance := idleStallResumeGuidance(r); guidance != "" {
		lines = append(lines, guidance)
	}
	return append(lines, workerExhaustionEvidence(r)...)
}

// workerHoldNoticeKey includes timing so a finite-to-NULL transition is visible
// even when status and cause have not changed.
func workerHoldNoticeKey(r apitypes.RunDTO) string {
	key := r.Status + "/" + strOr(r.HoldReason, "") + "/" + strOr(r.RecoveryWaitCause, "")
	if isDiskFullPark(r) {
		key += "/disk/" + itoa(int(diskWaitOf(r)))
		if diskWaitOf(r) == diskWaitTimed {
			key += "/" + r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339Nano)
		}
		return key
	}
	if d := adoptionFailedDetail(r); d != "" {
		key += "/" + d
	}
	if isRecoveryDecisionPark(r) || isForgePark(r) {
		if isRecoveryDecisionHold(r) || isForgeOwnerHold(r) {
			return key + "/held"
		}
		if r.RecoveryRetryNotBefore == nil {
			return key + "/unknown"
		}
		return key + "/" + r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339Nano)
	}
	return key
}

func recoveryDecisionName(r apitypes.RunDTO) string {
	if strOr(r.RecoveryWaitCause, "") == retainedRecoveryAdoptionFailedCause {
		return "integrity hold · retained adoption failed, a predecessor needs remediation review, or this release does not support the predecessor · agent did not start · local work and custody preserved"
	}
	if strOr(r.RecoveryWaitCause, "") == retainedContinuationUnwitnessedCause {
		return "retained continuation not witnessed · owner decision needed · claim released · local work and custody kept · no retry scheduled"
	}
	if strOr(r.RecoveryWaitCause, "") == "codex_idle_stall" {
		return "Codex idle stall"
	}
	return "retained recovery exhausted"
}

func idleStallResumeGuidance(r apitypes.RunDTO) string {
	if strOr(r.RecoveryWaitCause, "") != "codex_idle_stall" {
		return ""
	}
	return "Resume starts a fresh wall and resets the idle-stall count; Cancel ends the run"
}

// freshResumeEligible mirrors the server's eligibility base (fn_fresh_resume_run_base in migration
// 00535_run_fresh_resumes.sql) on the fields the CLI has: a recovery_wait with one of four causes, no
// secret_remediation_active or source_end_declined detail, an explicit null retry stamp (the key must
// be present), a run kind that can seed, not a claimed Codex run (harness codex and claim_generation
// >= 1), no codex_policy_hold and no pending reserve pause. Clauses only the server can check, because
// the DTO lacks the field or the data: codex_material_revision and codex_secret_id in the Codex
// provenance test, an active publication-resume row, policy_pending_fresh, and the wall budget. The
// server stays the authority; this only keeps the hint off holds it always refuses.
func freshResumeEligible(r apitypes.RunDTO) bool {
	if r.Status != statusRecoveryWait || !r.RecoveryRetryNotBeforePresent || r.RecoveryRetryNotBefore != nil {
		return false
	}
	if r.Harness == "codex" && r.ClaimGeneration != nil && *r.ClaimGeneration >= 1 {
		return false
	}
	if strOr(r.HoldReason, "") == "codex_policy_hold" || strOr(r.PauseMode, "") == "reserve" {
		return false
	}
	switch strOr(r.RecoveryWaitCause, "") {
	case workerRequeueExhaustedCause, retainedRecoveryExhaustedCause,
		retainedRecoveryAdoptionFailedCause, retainedContinuationUnwitnessedCause:
	default:
		return false
	}
	switch strOr(r.RecoveryWaitDetail, "") {
	case recoveryDetailSecretRemediationActive, recoveryDetailSourceEndDeclined:
		return false
	}
	switch r.Kind {
	case "issue", "mr_rework", "self_improve":
		return true
	}
	return false
}

// freshResumeHintSuffix points a held run at `uzi run fresh-resume` (issue #4628) when the run passes
// freshResumeEligible.
func freshResumeHintSuffix(r apitypes.RunDTO) string {
	if !freshResumeEligible(r) {
		return ""
	}
	return " · " + freshResumeHint(r.ID)
}
