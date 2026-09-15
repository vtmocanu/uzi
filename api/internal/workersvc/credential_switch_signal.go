package workersvc

import (
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CredentialSwitchSignal is the worker-facing held-state switch signal (PRD #1247 M5, D3/D4):
// it tells the worker holding claim generation N that a token switch has been requested for
// exactly that claim, so the worker begins its two-phase local release. It is SEPARATE from the
// user-facing credentialSwitchState string (requested/released) the run DTO carries — that one
// tells the operator what stage the switch is in; this one is the wire trigger the worker acts on.
type CredentialSwitchSignal struct {
	Generation int64 `json:"generation"`
}

// PendingCredentialSwitchSignal returns the signal for the worker holding the run's CURRENT
// claim, or nil when no switch is pending for this claim. A switch is pending-for-this-claim
// iff it was requested, the claim is NOT yet released, and the stamp targets the current
// generation. After release (claim_released_at set) or after reclaim (claim_generation
// advances past the stamp), this returns nil — so the signal is scoped to the exact claim the
// switch targeted and never leaks onto the next claim.
func PendingCredentialSwitchSignal(run store.Run) *CredentialSwitchSignal {
	if !run.CredentialSwitchRequestedAt.Valid {
		return nil
	}
	if run.ClaimReleasedAt.Valid {
		return nil
	}
	if !run.CredentialSwitchGeneration.Valid || run.CredentialSwitchGeneration.Int64 != run.ClaimGeneration {
		return nil
	}
	return &CredentialSwitchSignal{Generation: run.ClaimGeneration}
}
