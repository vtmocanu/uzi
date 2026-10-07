package workersvc

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// RunInventoryGuard reads the exact generation's immutable claim-time guard.
func (s *Service) RunInventoryGuard(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64) (bool, error) {
	q, ok := s.q.(interface {
		RunHasGuardedInventoryHold(context.Context, store.RunHasGuardedInventoryHoldParams) (bool, error)
	})
	if !ok {
		if slices.Contains(wkr.ProtocolCapabilities, capability.RecoveryInventoryV1) {
			return false, errors.New("worker inventory guard query unavailable")
		}
		return false, nil
	}
	return q.RunHasGuardedInventoryHold(ctx, store.RunHasGuardedInventoryHoldParams{
		RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID, Generation: generation,
	})
}
