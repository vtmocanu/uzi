package workersvc

import (
	"context"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PolicyStopExecution is operation-local authority for the current claim only.
// It carries no approval of the candidate plan or replacement execution scope.
type PolicyStopExecution struct {
	ResolutionID    string `json:"resolution_id"`
	ClaimGeneration string `json:"claim_generation"`
}

// policyStopEvidenceLocked requires the same transaction holding the parent
// row lock from a separate READ COMMITTED statement. Validate present evidence
// even when emission is disabled; only an absent receipt permits ordinary fallback.
// A question park retains evidence but grants no execution authority.
func (s *Service) policyStopEvidenceLocked(ctx context.Context, reader any, w store.Worker, r store.Run, generation int64, holdNegotiated bool) (*policyStopExecutionContext, error) {
	if len(r.PolicyStopResolution) == 0 {
		return nil, nil
	}
	q, ok := reader.(policyStopExecutionReader)
	if !ok {
		return nil, stopReviewConflict("policy_stop_execution_unverifiable")
	}
	evidence, err := policyStopExecutionLocked(ctx, q, w, r, generation)
	if err != nil {
		return nil, err
	}
	if !s.p.PolicyStopNegotiated || !holdNegotiated {
		return nil, stopReviewConflict("policy_stop_execution_not_negotiated")
	}
	return evidence, nil
}

// policyStopProjectionLocked issues authority only for an executing status after
// the same evidence and negotiation validation used by live-transition preflight.
func (s *Service) policyStopProjectionLocked(ctx context.Context, reader any, w store.Worker, r store.Run, generation int64, holdNegotiated bool) (*PolicyStopExecution, error) {
	evidence, err := s.policyStopEvidenceLocked(ctx, reader, w, r, generation, holdNegotiated)
	if err != nil || evidence == nil {
		return nil, err
	}
	if r.Status != "claimed" && r.Status != "running" && r.Status != "awaiting_approval" {
		return nil, stopReviewConflict("policy_stop_execution_unverifiable")
	}
	return &PolicyStopExecution{
		ResolutionID:    evidence.record.ResolutionID.String(),
		ClaimGeneration: strconv.FormatInt(generation, 10),
	}, nil
}
