package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

type FrozenFailWorkerRunsOverCapParams struct {
	// Nil means use the locked current incarnation; non-nil can capture a NULL nonce.
	ReleasedWorkerNonceOverride *pgtype.Text `json:"-"`
	FailureReason               pgtype.Text  `json:"failure_reason"`
	WorkerID                    pgtype.UUID  `json:"worker_id"`
	MaxRequeues                 int32        `json:"max_requeues"`
	FrozenTargets               []byte       `json:"frozen_targets"`
	LockedParentIds             []uuid.UUID  `json:"locked_parent_ids"`
}

func (q *Queries) FrozenFailWorkerRunsOverCap(ctx context.Context, arg FrozenFailWorkerRunsOverCapParams) ([]WorkerRecoveryDisposition, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]WorkerRecoveryDisposition, error) {
		ids, err := qtx.LockFrozenFailWorkerRunsOverCap(ctx, LockFrozenFailWorkerRunsOverCapParams{WorkerID: arg.WorkerID, MaxRequeues: arg.MaxRequeues, FrozenTargets: arg.FrozenTargets, LockedParentIds: arg.LockedParentIds})
		if err != nil {
			return nil, err
		}
		evidence, err := qtx.classifyWorkerExhaustion(ctx, ids, arg.ReleasedWorkerNonceOverride)
		if err != nil {
			return nil, err
		}
		rows, err := qtx.frozenFailWorkerRunsOverCapLocked(ctx, frozenFailWorkerRunsOverCapLockedParams{ExhaustionEvidence: evidence, FailureReason: arg.FailureReason, WorkerID: arg.WorkerID, MaxRequeues: arg.MaxRequeues, FrozenTargets: arg.FrozenTargets, LockedParentIds: arg.LockedParentIds})
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

type FrozenFailAttestedFinalizeRunsOverCapParams struct {
	// Nil means use the locked current incarnation; non-nil can capture a NULL nonce.
	ReleasedWorkerNonceOverride *pgtype.Text `json:"-"`
	FailureReason               pgtype.Text  `json:"failure_reason"`
	WorkerID                    pgtype.UUID  `json:"worker_id"`
	RunIds                      []uuid.UUID  `json:"run_ids"`
	ClaimGenerations            []int64      `json:"claim_generations"`
	MaxRequeues                 int32        `json:"max_requeues"`
	FrozenTargets               []byte       `json:"frozen_targets"`
	LockedParentIds             []uuid.UUID  `json:"locked_parent_ids"`
}

func (q *Queries) FrozenFailAttestedFinalizeRunsOverCap(ctx context.Context, arg FrozenFailAttestedFinalizeRunsOverCapParams) ([]WorkerRecoveryDisposition, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]WorkerRecoveryDisposition, error) {
		ids, err := qtx.LockFrozenFailAttestedFinalizeRunsOverCap(ctx, LockFrozenFailAttestedFinalizeRunsOverCapParams{WorkerID: arg.WorkerID, RunIds: arg.RunIds, ClaimGenerations: arg.ClaimGenerations, MaxRequeues: arg.MaxRequeues, FrozenTargets: arg.FrozenTargets, LockedParentIds: arg.LockedParentIds})
		if err != nil {
			return nil, err
		}
		evidence, err := qtx.classifyWorkerExhaustion(ctx, ids, arg.ReleasedWorkerNonceOverride)
		if err != nil {
			return nil, err
		}
		rows, err := qtx.frozenFailAttestedFinalizeRunsOverCapLocked(ctx, frozenFailAttestedFinalizeRunsOverCapLockedParams{ExhaustionEvidence: evidence, FailureReason: arg.FailureReason, WorkerID: arg.WorkerID, RunIds: arg.RunIds, ClaimGenerations: arg.ClaimGenerations, MaxRequeues: arg.MaxRequeues, FrozenTargets: arg.FrozenTargets, LockedParentIds: arg.LockedParentIds})
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

type FrozenFailRunsMissingFromSnapshotParams struct {
	FailureReason        pgtype.Text        `json:"failure_reason"`
	WorkerID             pgtype.UUID        `json:"worker_id"`
	MissingCutoff        pgtype.Timestamptz `json:"missing_cutoff"`
	MaxRequeues          int32              `json:"max_requeues"`
	Now                  pgtype.Timestamptz `json:"now"`
	GlobalTimeoutSeconds int32              `json:"global_timeout_seconds"`
	FrozenTargets        []byte             `json:"frozen_targets"`
	LockedParentIds      []uuid.UUID        `json:"locked_parent_ids"`
}

type FrozenFailRunsMissingFromSnapshotRow = frozenFailRunsMissingFromSnapshotLockedRow

func (q *Queries) FrozenFailRunsMissingFromSnapshot(ctx context.Context, arg FrozenFailRunsMissingFromSnapshotParams) ([]FrozenFailRunsMissingFromSnapshotRow, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]FrozenFailRunsMissingFromSnapshotRow, error) {
		ids, err := qtx.LockFrozenFailRunsMissingFromSnapshot(ctx, LockFrozenFailRunsMissingFromSnapshotParams{WorkerID: arg.WorkerID, MissingCutoff: arg.MissingCutoff, MaxRequeues: arg.MaxRequeues, Now: arg.Now, GlobalTimeoutSeconds: arg.GlobalTimeoutSeconds, FrozenTargets: arg.FrozenTargets, LockedParentIds: arg.LockedParentIds})
		if err != nil {
			return nil, err
		}
		evidence, err := qtx.classifyWorkerExhaustion(ctx, ids, nil)
		if err != nil {
			return nil, err
		}
		return qtx.frozenFailRunsMissingFromSnapshotLocked(ctx, frozenFailRunsMissingFromSnapshotLockedParams{ExhaustionEvidence: evidence, FailureReason: arg.FailureReason, WorkerID: arg.WorkerID, MissingCutoff: arg.MissingCutoff, MaxRequeues: arg.MaxRequeues, Now: arg.Now, GlobalTimeoutSeconds: arg.GlobalTimeoutSeconds, FrozenTargets: arg.FrozenTargets, LockedParentIds: arg.LockedParentIds})
	})
}
