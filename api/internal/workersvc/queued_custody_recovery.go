package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type custodyTransactionRowsReader interface {
	CustodyHoldLimitFromRows([]store.AppSetting) (int32, error)
}

// queuedCustodyCachedLimit supplies a cache comparison, never admission evidence.
func (s *Service) queuedCustodyCachedLimit(ctx context.Context) int32 {
	if _, ok := s.p.CustodyHoldLimit.(custodyTransactionRowsReader); !ok {
		return 0
	}
	n, err := s.custodyLimit(ctx)
	if err != nil {
		slog.Warn("queued recovery: custody setting unavailable", "error", err)
		return 0
	}
	return n
}

// Positive supported queued admission takes the settings lock before any savepoint
// or worker lock. Disabled admission performs no settings DB work. A recoverable
// settings statement failure disables classification after a
// successful savepoint rollback; transaction management, context and connection
// failures still propagate, so liveness is not guaranteed on those failures.
// Only fresh transaction rows authorize admission; a stale cache never does.
func (s *Service) queuedCustodyAdmission(ctx context.Context, tx pgx.Tx, cached int32) (int32, error) {
	reader, ok := s.p.CustodyHoldLimit.(custodyTransactionRowsReader)
	if !ok || cached <= 0 {
		return 0, nil
	}
	if err := store.LockSettingsAdmission(ctx, tx); err != nil {
		return 0, err
	}
	settingsTx, err := tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := store.New(settingsTx).ListAppSettings(ctx)
	if err != nil {
		// One cleanup attempt, bounded independently of a canceled request.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
		defer cancel()
		if rollbackErr := settingsTx.Rollback(cleanup); rollbackErr != nil {
			return 0, errors.Join(err, rollbackErr)
		}
		var sqlErr *pgconn.PgError
		if ctx.Err() != nil || tx.Conn().IsClosed() || !errors.As(err, &sqlErr) ||
			sqlErr.Severity != "ERROR" || len(sqlErr.Code) != 5 || sqlErr.Code[:2] == "08" {
			return 0, err
		}
		slog.Warn("queued recovery: custody settings statement failed")
		return 0, nil
	}
	if err := settingsTx.Commit(ctx); err != nil {
		return 0, err
	}
	fresh, err := reader.CustodyHoldLimitFromRows(rows)
	if err != nil {
		slog.Warn("queued recovery: custody setting unavailable", "error", err)
		return 0, nil
	}
	if cached <= 0 || fresh > 1000 || cached != fresh {
		slog.Warn("queued recovery: custody setting cache disagrees with transaction")
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
func (s *Service) parkQueuedCustody(ctx context.Context, qtx *store.Queries, workerID uuid.UUID, nonce pgtype.Text, limit int32, locks workerRecoveryLockSet, requeuedIDs []uuid.UUID) ([]store.WorkerRecoveryDisposition, error) {
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
		FrozenTargets: frozen, LockedParentIds: parents, CustodyHoldLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	return s.parkQueuedCustodyWithHolds(ctx, qtx, workerID, nonce, limit, frozen, parents, holdIDs, s.now())
}

func (s *Service) parkQueuedCustodyWithHolds(ctx context.Context, qtx *store.Queries, workerID uuid.UUID, nonce pgtype.Text, limit int32, frozen []byte, parents, holdIDs []uuid.UUID, now time.Time) ([]store.WorkerRecoveryDisposition, error) {
	rows, err := qtx.ParkFrozenQueuedCustodyExhausted(ctx, store.ParkFrozenQueuedCustodyExhaustedParams{
		WorkerID: pgconv.UUID(workerID), ReleasedWorkerNonce: nonce,
		FrozenTargets: frozen, LockedParentIds: parents, CustodyHoldLimit: limit,
		LockedHoldIds:       holdIDs,
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

// parkStaleQueuedCustody discovers at most 32 targets before each transaction.
// Each page locks settings, all workers, sorted parents, then globally sorted
// holds. Errors stop later pages; committed pages publish immediately and count.
// The local UUID cursor and creation fence bound this sweep's traversal; memory
// and lock acquisition are bounded per batch, not by a fleet-sized collection.
func (s *Service) parkStaleQueuedCustody(ctx context.Context, cutoff pgtype.Timestamptz, sweepStarted time.Time) (int64, error) {
	if s.txBeginner == nil {
		return 0, nil
	}
	q, ok := s.q.(*store.Queries)
	if !ok {
		return 0, nil
	}
	var count int64
	var cursor uuid.UUID
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		limit := s.queuedCustodyCachedLimit(ctx)
		if limit <= 0 {
			return count, nil
		}
		discovered, err := q.DiscoverStaleQueuedCustodyTargets(ctx, store.DiscoverStaleQueuedCustodyTargetsParams{
			AfterID: cursor, SweepStarted: pgconv.Time(sweepStarted), Cutoff: cutoff,
			MaxRequeues:      int32(s.p.RunMaxRequeues), //nolint:gosec // bounded configuration, as in active-run recovery.
			CustodyHoldLimit: limit,
		})
		if err != nil {
			return count, err
		}
		if len(discovered) == 0 {
			return count, nil
		}
		cursor = discovered[len(discovered)-1].ID
		ids := make([]uuid.UUID, 0, len(discovered))
		workers := make([]uuid.UUID, 0, len(discovered))
		frozen := make([]json.RawMessage, 0, len(discovered))
		seen := make(map[uuid.UUID]bool)
		for _, row := range discovered {
			ids = append(ids, row.ID)
			frozen = append(frozen, json.RawMessage(row.FrozenTarget))
			if !seen[row.WorkerID.Bytes] {
				workers = append(workers, row.WorkerID.Bytes)
				seen[row.WorkerID.Bytes] = true
			}
		}
		captured, err := json.Marshal(frozen)
		if err != nil {
			return count, err
		}
		rows, err := s.parkStaleQueuedCustodyBatch(ctx, cutoff, sweepStarted, ids, workers, captured)
		if err != nil {
			return count, err
		}
		for _, row := range rows {
			s.publishSwept(row.ID, row.Status)
		}
		count += int64(len(rows))
	}
}

func (s *Service) parkStaleQueuedCustodyBatch(ctx context.Context, cutoff pgtype.Timestamptz, sweepStarted time.Time, ids, workerIDs []uuid.UUID, captured []byte) ([]store.WorkerRecoveryDisposition, error) {
	cachedLimit := s.queuedCustodyCachedLimit(ctx)
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	limit, err := s.queuedCustodyAdmission(ctx, tx, cachedLimit)
	if err != nil || limit == 0 {
		return nil, err
	}
	qtx := store.New(tx)
	workers, err := qtx.LockStaleQueuedCustodyWorkers(ctx, store.LockStaleQueuedCustodyWorkersParams{WorkerIds: workerIDs, Cutoff: cutoff})
	if err != nil {
		return nil, err
	}
	lockedWorkerIDs := make([]uuid.UUID, 0, len(workers))
	for _, worker := range workers {
		lockedWorkerIDs = append(lockedWorkerIDs, worker.ID)
	}
	frozenRows, err := qtx.LockWorkerRecoveryParents(ctx, store.LockWorkerRecoveryParentsParams{BoundedTargetIds: ids, AdditionalWorkerIds: lockedWorkerIDs})
	if err != nil {
		return nil, err
	}
	locks, err := workerRecoveryLocksFromRows(frozenRows)
	if err != nil {
		return nil, err
	}
	// Keep discovery evidence, never replace it with the post-wait capture.
	locks.frozenJSON = string(captured)
	locks, err = validateQueuedCustodyLocks(ctx, qtx, locks)
	if err != nil {
		return nil, err
	}
	holdIDs, err := qtx.LockFrozenQueuedCustodyHolds(ctx, store.LockFrozenQueuedCustodyHoldsParams{
		FrozenTargets: locks.queuedJSON, LockedParentIds: locks.parentIDs, CustodyHoldLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	result := make([]store.WorkerRecoveryDisposition, 0, len(ids))
	for _, worker := range workers {
		rows, err := s.parkQueuedCustodyWithHolds(ctx, qtx, worker.ID, worker.SnapshotRegisterNonce, limit, locks.queuedJSON, locks.parentIDs, holdIDs, sweepStarted)
		if err != nil {
			return nil, err
		}
		result = append(result, rows...)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// queuedCustodyCandidates applies the same guards before the bounded queued-only
// discovery. The mutation still revalidates all evidence under the row locks.
func (s *Service) queuedCustodyCandidates(limit int32) store.LockWorkerRecoveryParentsParams {
	return store.LockWorkerRecoveryParentsParams{
		CustodyHoldLimit:    limit,
		MaxRequeues:         int32(s.p.RunMaxRequeues), //nolint:gosec // bounded server configuration.
		SnapshotFreshCutoff: pgconv.Time(s.now().Add(-(s.p.WorkerHeartbeatStale + s.p.WorkerHeartbeatInterval))),
	}
}
