// Package historyretention is the opt-in sweeper pass that deletes recorded history older than
// UZI_HISTORY_RETENTION (issue #4695): ordinary pr_history_events rows (never first_merge ones)
// and the anthropic, codex and worker capacity sample tables. pr_history_heads is never touched.
//
// One tick takes a cross-replica session advisory lock, reads one cutoff from the database
// clock, deletes at most one adaptive batch (at most 1000 rows) per table, and releases the
// lock. Every database call is bound to a 2 s client budget (the work context ends 250 ms before
// the cleanup context, which ends at the budget), but the pass's actual return time also depends
// on the driver and on scheduling, so it is measured, not guaranteed.
package historyretention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	// totalBudget is the client budget of one tick (parent deadline D = start + totalBudget).
	totalBudget = 2 * time.Second
	// cleanupReserve is held back from the work context: work runs until D - cleanupReserve, the
	// unlock and the close run under the cleanup context until D.
	cleanupReserve = 250 * time.Millisecond
	// Server-side bounds per statement, on top of the client deadlines.
	statementTimeout = 300 * time.Millisecond
	lockTimeout      = 200 * time.Millisecond

	maxBatch = 1000
	minBatch = 50
	// warnAfterTimeouts is the number of consecutive statement timeouts on one table after which
	// a warning is logged (and again at every further multiple).
	warnAfterTimeouts = 5
)

// session is the slice of store.HistoryRetentionSession the pass uses; tests fake it.
type session interface {
	Cutoff(ctx context.Context, retention time.Duration) (reference, cutoff time.Time, err error)
	DeleteBatch(ctx context.Context, table store.HistoryRetentionTable, cutoff time.Time, limit int32) (int64, error)
	Idle() bool
	Close(ctx context.Context) (unlocked bool, err error)
}

// opener opens the pinned session; (nil, false, nil) means another replica holds the lock.
type opener func(workCtx, cleanupCtx context.Context) (session, bool, error)

type tableState struct {
	batch    int32
	timeouts int
}

// Retention runs the history_retention sweeper pass.
type Retention struct {
	retention time.Duration
	open      opener
	now       func() time.Time

	mu      sync.Mutex
	counter uint64
	tables  map[store.HistoryRetentionTable]*tableState
}

// New builds the production pass over pool. A retention of 0 is off: Pass returns (0, nil)
// without touching the database.
func New(pool *pgxpool.Pool, retention time.Duration) *Retention {
	open := func(workCtx, cleanupCtx context.Context) (session, bool, error) {
		s, ok, err := store.OpenHistoryRetentionSession(workCtx, cleanupCtx, pool, statementTimeout, lockTimeout)
		if err != nil || !ok {
			return nil, false, err
		}
		return s, true, nil
	}
	// The starting table is random so that no replica restart pattern can permanently skip one.
	return newRetention(open, time.Now, retention, rand.Uint64()) //nolint:gosec // rotation start, not a secret
}

func newRetention(open opener, now func() time.Time, retention time.Duration, counter uint64) *Retention {
	r := &Retention{
		retention: retention, open: open, now: now, counter: counter,
		tables: map[store.HistoryRetentionTable]*tableState{},
	}
	for _, t := range store.HistoryRetentionTables() {
		r.tables[t] = &tableState{batch: maxBatch}
	}
	return r
}

// Pass is the sweeper pass: it returns the number of rows deleted (confirmed counts only) and an
// error joining every failed table and unknown outcome. It always closes the session it opened.
func (r *Retention) Pass(ctx context.Context) (int64, error) {
	if r.retention <= 0 {
		return 0, nil
	}
	deadline := r.now().Add(totalBudget)
	workDeadline := deadline.Add(-cleanupReserve)
	cleanupCtx, cancelCleanup := context.WithDeadline(ctx, deadline)
	defer cancelCleanup()
	workCtx, cancelWork := context.WithDeadline(cleanupCtx, workDeadline)
	defer cancelWork()

	sess, ok, err := r.open(workCtx, cleanupCtx)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}

	r.mu.Lock()
	start := r.counter
	r.counter++ // every lock-holding tick advances the rotation, failed ones included
	r.mu.Unlock()

	var (
		total int64
		errs  []error
	)
	canWork := func() bool { return workCtx.Err() == nil && r.now().Before(workDeadline) }

	if canWork() {
		total, errs = r.run(workCtx, sess, start, canWork)
	}

	cancelWork()
	if _, cerr := sess.Close(cleanupCtx); cerr != nil {
		errs = append(errs, cerr)
	}
	return total, errors.Join(errs...)
}

// run reads the cutoff and visits the tables from the rotated start. It ends the tick at a
// client or connection error or a non-idle session, and continues past a known rollback.
func (r *Retention) run(ctx context.Context, sess session, start uint64, canWork func() bool) (int64, []error) {
	var (
		total int64
		errs  []error
	)
	_, cutoff, err := sess.Cutoff(ctx, r.retention)
	if err != nil {
		return 0, []error{fmt.Errorf("history retention: cutoff: %w", err)}
	}
	tables := store.HistoryRetentionTables()
	for i := 0; i < len(tables) && canWork(); i++ {
		table := tables[(start+uint64(i))%uint64(len(tables))]
		st := r.state(table)
		n, derr := sess.DeleteBatch(ctx, table, cutoff, st.batch)
		if derr == nil {
			total += n
			r.succeeded(table)
			continue
		}
		var rb *store.HistoryRetentionRollbackError
		if errors.As(derr, &rb) {
			// Known rollback: 0 rows. Only a statement timeout says the batch was too large.
			if rb.StatementTimeout() {
				r.timedOut(table)
			}
			errs = append(errs, fmt.Errorf("history retention: %s: %w", table, derr))
			continue
		}
		// Unknown outcome: the batch may or may not have committed. Only confirmed counts are
		// reported; the table name is logged and named in the returned error.
		slog.Warn("history retention: unknown delete outcome", "table", table, "error", secretscrub.Scrub(derr.Error()))
		errs = append(errs, fmt.Errorf("history retention: %s: unknown outcome: %w", table, derr))
		var pgErr *pgconn.PgError
		if !sess.Idle() || !errors.As(derr, &pgErr) {
			break // client or connection error, or a session that cannot take another statement
		}
	}
	return total, errs
}

func (r *Retention) state(table store.HistoryRetentionTable) tableState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.tables[table]
}

// succeeded resets the timeout streak and doubles the batch back toward maxBatch.
func (r *Retention) succeeded(table store.HistoryRetentionTable) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.tables[table]
	st.timeouts = 0
	st.batch = min(st.batch*2, maxBatch)
}

// timedOut halves the batch (floor minBatch) and warns on every warnAfterTimeouts-th consecutive
// timeout of the table.
func (r *Retention) timedOut(table store.HistoryRetentionTable) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.tables[table]
	st.timeouts++
	st.batch = max(st.batch/2, minBatch)
	if st.timeouts%warnAfterTimeouts == 0 {
		slog.Warn("history retention: repeated statement timeouts; the table is not being trimmed",
			"table", table, "consecutive_timeouts", st.timeouts, "batch", st.batch)
	}
}
