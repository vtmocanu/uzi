package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RecordLiveCheckpointPublishAttemptParams binds admission to the claim captured
// at Publish authorization. AttemptID optionally names an unsent prepared row.
type RecordLiveCheckpointPublishAttemptParams struct {
	RecordCheckpointPublishAttemptParams
	ExpectedWorkerID        pgtype.UUID
	ExpectedClaimGeneration int64
	AttemptID               uuid.UUID
}

// RecordLiveCheckpointPublishAttempt commits evidence under the run lock before
// any forge I/O. It requires a pool-owned top-level transaction; a caller's
// transaction or savepoint cannot guarantee committed evidence to exhaustion.
func (q *Queries) RecordLiveCheckpointPublishAttempt(ctx context.Context, p RecordLiveCheckpointPublishAttemptParams) (uuid.UUID, error) {
	if q == nil {
		return uuid.Nil, errors.New("checkpoint admission requires a pool")
	}
	pool, ok := q.db.(*pgxpool.Pool)
	if !ok || pool == nil {
		return uuid.Nil, errors.New("checkpoint admission requires a pool")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked := New(tx)
	if _, err := locked.GetRunByIDForUpdate(ctx, p.RunID); err != nil {
		return uuid.Nil, err
	}
	if !p.ExpectedWorkerID.Valid {
		return uuid.Nil, pgx.ErrNoRows
	}
	if _, err := locked.CheckLiveCheckpointPublishAdmission(ctx, CheckLiveCheckpointPublishAdmissionParams{
		RunID: p.RunID, ExpectedWorkerID: uuid.UUID(p.ExpectedWorkerID.Bytes), ExpectedClaimGeneration: p.ExpectedClaimGeneration,
	}); err != nil {
		return uuid.Nil, err
	}
	id := p.AttemptID
	if id != uuid.Nil {
		row, err := locked.GetCheckpointPublishAttempt(ctx, id)
		if err != nil {
			return uuid.Nil, err
		}
		if row.RunID != p.RunID || row.Branch != p.Branch || row.Ref != p.Ref || row.Tip != p.Tip {
			return uuid.Nil, errors.New("checkpoint prepared attempt does not match publish")
		}
	} else {
		id, err = locked.RecordCheckpointPublishAttempt(ctx, p.RecordCheckpointPublishAttemptParams)
		if err != nil {
			return uuid.Nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
