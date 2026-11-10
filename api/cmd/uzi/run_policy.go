package main

import (
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const holdCodexPolicy = "codex_policy_hold"

func isCodexPolicyHold(r apitypes.RunDTO) bool {
	return r.Status == statusPaused && strOr(r.HoldReason, "") == holdCodexPolicy
}

// policyCaptureClause uses only the current episode's worker-reported evidence.
// Neither a later checkpoint nor retained session metadata verifies this capture.
func policyCaptureClause(r apitypes.RunDTO) string {
	p := r.PolicyHold
	if p == nil || p.Published == nil {
		return "unverified preservation"
	}
	head := cellText(strOr(p.CapturedHead, ""))
	if head != "" {
		if *p.Published {
			return "worker-reported published checkpoint"
		}
		return "worker-reported verified local checkpoint"
	}
	if !*p.Published {
		return "local-only/unverified preservation"
	}
	return "unverified preservation"
}

// policyHoldAction names the owner's next step. Without a resume mode the episode accepts, Resume
// is unavailable; with one, it points at `run resume-now --policy-mode`. Stop is never offered.
func policyHoldAction(r apitypes.RunDTO) string {
	id := cellText(r.ID)
	if p := r.PolicyHold; p != nil && len(p.ResumeModes) > 0 && isCodexPolicyHold(r) {
		return "Resume: uzi run resume-now " + id + " --policy-mode <mode> --episode " + cellText(p.OutcomeID) +
			" (Stop unavailable); Cancel: uzi run cancel " + id
	}
	return "Resume and Stop unavailable; Cancel: uzi run cancel " + id
}

// policyHoldDetailClauses are the cause, the accepted resume modes, a pending one-claim fresh start
// and unverified recovery for a confirmed hold; nil when the DTO carries no policy hold. Every
// server-supplied string goes through cellText; known causes and modes use the shared wording table.
func policyHoldDetailClauses(r apitypes.RunDTO) []string {
	p := r.PolicyHold
	if p == nil {
		return nil
	}
	clauses := []string{"cause: " + apitypes.PolicyHoldCauseText(p.HoldCause)}
	if len(p.ResumeModes) > 0 {
		modes := make([]string, 0, len(p.ResumeModes))
		for _, m := range p.ResumeModes {
			label := apitypes.PolicyResumeModeLabel(m)
			if label == "" {
				label = "unrecognised mode"
			}
			modes = append(modes, cellText(m)+" ("+label+")")
		}
		clauses = append(clauses, "resume modes: "+strings.Join(modes, ", "))
	}
	if p.PendingFresh {
		clauses = append(clauses, "one-claim fresh start pending, not yet confirmed")
	}
	if p.RecoveryRequired && !p.RecoveryVerifiedThisClaim {
		clauses = append(clauses, "recovery head required, not yet verified by this claim")
	}
	return clauses
}

// policyFreshPendingRow reports a resolved hold whose one-pickup fresh start is still pending:
// the run is no longer held, so policyHoldDetailClauses is not rendered for it.
func policyFreshPendingRow(r apitypes.RunDTO) []string {
	if terminalRunStatuses[r.Status] || isCodexPolicyHold(r) || r.PolicyHold == nil || !r.PolicyHold.PendingFresh {
		return nil
	}
	return []string{"POLICY_FRESH", "One-pickup fresh start pending: the next pickup may start one new conversation from the saved work."}
}

// policyPendingRow is delivery telemetry, not proof that the server applied a hold.
// A confirmed hold takes precedence over telemetry left over from its delivery.
func policyPendingRow(r apitypes.RunDTO) []string {
	if terminalRunStatuses[r.Status] || isCodexPolicyHold(r) ||
		r.OutcomePending == nil || r.OutcomePending.Reason != apitypes.OutcomePendingPolicyHold {
		return nil
	}
	return []string{"POLICY_PENDING", "Codex policy hold delivery pending; server hold not confirmed; " + policyHoldAction(r)}
}
