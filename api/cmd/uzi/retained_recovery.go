package main

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const retainedRecoveryExhaustedCause = "retained_recovery_exhausted"

// Full RunDTO reads carry an explicit nullable retry stamp. Cause-only
// projections must use their neutral presentation instead of this predicate.
func isRetainedRecovery(r apitypes.RunDTO) bool {
	return r.Status == statusRecoveryWait && strOr(r.RecoveryWaitCause, "") == retainedRecoveryExhaustedCause
}

func isRetainedRecoveryHold(r apitypes.RunDTO) bool {
	return isRetainedRecovery(r) && r.RecoveryRetryNotBefore == nil
}

func retainedRecoveryWord(r apitypes.RunDTO) string {
	if isRetainedRecoveryHold(r) {
		return "recovery held"
	}
	return "recovery retry"
}

func retainedRecoveryLine(r apitypes.RunDTO) string {
	state := "retained recovery exhausted · owner-held; no automatic resume"
	if r.RecoveryRetryNotBefore != nil {
		state = "retained recovery exhausted · timed retry not before " + r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339) + "; retry may still be pending"
	}
	return state + " · owner Resume now: uzi run resume " + cellText(r.ID) +
		"; Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)" +
		" · Resume requests an attempt; success is not assured · source/archive availability and latest edits unverified"
}

func retainedRecoveryTUILines(r apitypes.RunDTO) []string {
	state := "Owner-held · no automatic resume."
	if r.RecoveryRetryNotBefore != nil {
		state = "Timed retry not before " + r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339)
	}
	lines := []string{
		"retained recovery exhausted · owner Resume now or Cancel",
		state,
		"Resume: uzi run resume " + cellText(r.ID),
		"Cancel: uzi run cancel " + cellText(r.ID) + " (terminal)",
		"Resume requests an attempt; success is not assured.",
		"Source/archive availability and latest edits unverified.",
	}
	return append(lines, workerExhaustionEvidence(r)...)
}

// workerHoldNoticeKey includes timing so a finite-to-NULL transition is visible
// even when status and cause have not changed.
func workerHoldNoticeKey(r apitypes.RunDTO) string {
	key := r.Status + "/" + strOr(r.HoldReason, "") + "/" + strOr(r.RecoveryWaitCause, "")
	if isRetainedRecovery(r) {
		if r.RecoveryRetryNotBefore == nil {
			return key + "/held"
		}
		return key + "/" + r.RecoveryRetryNotBefore.UTC().Format(time.RFC3339Nano)
	}
	return key
}
