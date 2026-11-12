package main

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// anthropicSpendLimitCause is the recovery_wait cause the api sets when a Claude run on an
// Anthropic API key hit its organization or workspace spend limit. The hold is owner-held:
// recovery_retry_not_before is NULL and nothing resumes it automatically.
const anthropicSpendLimitCause = "anthropic_spend_limit"

// spendLimitHoldShort is the compact table form of the hold.
const spendLimitHoldShort = "spend limit: owner hold"

// isSpendLimitHold reports an owner-held Anthropic spend-limit park. It checks the status
// as well as the cause: a credential switch re-queues the run without clearing the cause,
// so a queued run can still carry it and is not a hold.
func isSpendLimitHold(r apitypes.RunDTO) bool {
	return r.Status == statusRecoveryWait && strOr(r.RecoveryWaitCause, "") == anthropicSpendLimitCause
}

// spendLimitSwitchable reports whether the run's kind accepts a credential switch. Chat,
// judge, self_improve and job runs resolve their credential through their own ladders, so
// `uzi run set-token` is refused for them (the same set the api's credential override
// validation rejects).
func spendLimitSwitchable(r apitypes.RunDTO) bool {
	switch r.Kind {
	case runkind.Chat, runkind.Judge, runkind.SelfImprove, runkind.Job:
		return false
	}
	return true
}

const spendLimitResumeText = "Anthropic API spend limit reached · Resume uses this run's current credential selection. " +
	"If it still selects the capped token, raise the limit first: uzi run resume "

// spendLimitHoldLine is the full owner guidance sentence; "" for a run that is not held.
func spendLimitHoldLine(r apitypes.RunDTO) string {
	if !isSpendLimitHold(r) {
		return ""
	}
	id := cellText(r.ID)
	line := spendLimitResumeText + id
	if spendLimitSwitchable(r) {
		line += " · To choose another token now: uzi token list, then uzi run set-token " + id + " <label>"
	}
	return line + " · Cancel: uzi run cancel " + id
}

// spendLimitHoldTUILines is the short-row form for the TUI detail view.
func spendLimitHoldTUILines(r apitypes.RunDTO) []string {
	if !isSpendLimitHold(r) {
		return nil
	}
	id := cellText(r.ID)
	lines := []string{
		"Anthropic API spend limit reached · owner Resume or Cancel",
		"Resume may re-select the capped token; raise the limit first.",
		"Resume: uzi run resume " + id,
	}
	if spendLimitSwitchable(r) {
		lines = append(lines, "Other token: uzi token list, then uzi run set-token "+id+" <label>")
	}
	return append(lines, "Cancel: uzi run cancel "+id+" (terminal)", "No automatic resume.")
}
