package main

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const retainedRecoveryExhaustedCause = "retained_recovery_exhausted"
const retainedRecoveryAdoptionFailedCause = "retained_recovery_adoption_failed"

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
		guidance = "Resume starts another recovery episode. If the block persists, the run can hold again. Each new claim adds a custody hold. Resolve the cause before resuming."
	}
	if guidance != "" {
		guidance = " · " + guidance
	}
	return state + guidance + " · owner Resume now: uzi run resume " + cellText(r.ID) +
		"; Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)" +
		" · Resume requests an attempt; success is not assured · source/archive availability and latest edits unverified"
}

func recoveryDecisionTUILines(r apitypes.RunDTO) []string {
	if strOr(r.RecoveryWaitCause, "") == retainedRecoveryAdoptionFailedCause {
		return append([]string{
			"Integrity hold: the agent did not start.",
			"Adoption failed, a predecessor needs remediation review,",
			"or this release does not support the predecessor.",
			"Local work and custody preserved.",
			"Owner-held · no automatic resume.",
			"Resume starts another recovery episode.",
			"If the block persists, the run can hold again.",
			"Each new claim adds a custody hold.",
			"Resolve the cause before resuming.",
			"Owner Resume or Cancel:",
			"Resume: uzi run resume " + cellText(r.ID),
			"Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)",
			"Resume requests an attempt; success is not assured.",
			"Source/archive availability and latest edits unverified.",
		}, workerExhaustionEvidence(r)...)
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
