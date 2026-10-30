package main

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const retainedRecoveryExhaustedCause = "retained_recovery_exhausted"
const retainedRecoveryAdoptionFailedCause = "retained_recovery_adoption_failed"

// Recovery decisions require timing presence to distinguish an explicit hold
// from a partial response with unknown timing. Adoption failure is always held,
// even when a malformed or partial response supplies another timing shape.
func isRecoveryDecisionPark(r apitypes.RunDTO) bool {
	return r.Status == statusRecoveryWait && (strOr(r.RecoveryWaitCause, "") == retainedRecoveryExhaustedCause || strOr(r.RecoveryWaitCause, "") == "codex_idle_stall" || strOr(r.RecoveryWaitCause, "") == retainedRecoveryAdoptionFailedCause)
}

func isRecoveryDecisionHold(r apitypes.RunDTO) bool {
	return isRecoveryDecisionPark(r) && (strOr(r.RecoveryWaitCause, "") == retainedRecoveryAdoptionFailedCause || (r.RecoveryRetryNotBeforePresent && r.RecoveryRetryNotBefore == nil))
}

func recoveryDecisionWord(r apitypes.RunDTO) string {
	if isRecoveryDecisionHold(r) {
		return "recovery held"
	}
	if strOr(r.RecoveryWaitCause, "") != retainedRecoveryAdoptionFailedCause && r.RecoveryRetryNotBefore != nil {
		return "recovery retry"
	}
	return "recovery unknown"
}

func recoveryDecisionLine(r apitypes.RunDTO) string {
	state := recoveryDecisionName(r) + " · retry timing unknown"
	if isRecoveryDecisionHold(r) {
		state = recoveryDecisionName(r) + " · owner-held; no automatic resume"
	}
	if strOr(r.RecoveryWaitCause, "") != retainedRecoveryAdoptionFailedCause && r.RecoveryRetryNotBefore != nil {
		state = recoveryDecisionName(r) + " · timed retry not before " + r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339) + "; retry may still be pending"
	}
	guidance := idleStallResumeGuidance(r)
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
			"Retained recovery adoption failed.",
			"Integrity check refused adoption.",
			"Local work and custody preserved.",
			"Owner-held · no automatic resume.",
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
	if strOr(r.RecoveryWaitCause, "") != retainedRecoveryAdoptionFailedCause && r.RecoveryRetryNotBefore != nil {
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
		return "retained recovery adoption failed · integrity check refused adoption · local work and custody preserved"
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
