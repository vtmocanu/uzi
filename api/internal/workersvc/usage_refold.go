package workersvc

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// RefoldRunUsage re-folds ONE pre-migration run's usage per SDK query() leg (PRD
// #1079 M3), replacing the collapsed (run_id, session_id, model) rows the old
// MAX-per-model key produced with correct per-(model, lineage_epoch) rows. It reuses
// M1's foldUsageFrames verbatim over the run's FULL persisted status/error history,
// so the refold and the incremental path are the same computation and cannot disagree
// about a frame's leg — both read the epoch from run_messages via
// CountRunInitFramesBefore, backed by 00188's idx_run_messages_init.
//
// It is a PACKAGE-LEVEL function, not a Service method, because foldUsageFrames is
// unexported (so the caller must live in this package) while workersvc.Service holds
// no pgx pool. The whole thing runs in ONE transaction: delete the run's old rows,
// re-fold the history, mark usage_refolded=true, commit. That atomicity is what makes
// a second pass (a second api replica, a re-boot) a no-op — the marker flips only when
// the rows are correct, and a post-migration run is born refolded so this is never
// called for a live run's own writes.
//
// A run with no result frames simply ends with no run_usage rows and the marker set.
// A GREATEST re-delivery of a straggler frame after this commits lands at the same
// position-absolute epoch and changes nothing (PRD D1).
func RefoldRunUsage(ctx context.Context, pool *pgxpool.Pool, q *store.Queries, run store.Run) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("refold usage (run %s): begin: %w", run.ID, err)
	}
	// Rollback is a no-op after a successful Commit; on any early return it undoes the
	// delete so a failed refold marks nothing and is retried on the next tick.
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := q.WithTx(tx)

	if err := qtx.DeleteRunUsage(ctx, run.ID); err != nil {
		return fmt.Errorf("refold usage (run %s): delete old rows: %w", run.ID, err)
	}

	rows, err := qtx.ListRunUsageFrames(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("refold usage (run %s): load frames: %w", run.ID, err)
	}

	frames := make([]IncomingMessage, len(rows))
	for i, m := range rows {
		frames[i] = IncomingMessage{
			Seq:           m.Seq,
			Kind:          m.Kind,
			Agent:         m.Agent.String,
			AgentInstance: m.AgentInstance.String,
			AgentLabel:    m.AgentLabel.String,
			Payload:       m.Payload,
		}
	}

	// The same fold body the incremental path runs, writing through the tx-bound
	// querier. Chat runs are skipped inside foldUsageFrames, but the refold never
	// selects one anyway (00188 left chat rows usage_refolded=true).
	if err := foldUsageFrames(ctx, qtx, run, frames); err != nil {
		return fmt.Errorf("refold usage (run %s): fold frames: %w", run.ID, err)
	}

	if err := qtx.MarkRunUsageRefolded(ctx, run.ID); err != nil {
		return fmt.Errorf("refold usage (run %s): mark refolded: %w", run.ID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("refold usage (run %s): commit: %w", run.ID, err)
	}
	return nil
}
