package workersvc

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"

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

// validatePolicyStopStateRequest runs before store access in setStateWithTx.
func validatePolicyStopStateRequest(req StateRequest) error {
	if req.PolicyStopResolutionID == nil {
		return nil
	}
	id, err := uuid.Parse(*req.PolicyStopResolutionID)
	if err != nil || id == uuid.Nil || id.String() != *req.PolicyStopResolutionID {
		return fmt.Errorf("%w: policy_stop_resolution_id must be a canonical nonzero UUID", ErrInvalidState)
	}
	return nil
}

// policyStopStateRequestLocked binds the scalar to checked evidence under the
// existing parent lock. The scalar supplies neither worker nor claim generation.
func (s *Service) policyStopStateRequestLocked(ctx context.Context, reader any, w store.Worker, r store.Run, req StateRequest) error {
	if req.ClaimGeneration == nil {
		return stopReviewConflict("policy_stop_execution_unverifiable")
	}
	evidence, err := s.policyStopEvidenceLocked(ctx, reader, w, r, *req.ClaimGeneration, s.policyRevisionProjectionNegotiated(w))
	if err != nil {
		return err
	}
	if evidence == nil {
		if req.PolicyStopResolutionID == nil {
			return nil
		}
		return stopReviewConflict("policy_stop_execution_unverifiable")
	}
	if req.PolicyStopResolutionID == nil || *req.PolicyStopResolutionID != evidence.record.ResolutionID.String() {
		return stopReviewConflict("policy_stop_execution_unverifiable")
	}
	return nil
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
