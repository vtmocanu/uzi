package workersvc

import (
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// CredentialSwitchSignal is the worker-facing held-state switch signal (PRD #1247 M5, D3/D4):
// it tells the worker holding claim generation N that a token switch has been requested for
// exactly that claim, so the worker begins its two-phase local release. It is SEPARATE from the
// user-facing credentialSwitchState string (requested/released) the run DTO carries — that one
// tells the operator what stage the switch is in; this one is the wire trigger the worker acts on.
type CredentialSwitchSignal struct {
	Generation int64 `json:"generation"`
	// OpID is the switch operation id the credential_switch_op_id trigger minted for the pending
	// stamp (issue #4201). An in-place worker echoes it on the apply request and on its held,
	// failed and uncertain reports so each is fenced to this exact request. Omitted when the stamp
	// carries no id (an older row), which a v1 worker never reads.
	OpID string `json:"op_id,omitempty"`
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
	sig := &CredentialSwitchSignal{Generation: run.ClaimGeneration}
	if run.CredentialSwitchOpID.Valid {
		sig.OpID = uuid.UUID(run.CredentialSwitchOpID.Bytes).String()
	}
	return sig
}
