package main

import "github.com/vtmocanu/uzi/api/internal/apitypes"

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

func policyHoldAction(r apitypes.RunDTO) string {
	return "Resume and Stop unavailable; Cancel: uzi run cancel " + cellText(r.ID)
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
