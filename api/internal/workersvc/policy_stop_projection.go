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

// policyStopProjectionLocked requires the same transaction holding the parent
// row lock from a separate READ COMMITTED statement. Validate present evidence
// even when emission is disabled; only an absent receipt permits ordinary fallback.
func (s *Service) policyStopProjectionLocked(ctx context.Context, reader any, w store.Worker, r store.Run, generation int64, holdNegotiated bool) (*PolicyStopExecution, error) {
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
	return &PolicyStopExecution{
		ResolutionID:    evidence.record.ResolutionID.String(),
		ClaimGeneration: strconv.FormatInt(generation, 10),
	}, nil
}
