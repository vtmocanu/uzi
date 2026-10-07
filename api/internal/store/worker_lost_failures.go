package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// withWorkerLostLocks opens a transaction (a savepoint when db is already pgx.Tx).
// The lock statement finishes before the writer reads recovery_custody_holds, so
// a report that held the run lock is visible in the writer's READ COMMITTED snapshot.
// Failure rolls back this unit; an outer transaction retains its existing locks.
func withWorkerLostLocks[T any](ctx context.Context, q *Queries, write func(*Queries) (T, error)) (T, error) {
	var zero T
	beginner, ok := q.db.(TxBeginner)
	if !ok {
		return zero, errors.New("worker-lost failure transaction unavailable")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := write(New(tx))
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	return result, nil
}

type FailRunsOfStaleWorkersOverCapParams struct {
	FailureReason pgtype.Text        `json:"failure_reason"`
	MaxRequeues   int32              `json:"max_requeues"`
	FailCutoff    pgtype.Timestamptz `json:"fail_cutoff"`
}

type FailRunsOfStaleWorkersOverCapRow = failRunsOfStaleWorkersOverCapLockedRow

// FailRunsOfStaleWorkersOverCap locks workers before runs and rechecks the failure predicate on a fresh snapshot.
func (q *Queries) FailRunsOfStaleWorkersOverCap(ctx context.Context, arg FailRunsOfStaleWorkersOverCapParams) ([]FailRunsOfStaleWorkersOverCapRow, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]FailRunsOfStaleWorkersOverCapRow, error) {
		ids, err := qtx.LockFailRunsOfStaleWorkersOverCap(ctx, LockFailRunsOfStaleWorkersOverCapParams{MaxRequeues: arg.MaxRequeues, FailCutoff: arg.FailCutoff})
		if err != nil {
			return nil, err
		}
		evidence, err := qtx.classifyWorkerExhaustion(ctx, ids, nil)
		if err != nil {
			return nil, err
		}
		return qtx.failRunsOfStaleWorkersOverCapLocked(ctx, failRunsOfStaleWorkersOverCapLockedParams{FailureReason: arg.FailureReason, MaxRequeues: arg.MaxRequeues, FailCutoff: arg.FailCutoff, LockedRunIds: ids, ExhaustionEvidence: evidence})
	})
}

type FailWorkerRunsOverCapParams struct {
	// Nil means use the locked current incarnation; non-nil can capture a NULL nonce.
	ReleasedWorkerNonceOverride *pgtype.Text `json:"-"`
	FailureReason               pgtype.Text  `json:"failure_reason"`
	WorkerID                    pgtype.UUID  `json:"worker_id"`
	MaxRequeues                 int32        `json:"max_requeues"`
}

// FailWorkerRunsOverCap locks workers before runs and rechecks the failure predicate on a fresh snapshot.
func (q *Queries) FailWorkerRunsOverCap(ctx context.Context, arg FailWorkerRunsOverCapParams) ([]WorkerRecoveryDisposition, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]WorkerRecoveryDisposition, error) {
		if arg.WorkerID.Valid {
			if _, err := qtx.GetWorkerForUpdate(ctx, uuid.UUID(arg.WorkerID.Bytes)); err != nil {
				return nil, err
			}
		}
		ids, err := qtx.LockFailWorkerRunsOverCap(ctx, LockFailWorkerRunsOverCapParams{WorkerID: arg.WorkerID, MaxRequeues: arg.MaxRequeues})
		if err != nil {
			return nil, err
		}
		evidence, err := qtx.classifyWorkerExhaustion(ctx, ids, arg.ReleasedWorkerNonceOverride)
		if err != nil {
			return nil, err
		}
		rows, err := qtx.failWorkerRunsOverCapLocked(ctx, failWorkerRunsOverCapLockedParams{FailureReason: arg.FailureReason, WorkerID: arg.WorkerID, MaxRequeues: arg.MaxRequeues, LockedRunIds: ids, ExhaustionEvidence: evidence})
		if err != nil {
			return nil, err
		}
		result := make([]WorkerRecoveryDisposition, 0, len(rows))
		for _, row := range rows {
			result = append(result, WorkerRecoveryDisposition(row))
		}
		return result, nil

	})
}

type FailAttestedFinalizeRunsOverCapParams struct {
	// Nil means use the locked current incarnation; non-nil can capture a NULL nonce.
	ReleasedWorkerNonceOverride *pgtype.Text `json:"-"`
	FailureReason               pgtype.Text  `json:"failure_reason"`
	WorkerID                    pgtype.UUID  `json:"worker_id"`
	RunIds                      []uuid.UUID  `json:"run_ids"`
	ClaimGenerations            []int64      `json:"claim_generations"`
	MaxRequeues                 int32        `json:"max_requeues"`
}

// FailAttestedFinalizeRunsOverCap locks workers before runs and rechecks the failure predicate on a fresh snapshot.
func (q *Queries) FailAttestedFinalizeRunsOverCap(ctx context.Context, arg FailAttestedFinalizeRunsOverCapParams) ([]WorkerRecoveryDisposition, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]WorkerRecoveryDisposition, error) {
		if arg.WorkerID.Valid {
			if _, err := qtx.GetWorkerForUpdate(ctx, uuid.UUID(arg.WorkerID.Bytes)); err != nil {
				return nil, err
			}
		}
		ids, err := qtx.LockFailAttestedFinalizeRunsOverCap(ctx, LockFailAttestedFinalizeRunsOverCapParams{WorkerID: arg.WorkerID, RunIds: arg.RunIds, ClaimGenerations: arg.ClaimGenerations, MaxRequeues: arg.MaxRequeues})
		if err != nil {
			return nil, err
		}
		evidence, err := qtx.classifyWorkerExhaustion(ctx, ids, arg.ReleasedWorkerNonceOverride)
		if err != nil {
			return nil, err
		}
		rows, err := qtx.failAttestedFinalizeRunsOverCapLocked(ctx, failAttestedFinalizeRunsOverCapLockedParams{FailureReason: arg.FailureReason, WorkerID: arg.WorkerID, RunIds: arg.RunIds, ClaimGenerations: arg.ClaimGenerations, MaxRequeues: arg.MaxRequeues, LockedRunIds: ids, ExhaustionEvidence: evidence})
		if err != nil {
			return nil, err
		}
		result := make([]WorkerRecoveryDisposition, 0, len(rows))
		for _, row := range rows {
			result = append(result, WorkerRecoveryDisposition(row))
		}
		return result, nil

	})
}

type FailRunsMissingFromSnapshotParams struct {
	FailureReason        pgtype.Text        `json:"failure_reason"`
	WorkerID             pgtype.UUID        `json:"worker_id"`
	MissingCutoff        pgtype.Timestamptz `json:"missing_cutoff"`
	MaxRequeues          int32              `json:"max_requeues"`
	Now                  pgtype.Timestamptz `json:"now"`
	GlobalTimeoutSeconds int32              `json:"global_timeout_seconds"`
}

type FailRunsMissingFromSnapshotRow = failRunsMissingFromSnapshotLockedRow

// FailRunsMissingFromSnapshot locks workers before runs and rechecks the failure predicate on a fresh snapshot.
func (q *Queries) FailRunsMissingFromSnapshot(ctx context.Context, arg FailRunsMissingFromSnapshotParams) ([]FailRunsMissingFromSnapshotRow, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]FailRunsMissingFromSnapshotRow, error) {
		if arg.WorkerID.Valid {
			if _, err := qtx.GetWorkerForUpdate(ctx, uuid.UUID(arg.WorkerID.Bytes)); err != nil {
				return nil, err
			}
		}
		ids, err := qtx.LockFailRunsMissingFromSnapshot(ctx, LockFailRunsMissingFromSnapshotParams{WorkerID: arg.WorkerID, MissingCutoff: arg.MissingCutoff, MaxRequeues: arg.MaxRequeues, Now: arg.Now, GlobalTimeoutSeconds: arg.GlobalTimeoutSeconds})
		if err != nil {
			return nil, err
		}
		evidence, err := qtx.classifyWorkerExhaustion(ctx, ids, nil)
		if err != nil {
			return nil, err
		}
		return qtx.failRunsMissingFromSnapshotLocked(ctx, failRunsMissingFromSnapshotLockedParams{FailureReason: arg.FailureReason, WorkerID: arg.WorkerID, MissingCutoff: arg.MissingCutoff, MaxRequeues: arg.MaxRequeues, Now: arg.Now, GlobalTimeoutSeconds: arg.GlobalTimeoutSeconds, LockedRunIds: ids, ExhaustionEvidence: evidence})
	})
}
