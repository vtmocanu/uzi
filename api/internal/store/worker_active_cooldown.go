package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// WithWorkerActiveCooldownSavepoint isolates one optional cooldown projection from
// already validated snapshot custody. The caller must clear prior display metadata
// before this savepoint. Recoverable callback failures leave that metadata NULL
// and allow sibling entries to proceed; begin, rollback and release failures
// propagate because the outer transaction may be unusable.
// A transaction-bound Queries is required: a pool must never open a separate
// transaction here. There is one attempt, bounded by ctx; rollback gets a separate
// 250ms cleanup budget even when ctx has been canceled.
func (q *Queries) WithWorkerActiveCooldownSavepoint(ctx context.Context, project func(*Queries) error) error {
	outer, ok := q.db.(pgx.Tx)
	if !ok {
		return errors.New("active cooldown savepoint requires a transaction")
	}
	tx, err := outer.Begin(ctx)
	if err != nil {
		return err
	}
	if err := project(New(tx)); err != nil {
		// Do not log callback errors: they can contain SQL or provider secrets.
		slog.Warn("optional active cooldown projection failed")
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
		defer cancel()
		// pgx closes the nested handle on rollback, including a failed rollback.
		// Propagate that failure rather than retrying on the closed handle.
		return tx.Rollback(cleanup)
	}
	return tx.Commit(ctx)
}
