package workersvc

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type custodyTransactionRowsReader interface {
	CustodyHoldLimitFromRows([]store.AppSetting) (int32, error)
}

// queuedCustodyAdmission is called before any worker lock. A settings failure or
// stale cache disables only this recovery classification, never liveness updates.
// The transaction rows, not a cache refresh, are the admission evidence.
func (s *Service) queuedCustodyAdmission(ctx context.Context, tx pgx.Tx) (int32, error) {
	if err := store.LockSettingsAdmission(ctx, tx); err != nil {
		return 0, err
	}
	reader, ok := s.p.CustodyHoldLimit.(custodyTransactionRowsReader)
	if !ok {
		return 0, nil
	}
	rows, err := store.New(tx).ListAppSettings(ctx)
	if err != nil {
		return 0, err
	}
	fresh, err := reader.CustodyHoldLimitFromRows(rows)
	if err != nil {
		slog.Warn("queued recovery: custody setting unavailable", "error", err)
		return 0, nil
	}
	cached, err := s.custodyLimit(ctx)
	if err != nil || cached != fresh {
		slog.Warn("queued recovery: custody setting cache disagrees with transaction", "error", err)
		return 0, nil
	}
	return fresh, nil
}

// validateQueuedCustodyLocks reads the post-lock tuple before any recovery
// transition. A target superseded while waiting for its parent lock is omitted.
func validateQueuedCustodyLocks(ctx context.Context, qtx *store.Queries, locks workerRecoveryLockSet) (workerRecoveryLockSet, error) {
	frozen, parents, err := locks.parameters()
	if err != nil {
		return workerRecoveryLockSet{}, err
	}
	locks.queuedJSON, err = qtx.RefreshFrozenQueuedCustodyTargets(ctx, store.RefreshFrozenQueuedCustodyTargetsParams{
		FrozenTargets: frozen, LockedParentIds: parents,
	})
	return locks, err
}

// parkQueuedCustody operates only inside the captured parent set. The hold-lock
// statement finishes before the fresh owner/own-hold recount in the state writer.
func (s *Service) parkQueuedCustody(ctx context.Context, qtx *store.Queries, workerID uuid.UUID, nonce pgtype.Text, limit int32, locks workerRecoveryLockSet, requeuedIDs, finalizeRequeuedIDs []uuid.UUID) ([]store.WorkerRecoveryDisposition, error) {
	if limit <= 0 {
		return nil, nil
	}
	_, parents, err := locks.parameters()
	if err != nil {
		return nil, err
	}
	if locks.queuedJSON == nil {
		return nil, fmt.Errorf("queued custody lock set was not validated")
	}
	frozen, err := qtx.RefreshFrozenQueuedCustodyTargets(ctx, store.RefreshFrozenQueuedCustodyTargetsParams{
		FrozenTargets: locks.queuedJSON, LockedParentIds: parents, RequeuedIds: requeuedIDs,
	})
	if err != nil {
		return nil, err
	}
	holdIDs, err := qtx.LockFrozenQueuedCustodyHolds(ctx, store.LockFrozenQueuedCustodyHoldsParams{
		WorkerID: pgconv.UUID(workerID), FrozenTargets: frozen, LockedParentIds: parents,
	})
	if err != nil {
		return nil, err
	}
	now := s.now()
	rows, err := qtx.ParkFrozenQueuedCustodyExhausted(ctx, store.ParkFrozenQueuedCustodyExhaustedParams{
		WorkerID: pgconv.UUID(workerID), ReleasedWorkerNonce: nonce,
		FrozenTargets: frozen, LockedParentIds: parents, CustodyHoldLimit: limit,
		LockedHoldIds: holdIDs, FinalizeRequeuedIds: finalizeRequeuedIDs,
		MaxRequeues:         int32(s.p.RunMaxRequeues), //nolint:gosec // bounded configuration, as in the active-run recovery paths.
		HeartbeatCutoff:     pgconv.Time(now.Add(-s.p.WorkerHeartbeatStale)),
		SnapshotFreshCutoff: pgconv.Time(now.Add(-(s.p.WorkerHeartbeatStale + s.p.WorkerHeartbeatInterval))),
	})
	if err != nil {
		return nil, err
	}
	result := make([]store.WorkerRecoveryDisposition, 0, len(rows))
	for _, row := range rows {
		result = append(result, store.WorkerRecoveryDisposition(row))
	}
	return result, nil
}

// parkStaleQueuedCustody follows the stale requeue pass. All worker locks are
// acquired before any parent locks. Each finite worker set is visited once;
// failure rolls back the complete pass, and the sweep reports it to its caller.
func (s *Service) parkStaleQueuedCustody(ctx context.Context, cutoff pgtype.Timestamptz) ([]store.WorkerRecoveryDisposition, error) {
	if s.txBeginner == nil {
		return nil, nil
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	limit, err := s.queuedCustodyAdmission(ctx, tx)
	if err != nil || limit == 0 {
		return nil, err
	}
	qtx := store.New(tx)
	workers, err := qtx.LockStaleQueuedCustodyWorkers(ctx, cutoff)
	if err != nil {
		return nil, err
	}
	// Freeze the complete union and acquire every parent in global ID order
	// before any hold lock, including parents shared by different workers.
	frozenRows, err := qtx.LockWorkerRecoveryParents(ctx, store.LockWorkerRecoveryParentsParams{AdditionalWorkerIds: workers})
	if err != nil {
		return nil, err
	}
	locks, err := workerRecoveryLocksFromRows(frozenRows)
	if err != nil {
		return nil, err
	}
	locks, err = validateQueuedCustodyLocks(ctx, qtx, locks)
	if err != nil {
		return nil, err
	}
	var result []store.WorkerRecoveryDisposition
	for _, id := range workers {
		worker, err := qtx.GetWorkerByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if worker.LastHeartbeatAt.Valid && !worker.LastHeartbeatAt.Time.Before(cutoff.Time) {
			continue
		}
		rows, err := s.parkQueuedCustody(ctx, qtx, id, worker.SnapshotRegisterNonce, limit, locks, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("park stale queued custody: %w", err)
		}
		result = append(result, rows...)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
