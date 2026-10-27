package workersvc

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This is the existing hold negotiation, not Stop execution authority.
func (s *Service) policyRevisionProjectionNegotiated(w store.Worker) bool {
	return s.p.PolicyHoldNegotiated && s.p.PolicyHoldSnapshotEnabled && s.p.TerminalPendingLease > 0 &&
		slices.Contains(w.ProtocolCapabilities, capability.ProviderPolicyHoldV1) &&
		slices.Contains(w.ProtocolCapabilities, capability.CredentialSwitchV1)
}

// policyRevisionStateProjection is captured under one owned lock and published
// only after the state transaction commits. A false approval remains present.
type policyRevisionStateProjection struct {
	Gate         json.RawMessage
	PlanApproved *bool
}

// policyRevisionGateProjection requires the caller's locked row and transaction
// reader. Nil means unknown; JSON null means verified coverage without a gate.
// Neither value authorizes execution or approves a plan.
func policyRevisionGateProjection(ctx context.Context, reader any, r store.Run) (json.RawMessage, error) {
	if !policyEligible(r) || r.TargetRunID.Valid {
		return nil, nil
	}
	source, err := policyRevisionSourceBeforePublication(ctx, reader, r)
	if err != nil || source == nil {
		return nil, err
	}
	return json.Marshal(source.Gate)
}
