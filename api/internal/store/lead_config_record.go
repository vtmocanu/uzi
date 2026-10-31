package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel outcomes of RecordRunLeadConfig (PRD #3244 section 4).
var (
	// ErrLeadConfigClaimNotCurrent: the claim fence admitted nothing (stale generation, released
	// claim, another worker, terminal status, non-lead kind, review run or egress-bound run),
	// whether or not a row with the key exists. Nothing was written.
	ErrLeadConfigClaimNotCurrent = errors.New("lead config: claim is not current")
	// ErrLeadConfigKeyConflict: the fence admitted, the key exists and the stored body differs.
	// The first row is kept.
	ErrLeadConfigKeyConflict = errors.New("lead config: key exists with a different body")
	// ErrLeadConfigInvariant: the fence admitted and the insert hit an existing key, yet the
	// comparison found no stored row. Cannot happen (the run-row lock serializes writers and the
	// conflict was against a committed row); the transaction is rolled back and nothing is written.
	ErrLeadConfigInvariant = errors.New("lead config: duplicate key without a stored row")
)

// RecordRunLeadConfigParams is one worker-reported row. The server copies harness from the run, and
// reported_at is server receipt time, so neither is a parameter.
type RecordRunLeadConfigParams struct {
	RunID                  uuid.UUID
	WorkerID               uuid.UUID
	ClaimGeneration        int64
	Phase                  string
	PhaseEntry             int16
	Seq                    int16
	Truncated              bool
	RequestedModel         pgtype.Text
	RequestedModelSource   string
	RequestedEffort        pgtype.Text
	RequestedEffortMode    string
	RequestedEffortSource  string
	ObservedModel          pgtype.Text
	ObservedModelEvidence  pgtype.Text
	ObservedEffort         pgtype.Text
	ObservedEffortEvidence pgtype.Text
}

// RecordRunLeadConfigResult reports whether a new row was inserted (true) or the fence admitted an
// identical row that already existed (false).
type RecordRunLeadConfigResult struct {
	Inserted bool
}

// leadConfigTxQuerier is the two generated queries the write runs, so a store-package test can
// inject a statement-2 failure.
type leadConfigTxQuerier interface {
	InsertRunLeadConfigFenced(ctx context.Context, arg InsertRunLeadConfigFencedParams) (InsertRunLeadConfigFencedRow, error)
	RunLeadConfigSameBody(ctx context.Context, arg RunLeadConfigSameBodyParams) (bool, error)
}

// RecordRunLeadConfig writes one lead-config row under the claim fence (PRD #3244 section 4, D22).
// It is two statements in one explicit READ COMMITTED transaction: a single statement cannot decide
// the duplicate, because an identical retry that waited on the run-row lock sees the committed row
// through the unique index but not through its own start snapshot. The isolation level must stay
// READ COMMITTED: REPEATABLE READ would fail the waiting retry with SQLSTATE 40001. Like
// RecordLiveCheckpointPublishAttempt it requires a pool-owned top-level transaction.
func (q *Queries) RecordRunLeadConfig(ctx context.Context, p RecordRunLeadConfigParams) (RecordRunLeadConfigResult, error) {
	if q == nil {
		return RecordRunLeadConfigResult{}, errors.New("lead config record requires a pool")
	}
	pool, ok := q.db.(*pgxpool.Pool)
	if !ok || pool == nil {
		return RecordRunLeadConfigResult{}, errors.New("lead config record requires a pool")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RecordRunLeadConfigResult{}, err
	}
	return recordRunLeadConfigTx(ctx, tx, New(tx), p)
}

// recordRunLeadConfigTx runs the two statements on an already-begun transaction and always ends it:
// commit on an admitted outcome, rollback on everything else. A statement-1 error (including a
// 23514, returned as a wrapped *pgconn.PgError) never runs statement 2.
func recordRunLeadConfigTx(ctx context.Context, tx pgx.Tx, txq leadConfigTxQuerier, p RecordRunLeadConfigParams) (RecordRunLeadConfigResult, error) {
	defer func() { _ = tx.Rollback(ctx) }()
	ins, err := txq.InsertRunLeadConfigFenced(ctx, InsertRunLeadConfigFencedParams{
		RunID:                  p.RunID,
		WorkerID:               p.WorkerID,
		ClaimGeneration:        p.ClaimGeneration,
		Phase:                  p.Phase,
		PhaseEntry:             p.PhaseEntry,
		Seq:                    p.Seq,
		RequestedModel:         p.RequestedModel,
		RequestedModelSource:   p.RequestedModelSource,
		ObservedModel:          p.ObservedModel,
		ObservedModelEvidence:  p.ObservedModelEvidence,
		RequestedEffort:        p.RequestedEffort,
		RequestedEffortMode:    p.RequestedEffortMode,
		RequestedEffortSource:  p.RequestedEffortSource,
		ObservedEffort:         p.ObservedEffort,
		ObservedEffortEvidence: p.ObservedEffortEvidence,
		Truncated:              p.Truncated,
	})
	if err != nil {
		return RecordRunLeadConfigResult{}, fmt.Errorf("insert run lead config: %w", err)
	}
	if !ins.Fenced {
		return RecordRunLeadConfigResult{}, ErrLeadConfigClaimNotCurrent
	}
	if ins.Inserted {
		if err := tx.Commit(ctx); err != nil {
			return RecordRunLeadConfigResult{}, fmt.Errorf("commit run lead config: %w", err)
		}
		return RecordRunLeadConfigResult{Inserted: true}, nil
	}
	same, err := txq.RunLeadConfigSameBody(ctx, RunLeadConfigSameBodyParams{
		PhaseEntry:             p.PhaseEntry,
		Truncated:              p.Truncated,
		RequestedModelSource:   p.RequestedModelSource,
		RequestedEffortMode:    p.RequestedEffortMode,
		RequestedEffortSource:  p.RequestedEffortSource,
		RequestedModel:         p.RequestedModel,
		RequestedEffort:        p.RequestedEffort,
		ObservedModel:          p.ObservedModel,
		ObservedModelEvidence:  p.ObservedModelEvidence,
		ObservedEffort:         p.ObservedEffort,
		ObservedEffortEvidence: p.ObservedEffortEvidence,
		RunID:                  p.RunID,
		ClaimGeneration:        p.ClaimGeneration,
		Phase:                  p.Phase,
		Seq:                    p.Seq,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RecordRunLeadConfigResult{}, ErrLeadConfigInvariant
	}
	if err != nil {
		return RecordRunLeadConfigResult{}, fmt.Errorf("compare run lead config: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RecordRunLeadConfigResult{}, fmt.Errorf("commit run lead config: %w", err)
	}
	if !same {
		return RecordRunLeadConfigResult{}, ErrLeadConfigKeyConflict
	}
	return RecordRunLeadConfigResult{Inserted: false}, nil
}
