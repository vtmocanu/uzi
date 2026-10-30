package workersvc

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// observeParkedForgeOrigin is the exhausted transient clone/fetch group seam.
// Only parkForgeUnreachable's applied park calls it, under the authenticated run
// lock in the same transaction. It stores a fixed safe reason, never request text
// or an arbitrary URL. A floor_unverified refusal is not network evidence.
func (s *Service) observeParkedForgeOrigin(ctx context.Context, q *store.Queries, run store.Run) error {
	_, err := q.ObserveParkedForgeOrigin(ctx, store.ObserveParkedForgeOriginParams{
		RunID: run.ID, WorkerID: run.WorkerID.Bytes, Generation: run.ClaimGeneration,
		BaseMs: float64(s.recoveryParkFallbackFor(0).Milliseconds()),
		CapMs:  float64(s.recoveryParkFallbackFor(30).Milliseconds()),
	})
	return err
}
