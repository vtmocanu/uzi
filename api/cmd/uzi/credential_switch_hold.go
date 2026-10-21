package main

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The two server-owned holds a credential switch on a retained-recovery run can leave a run
// in (issue #3181). Both are status paused. They are VOCABULARY values of hold_reason (the
// server enum), not credentials.
//
//   - credential_switch_settling: settlement could not be proven at switch time and the worker
//     is still finishing its own operations (lane disposal, deps install). Owner Resume and
//     `run set-token` are refused (409) until the worker acknowledges, is lost, or
//     re-registers. Cancel always works.
//   - credential_switch_unsettled: the worker finished its own operations, but the run's
//     processes were not proven stopped; the work and custody stay on that worker. Owner
//     Resume (requeues on the newly chosen token; the next claim re-proves quiescence fail
//     closed) or Cancel.
const (
	holdCredentialSwitchSettling  = "credential_switch_settling"  //nolint:gosec // G101: a run hold-reason VOCABULARY value (the server enum), not a credential.
	holdCredentialSwitchUnsettled = "credential_switch_unsettled" //nolint:gosec // G101: a run hold-reason VOCABULARY value (the server enum), not a credential.
)

// TUI token glyph and words of the two holds; each word fits boardStatusWordWidth.
const (
	credSwitchGlyph         = "⊘"
	credSwitchSettlingWord  = "settling"
	credSwitchUnsettledWord = "unsettled"
)

func isCredentialSwitchSettlingHold(r apitypes.RunDTO) bool {
	return r.Status == statusPaused && strOr(r.HoldReason, "") == holdCredentialSwitchSettling
}

func isCredentialSwitchUnsettledHold(r apitypes.RunDTO) bool {
	return r.Status == statusPaused && strOr(r.HoldReason, "") == holdCredentialSwitchUnsettled
}

// isCredentialSwitchHold reports either credential-switch hold. Both are owner-attention
// parks: `run wait` stops on them and the TUI bands them into NEEDS YOU.
func isCredentialSwitchHold(r apitypes.RunDTO) bool {
	return isCredentialSwitchSettlingHold(r) || isCredentialSwitchUnsettledHold(r)
}

// credentialSwitchAction is the owner's next step for a credential-switch hold, or "".
func credentialSwitchAction(r apitypes.RunDTO) string {
	id := cellText(r.ID)
	switch {
	case isCredentialSwitchSettlingHold(r):
		return "worker is finishing its own operations; Resume opens when it finishes (or if the worker is lost); Cancel now: uzi run cancel " + id
	case isCredentialSwitchUnsettledHold(r):
		return "Resume or Cancel: uzi run resume " + id + " / uzi run cancel " + id
	}
	return ""
}

// credentialSwitchHoldLine is the one-line held-run sentence the TUI and `run logs --follow`
// print for a credential-switch hold, or "" for any other run.
func credentialSwitchHoldLine(r apitypes.RunDTO) string {
	switch {
	case isCredentialSwitchSettlingHold(r):
		return "waiting: credential switch settling · " + credentialSwitchAction(r)
	case isCredentialSwitchUnsettledHold(r):
		return "held: credential switch; processes were not proven stopped; work kept on that worker · " + credentialSwitchAction(r)
	}
	return ""
}
