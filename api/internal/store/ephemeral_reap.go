package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// TxBeginner is the slice of *pgxpool.Pool the reaper needs: it opens the one transaction both
// reap statements run in.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ReapEphemeralWorkers is the orphan/failure GC for ephemeral workers (PRD #529 M5, Decision 6),
// extended for the ephemeral lease (PRD #2006). It runs LockReapableEphemeralWorkers and then
// DeleteLockedEphemeralWorkers in ONE transaction: the first locks the selected rows FOR UPDATE
// SKIP LOCKED, the second deletes them by id re-checking the same predicate on a fresh snapshot
// taken after the locks are held, so a worker a concurrent claim, lease entry or terminal report
// just changed is judged on its current state. deadlineCutoff is the provision-deadline cutoff;
// lease is the operator's lease interval (an invalid or zero interval means no lease is live, so
// the selection is exactly the pre-lease one). It returns the number of workers deleted.
func ReapEphemeralWorkers(ctx context.Context, db TxBeginner, deadlineCutoff pgtype.Timestamptz, lease pgtype.Interval) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin ephemeral reap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)
	ids, err := q.LockReapableEphemeralWorkers(ctx, LockReapableEphemeralWorkersParams{
		EphemeralLease: lease,
		DeadlineCutoff: deadlineCutoff,
	})
	if err != nil {
		return 0, fmt.Errorf("lock reapable ephemeral workers: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := q.DeleteLockedEphemeralWorkers(ctx, DeleteLockedEphemeralWorkersParams{
		Ids:            ids,
		EphemeralLease: lease,
		DeadlineCutoff: deadlineCutoff,
	})
	if err != nil {
		return 0, fmt.Errorf("delete locked ephemeral workers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit ephemeral reap: %w", err)
	}
	return n, nil
}
