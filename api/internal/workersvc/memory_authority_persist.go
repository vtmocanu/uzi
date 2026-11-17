package workersvc

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// persistMemoryAuthority records a run-lane claim's memory-authority report on the worker row
// (issue #4395, workers.memory_authority_lost_at), so a keeper whose memory authority is lost is
// not counted as ready by the warm-reserve policy.
//
// It runs inside the claim transaction, with locked being the worker row as read by
// GetWorkerForUpdate, after the claim snapshot was validated against it. A lost report sets the
// column when it is NULL (the first-lost time is kept); a held report clears it when set. It
// writes only on a state change, so the common held-to-held claim does no write. An absent report
// from a worker without both capabilities is neither lost nor held and writes nothing.
//
// It persists on snapshot-carrying run and ordinary-lane claims, whether or not a run is claimed:
// the caller invokes it before the claim, so empty and overflow-refused polls persist too. It does
// not persist on claims with no active snapshot (for example UZI_ACTIVE_SNAPSHOT_DISABLED, a
// missing register nonce or a malformed body), nor when the owner's vault gate returns idle before
// the transaction starts. Ephemeral claims (ephemeral_lease.go) are out of scope: ephemeral
// workers are never in a pool.
//
// The registration fence is the snapshot's register nonce, repeated in the UPDATE's WHERE. A
// claim without a snapshot carries no registration identity, so it persists nothing.
func (s *Service) persistMemoryAuthority(ctx context.Context, qtx *store.Queries, locked store.Worker, snapshot *ActiveSnapshot, lane string, report MemoryAuthorityReport) error {
	if lane == "cross_check" || snapshot == nil || snapshot.RegisterNonce == "" {
		return nil
	}
	switch {
	case memoryAuthorityLost(locked.ProtocolCapabilities, report):
		if locked.MemoryAuthorityLostAt.Valid {
			return nil
		}
		_, err := qtx.SetWorkerMemoryAuthorityLost(ctx, store.SetWorkerMemoryAuthorityLostParams{
			ID: locked.ID, RegisterNonce: pgconv.TextOrNull(snapshot.RegisterNonce), LostAt: pgconv.Time(s.now()),
		})
		return err
	case report.held():
		if !locked.MemoryAuthorityLostAt.Valid {
			return nil
		}
		_, err := qtx.ClearWorkerMemoryAuthorityLost(ctx, store.ClearWorkerMemoryAuthorityLostParams{
			ID: locked.ID, RegisterNonce: pgconv.TextOrNull(snapshot.RegisterNonce),
		})
		return err
	}
	return nil
}
