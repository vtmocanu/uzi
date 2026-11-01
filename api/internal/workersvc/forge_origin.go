package workersvc

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Closed set of cooldown reasons; the table CHECK enforces the same set. Never request text.
const (
	forgeReasonCloneFetchExhausted = "transient_clone_fetch_exhausted"
	forgeReasonPublicationExhaust  = "transient_publication_exhausted"
)

// observeParkedForgeOrigin is the exhausted transient clone/fetch group seam.
// Only parkForgeUnreachable's applied park calls it, under the authenticated run
// lock in the same transaction. It stores a fixed safe reason, never request text
// or an arbitrary URL. A floor_unverified refusal is not network evidence.
func (s *Service) observeParkedForgeOrigin(ctx context.Context, q *store.Queries, run store.Run) error {
	return s.observeForgeOrigin(ctx, q, run, false, forgeReasonCloneFetchExhausted)
}

// observeForgeOrigin records one exhausted failure on the run's stamped Git origin, or for the api
// surface on its server-derived API origin (issue #1324 D9). Both write the same cooldown table with
// the same backoff; only the surface flag differs. Same lock context as observeParkedForgeOrigin.
func (s *Service) observeForgeOrigin(ctx context.Context, q *store.Queries, run store.Run, api bool, reason string) error {
	baseMs := float64(s.recoveryParkFallbackFor(0).Milliseconds())
	capMs := float64(s.recoveryParkFallbackFor(30).Milliseconds())
	if api {
		_, err := q.ObserveParkedForgeApiOrigin(ctx, store.ObserveParkedForgeApiOriginParams{
			RunID: run.ID, WorkerID: run.WorkerID.Bytes, Generation: run.ClaimGeneration,
			BaseMs: baseMs, CapMs: capMs, Reason: reason,
		})
		return err
	}
	_, err := q.ObserveParkedForgeOrigin(ctx, store.ObserveParkedForgeOriginParams{
		RunID: run.ID, WorkerID: run.WorkerID.Bytes, Generation: run.ClaimGeneration,
		BaseMs: baseMs, CapMs: capMs, Reason: reason,
	})
	return err
}
