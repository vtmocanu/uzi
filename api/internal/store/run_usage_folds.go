package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxRunUsageFoldBatch caps how many run ids one MaterializeRunUsageFolds call accepts. The
// call holds row locks and a transaction open for its whole duration, so the batch stays
// small and bounded; a larger input is refused (ErrRunUsageFoldBatchTooLarge), never
// silently truncated.
const MaxRunUsageFoldBatch = 100

// ErrRunUsageFoldBatchTooLarge is returned when ids exceeds MaxRunUsageFoldBatch.
var ErrRunUsageFoldBatchTooLarge = errors.New("run usage fold batch exceeds the maximum size")

const (
	runUsageFoldLockTimeout      = "2s"
	runUsageFoldStatementTimeout = "5s"
	runUsageFoldRollbackTimeout  = time.Second
)

// RunUsageFoldBeginner is the slice of *pgxpool.Pool the materializer needs.
type RunUsageFoldBeginner interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

// runUsageFoldStepHook, when non-nil, runs after each materializer step with the step's
// name ("a0", "a", "b", "c", "d", "e"). It is nil in production; tests set it to pause or
// cancel the materializer at a deterministic point.
var runUsageFoldStepHook func(ctx context.Context, step string)

func runUsageFoldStepDone(ctx context.Context, step string) {
	if h := runUsageFoldStepHook; h != nil {
		h(ctx, step)
	}
}

// MaterializeRunUsageFolds (re)builds the stored per-(run, model) fold
// (run_usage_model_folds) for the eligible runs among ids, and marks their
// run_usage_fold_state rows fresh. It returns how many runs it folded. Eligible means a
// terminal (completed, failed, cancelled), non-chat run whose usage_refolded flag is set;
// anything else is skipped and left exactly as it was. A run whose EXISTING state row a
// concurrent writer or deleter holds locked is skipped too (it stays stale and a later call
// picks it up). A run for which an open writer transaction has CREATED the state row (the
// run had none) is not skipped: step (a) waits on that uncommitted row, and after the 2s
// lock_timeout the WHOLE batch fails with SQLSTATE 55P03, nothing is folded, and the caller
// retries. At most MaxRunUsageFoldBatch ids are accepted per call.
//
// Everything runs in ONE explicit READ COMMITTED transaction, and every step is its own
// statement (see the migration 00543 header for the lock protocol). Invariants this code
// must keep:
//
//   - Steps (b) and (d) are SEPARATE statements, never merged into one. Under READ
//     COMMITTED each statement takes a fresh snapshot, so (d) reads run_usage only after (b)
//     has locked the state rows: every writer commit before that lock is visible to (d),
//     and every writer that arrives later blocks in fn_run_usage_fold_lock on the locked
//     state row until this transaction ends, leaving the run stale afterwards. A single
//     merged statement would read run_usage with a snapshot older than the lock. The
//     isolation level is explicit so a changed pool default cannot silently turn this into
//     REPEATABLE READ, whose one snapshot would predate the lock and clear a stale mark over
//     a newer write.
//   - Stale therefore cannot be wrongly cleared: the state row is locked at (b) before (d)
//     reads, and every run_usage writer takes that same row (inside fn_run_usage_fold_lock)
//     before it writes.
//   - Nothing is visible before commit: an error or cancellation rolls back the deletes and
//     inserts of (c)/(d), leaving the previous fold rows and the stale flag untouched.
func MaterializeRunUsageFolds(ctx context.Context, pool RunUsageFoldBeginner, ids []uuid.UUID) (int64, error) {
	if len(ids) > MaxRunUsageFoldBatch {
		return 0, fmt.Errorf("materialize run usage folds: %d ids: %w (max %d)", len(ids), ErrRunUsageFoldBatchTooLarge, MaxRunUsageFoldBatch)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("materialize run usage folds: begin: %w", err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runUsageFoldRollbackTimeout)
		defer cancel()
		if rerr := tx.Rollback(rctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			slog.Debug("run usage fold rollback failed", "err", rerr)
		}
	}()

	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+runUsageFoldLockTimeout+"'"); err != nil {
		return 0, fmt.Errorf("materialize run usage folds: set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+runUsageFoldStatementTimeout+"'"); err != nil {
		return 0, fmt.Errorf("materialize run usage folds: set statement_timeout: %w", err)
	}

	q := New(tx)

	// (a0) eligible runs with no state row yet, key-share locked (skip locked).
	pNew, err := q.SelectRunUsageFoldNewCandidates(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("materialize run usage folds: select new candidates: %w", err)
	}
	runUsageFoldStepDone(ctx, "a0")

	// (a) create their state rows (stale by default).
	if len(pNew) > 0 {
		if err := q.InsertRunUsageFoldStates(ctx, pNew); err != nil {
			return 0, fmt.Errorf("materialize run usage folds: insert states: %w", err)
		}
	}
	runUsageFoldStepDone(ctx, "a")

	// (b) lock the eligible state rows; a row a writer holds is skipped.
	locked, err := q.LockRunUsageFoldStates(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("materialize run usage folds: lock states: %w", err)
	}
	runUsageFoldStepDone(ctx, "b")

	if len(locked) > 0 {
		// (c) drop the previous fold rows.
		if err := q.DeleteRunUsageModelFolds(ctx, locked); err != nil {
			return 0, fmt.Errorf("materialize run usage folds: delete old folds: %w", err)
		}
		runUsageFoldStepDone(ctx, "c")

		// (d) re-fold from the view, in its own statement after (b).
		if _, err := q.InsertRunUsageModelFolds(ctx, locked); err != nil {
			return 0, fmt.Errorf("materialize run usage folds: insert folds: %w", err)
		}
		runUsageFoldStepDone(ctx, "d")

		// (e) mark fresh.
		if err := q.MarkRunUsageFoldStatesFresh(ctx, locked); err != nil {
			return 0, fmt.Errorf("materialize run usage folds: mark fresh: %w", err)
		}
		runUsageFoldStepDone(ctx, "e")
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("materialize run usage folds: commit: %w", err)
	}
	return int64(len(locked)), nil
}
