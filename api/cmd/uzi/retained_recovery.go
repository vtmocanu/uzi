package main

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const retainedRecoveryExhaustedCause = "retained_recovery_exhausted"

// Recovery decisions require timing presence to distinguish an explicit hold
// from a partial response with unknown timing.
func isRecoveryDecisionPark(r apitypes.RunDTO) bool {
	return r.Status == statusRecoveryWait && (strOr(r.RecoveryWaitCause, "") == retainedRecoveryExhaustedCause || strOr(r.RecoveryWaitCause, "") == "codex_idle_stall")
}

func isRecoveryDecisionHold(r apitypes.RunDTO) bool {
	return isRecoveryDecisionPark(r) && r.RecoveryRetryNotBeforePresent && r.RecoveryRetryNotBefore == nil
}

func recoveryDecisionWord(r apitypes.RunDTO) string {
	if isRecoveryDecisionHold(r) {
		return "recovery held"
	}
	if r.RecoveryRetryNotBefore != nil {
		return "recovery retry"
	}
	return "recovery unknown"
}

func recoveryDecisionLine(r apitypes.RunDTO) string {
	state := recoveryDecisionName(r) + " · retry timing unknown"
	if isRecoveryDecisionHold(r) {
		state = recoveryDecisionName(r) + " · owner-held; no automatic resume"
	}
	if r.RecoveryRetryNotBefore != nil {
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
	state := "Retry timing unknown."
	if isRecoveryDecisionHold(r) {
		state = "Owner-held · no automatic resume."
	}
	if r.RecoveryRetryNotBefore != nil {
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
	if isRecoveryDecisionPark(r) {
		if isRecoveryDecisionHold(r) {
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
