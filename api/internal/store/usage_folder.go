package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Production defaults for the UsageFolder (issue #4700 M2). Every one is overridable through
// UsageFolderOptions so tests run in milliseconds.
const maxFolderLaneLimit int32 = MaxRunUsageFoldBatch

const (
	usageFolderParentTimeout  = 5 * time.Second
	usageFolderLaneTimeout    = 2500 * time.Millisecond
	usageFolderKickTimeout    = 5 * time.Second
	usageFolderSkipTTL        = 10 * time.Minute
	usageFolderMissingBackoff = 5 * time.Minute
	usageFolderMaxKicks       = 2
	// usageFolderMaxSkip bounds the in-memory skip set (and so the @skip array every lane
	// query carries). When it is full of unexpired entries the soonest-to-expire one is
	// evicted; an evicted run is merely retried earlier than its TTL.
	usageFolderMaxSkip = 1000
	// usageFolderMissingWindow is the number of eligible runs one missing-lane query inspects. It
	// MUST equal the LIMIT literal in ListMissingRunUsageFoldRuns (queries/run_usage_folds.sql):
	// a window shorter than this means the traversal reached the end.
	usageFolderMissingWindow int32 = 2000
	// usageFolderRollbackTimeout bounds the cleanup rollback of a lane query's transaction.
	usageFolderRollbackTimeout = 2 * time.Second
)

// UsageFolderDB is what the folder needs from the pool: transactions for the materializer
// and plain queries for the lane selects. *pgxpool.Pool satisfies it.
type UsageFolderDB interface {
	RunUsageFoldBeginner
	DBTX
}

// UsageFolderOptions tunes a UsageFolder. The zero value of every field means the production
// default, so a test overrides only what it needs.
type UsageFolderOptions struct {
	// Materialize replaces MaterializeRunUsageFolds (tests inject failures or blocking).
	Materialize func(ctx context.Context, ids []uuid.UUID) (int64, error)
	// OnFolded is called after every kick's materialization, with its outcome. Test hook.
	OnFolded func(runID uuid.UUID, n int64, err error)
	// Now replaces time.Now for the skip TTL and the missing-lane backoff.
	Now func() time.Time
	// OnLane is called with "stale" or "missing" as each lane of a Pass starts. Test hook.
	OnLane func(lane string)

	ParentTimeout  time.Duration // whole-Pass deadline (default 5s)
	LaneTimeout    time.Duration // per-lane cap, itself bounded by what is left of the parent (2.5s)
	KickTimeout    time.Duration // one kick's materialization (5s)
	SkipTTL        time.Duration // how long a run that failed on its own is excluded (10m)
	MissingBackoff time.Duration // pause after a full missing-lane traversal found nothing (5m)
	LaneLimit      int32         // attempts per lane per tick (MaxRunUsageFoldBatch)
	MaxKicks       int           // concurrent kick goroutines (2)
}

// UsageFolder keeps run_usage_model_folds fresh. Two producers feed MaterializeRunUsageFolds:
//
//   - Notify, wired as a run-lifecycle hook, kicks a one-run materialization the moment a run
//     reaches a terminal status. It is the fast path and strictly best-effort.
//   - Pass, registered on the sweeper, is the safety net: it walks a stale lane and a missing
//     lane, so terminal writes that never kicked (a dropped kick, direct SQL, a writer that
//     staled an already-folded run) are caught.
//
// Every materializer call goes through one semaphore: two concurrent calls on the same NEW
// run make one whole batch fail with 55P03 (see MaterializeRunUsageFolds), so the folder
// serialises its own calls.
type UsageFolder struct {
	opt UsageFolderOptions
	db  UsageFolderDB
	mat chan struct{} // capacity 1: serialises every Materialize call

	baseCtx    context.Context
	baseCancel context.CancelFunc

	kickMu sync.Mutex
	closed bool
	slots  int
	wg     sync.WaitGroup

	passMu         sync.Mutex
	tick           uint64
	staleCursor    uuid.UUID
	missingC       time.Time
	missingCID     uuid.UUID
	missingFound   bool
	missingBackoff time.Time
	// retryStale / retryMissing hold the ids of a batch that failed because the lane deadline
	// expired. The next Pass retries them run by run, before taking new work (guarded by passMu).
	retryStale   []uuid.UUID
	retryMissing []uuid.UUID

	skipMu sync.Mutex
	skip   map[uuid.UUID]time.Time
}

// NewUsageFolder builds a folder over db. db may be nil only when opts.Materialize is set
// and Pass is never called (unit tests of the kick path).
func NewUsageFolder(db UsageFolderDB, opts UsageFolderOptions) *UsageFolder {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ParentTimeout <= 0 {
		opts.ParentTimeout = usageFolderParentTimeout
	}
	if opts.LaneTimeout <= 0 {
		opts.LaneTimeout = usageFolderLaneTimeout
	}
	if opts.KickTimeout <= 0 {
		opts.KickTimeout = usageFolderKickTimeout
	}
	if opts.SkipTTL <= 0 {
		opts.SkipTTL = usageFolderSkipTTL
	}
	if opts.MissingBackoff <= 0 {
		opts.MissingBackoff = usageFolderMissingBackoff
	}
	if opts.LaneLimit <= 0 || opts.LaneLimit > maxFolderLaneLimit {
		opts.LaneLimit = maxFolderLaneLimit
	}
	if opts.MaxKicks <= 0 {
		opts.MaxKicks = usageFolderMaxKicks
	}
	f := &UsageFolder{opt: opts, db: db, mat: make(chan struct{}, 1), skip: map[uuid.UUID]time.Time{}}
	if opts.Materialize == nil {
		f.opt.Materialize = func(ctx context.Context, ids []uuid.UUID) (int64, error) {
			return MaterializeRunUsageFolds(ctx, db, ids)
		}
	}
	f.baseCtx, f.baseCancel = context.WithCancel(context.Background())
	f.resetMissingCursor()
	return f
}

func (f *UsageFolder) resetMissingCursor() {
	f.missingC = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	f.missingCID = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
}

func terminalRunStatus(s string) bool {
	return s == "completed" || s == "failed" || s == "cancelled"
}

// materialize runs one materializer call under the folder-wide semaphore; waiting for it
// honours ctx.
func (f *UsageFolder) materialize(ctx context.Context, ids []uuid.UUID) (int64, error) {
	select {
	case f.mat <- struct{}{}:
	case <-ctx.Done():
		return 0, &materializerBusyError{err: ctx.Err()}
	}
	defer func() { <-f.mat }()
	return f.opt.Materialize(ctx, ids)
}

// materializerBusyError means ctx ended while the call was still waiting for the folder-wide
// semaphore (a kick held it): the run itself was never attempted, so it must not be skipped.
type materializerBusyError struct{ err error }

func (e *materializerBusyError) Error() string {
	return "waiting for the materializer: " + e.err.Error()
}
func (e *materializerBusyError) Unwrap() error { return e.err }

// fullBudget reports whether ctx still has at least half a lane budget, i.e. an attempt that
// runs out of time did so on its own and not because the parent deadline cut the lane short.
func (f *UsageFolder) fullBudget(ctx context.Context) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) >= f.opt.LaneTimeout/2
}

// readTx runs fn in a short read-only transaction with a server-side statement_timeout of one
// lane budget. pgx v5 sends a best-effort CancelRequest when ctx ends (pgconn asyncClose), but
// it is not guaranteed to arrive, so the server-side bound is the backstop that stops a slow
// lane query from running on after the lane gave up.
func (f *UsageFolder) readTx(ctx context.Context, fn func(q *Queries) error) error {
	tx, err := f.db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), usageFolderRollbackTimeout)
		defer rcancel()
		if rerr := tx.Rollback(rctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			slog.Debug("usage fold lane query rollback failed", "error", rerr)
		}
	}()
	ms := f.opt.LaneTimeout.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
		return err
	}
	return fn(New(tx))
}

// routineLaneDeadline reports whether err is a lane running out of its budget (the lane
// context's deadline or the server-side statement_timeout), which is routine under load and
// retried on the next tick, as opposed to a real failure.
func routineLaneDeadline(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014" && !isCanceledByUser(pgErr)
}

// isCanceledByUser tells a statement_timeout apart from a cancel request (both are 57014).
func isCanceledByUser(pgErr *pgconn.PgError) bool {
	return pgErr.Message == "canceling statement due to user request"
}

// Notify satisfies workersvc.RunLifecycle. For a terminal status it starts a bounded
// background materialization of that run; anything else, a folder that is shutting down, or
// a full set of kick slots is ignored (the Pass catches what a kick misses). It never blocks
// and never reports an error: it is called on the request path.
func (f *UsageFolder) Notify(runID uuid.UUID, status string) {
	if !terminalRunStatus(status) {
		return
	}
	f.kickMu.Lock()
	if f.closed || f.slots >= f.opt.MaxKicks {
		f.kickMu.Unlock()
		return
	}
	f.slots++
	f.wg.Add(1)
	f.kickMu.Unlock()

	go func() {
		defer f.wg.Done()
		defer func() {
			f.kickMu.Lock()
			f.slots--
			f.kickMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(f.baseCtx, f.opt.KickTimeout)
		defer cancel()
		n, err := f.materialize(ctx, []uuid.UUID{runID})
		if err != nil {
			slog.Warn("usage fold kick failed", "run_id", runID, "error", err)
		}
		if h := f.opt.OnFolded; h != nil {
			h(runID, n, err)
		}
	}()
}

// Wait stops accepting kicks and waits for the in-flight ones, bounded by ctx. When ctx ends
// first the remaining kicks are cancelled and ctx's error is returned.
func (f *UsageFolder) Wait(ctx context.Context) error {
	f.kickMu.Lock()
	f.closed = true
	f.kickMu.Unlock()

	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		f.baseCancel()
		return ctx.Err()
	}
}

// Pass is the sweeper pass: it returns how many runs it folded. See the type comment. One
// lane failing never stops the other; the errors are joined.
func (f *UsageFolder) Pass(ctx context.Context) (int64, error) {
	f.passMu.Lock()
	defer f.passMu.Unlock()

	pctx, cancel := context.WithTimeout(ctx, f.opt.ParentTimeout)
	defer cancel()

	f.tick++
	lanes := []struct {
		name string
		run  func(context.Context) (int64, error)
	}{{"stale", f.staleLane}, {"missing", f.missingLane}}
	if f.tick%2 == 0 {
		lanes[0], lanes[1] = lanes[1], lanes[0]
	}

	var total int64
	var errs []error
	// laneErr records a lane's failure. Running out of budget is routine under load: it is
	// logged at Warn and not reported, so the sweeper does not log an error every tick.
	laneErr := func(name string, err error) {
		if routineLaneDeadline(err) {
			slog.Warn("usage fold lane ran out of budget", "lane", name, "error", err)
			return
		}
		errs = append(errs, fmt.Errorf("usage fold %s lane: %w", name, err))
	}
	for _, l := range lanes {
		if err := pctx.Err(); err != nil {
			laneErr(l.name, err)
			continue
		}
		if h := f.opt.OnLane; h != nil {
			h(l.name)
		}
		// WithTimeout under pctx yields min(what is left of the parent, LaneTimeout).
		lctx, lcancel := context.WithTimeout(pctx, f.opt.LaneTimeout)
		n, err := l.run(lctx)
		lcancel()
		total += n
		if err != nil {
			laneErr(l.name, err)
		}
	}
	return total, errors.Join(errs...)
}

func (f *UsageFolder) staleLane(ctx context.Context) (int64, error) {
	limit := f.opt.LaneLimit
	n0, err := f.retryPending(ctx, &f.retryStale)
	if err != nil {
		return n0, err
	}
	var ids []uuid.UUID
	err = f.readTx(ctx, func(q *Queries) error {
		var qerr error
		ids, qerr = q.ListStaleRunUsageFoldRuns(ctx, ListStaleRunUsageFoldRunsParams{
			After: f.staleCursor, Skip: f.skipList(), Lim: limit,
		})
		return qerr
	})
	if err != nil {
		// The cursor stays where it was; the next tick (one sweeper interval away) retries.
		return n0, err
	}
	// Advance even when the fold below fails or times out: the unfolded runs stay stale and
	// come back on the wrap, so one stuck batch cannot starve the lane.
	if len(ids) < int(limit) {
		f.staleCursor = uuid.Nil
	} else {
		f.staleCursor = ids[len(ids)-1]
	}
	n1, err := f.fold(ctx, ids, &f.retryStale)
	return n0 + n1, err
}

// missingLane walks the eligible runs that have no state row, newest first, one window of
// usageFolderMissingWindow runs per call (see ListMissingRunUsageFoldRuns). A traversal ends
// when a window is shorter than the full window; it backs off MissingBackoff only when the
// whole traversal found no candidate. A failed query (including the lane deadline or the
// server-side statement_timeout) also backs off, so a scan slower than the lane budget is
// retried every MissingBackoff, not every tick, but only when the query had a full lane budget:
// a deadline caused by retryPending having consumed the budget says nothing about the query.
func (f *UsageFolder) missingLane(ctx context.Context) (int64, error) {
	now := f.opt.Now()
	if now.Before(f.missingBackoff) {
		return 0, nil
	}
	limit := f.opt.LaneLimit
	n0, err := f.retryPending(ctx, &f.retryMissing)
	if err != nil {
		return n0, err
	}
	var rows []ListMissingRunUsageFoldRunsRow
	fullQuery := f.fullBudget(ctx)
	err = f.readTx(ctx, func(q *Queries) error {
		var qerr error
		rows, qerr = q.ListMissingRunUsageFoldRuns(ctx, ListMissingRunUsageFoldRunsParams{
			C:    pgtype.Timestamptz{Time: f.missingC, Valid: true},
			Cid:  f.missingCID,
			Skip: f.skipList(),
			Lim:  limit,
		})
		return qerr
	})
	if err != nil {
		if fullQuery {
			f.missingBackoff = now.Add(f.opt.MissingBackoff)
		}
		return n0, err
	}
	var ids []uuid.UUID
	var oldest, end *ListMissingRunUsageFoldRunsRow
	for i := range rows {
		r := &rows[i]
		if r.WindowEnd {
			end = r
			continue
		}
		ids = append(ids, r.ID)
		if oldest == nil || r.CreatedAt.Time.Before(oldest.CreatedAt.Time) ||
			(r.CreatedAt.Time.Equal(oldest.CreatedAt.Time) && r.ID.String() < oldest.ID.String()) {
			oldest = r
		}
	}
	if len(ids) > 0 {
		f.missingFound = true
	}
	switch {
	case len(ids) >= int(limit):
		// More candidates may remain in this window: continue after the last one taken.
		f.missingC, f.missingCID = oldest.CreatedAt.Time, oldest.ID
	case end != nil && end.WindowRows >= usageFolderMissingWindow:
		// The whole window was scanned; the next one starts after its oldest run.
		f.missingC, f.missingCID = end.CreatedAt.Time, end.ID
	default:
		// A short (or empty) window: the traversal reached the end. One that found nothing at
		// all backs off; either way the next one starts from the newest run again.
		if !f.missingFound {
			f.missingBackoff = now.Add(f.opt.MissingBackoff)
		}
		f.missingFound = false
		f.resetMissingCursor()
	}
	n1, err := f.fold(ctx, ids, &f.retryMissing)
	return n0 + n1, err
}

// retryPending retries, run by run, the ids of a batch that failed on the lane deadline in an
// earlier tick. Progress persists in *pending: an id leaves it once it folded, or once it was
// skipped for the TTL because it ran out of time ALONE with a full lane budget. An id that ran
// out of time with a short budget (or while waiting for the materializer) stays for the next
// tick, where it goes first.
func (f *UsageFolder) retryPending(ctx context.Context, pending *[]uuid.UUID) (int64, error) {
	var total int64
	for len(*pending) > 0 {
		id := (*pending)[0]
		full := f.fullBudget(ctx)
		n, err := f.materialize(ctx, []uuid.UUID{id})
		if err == nil {
			total += n
			*pending = (*pending)[1:]
			continue
		}
		if ctx.Err() != nil {
			var busy *materializerBusyError
			if full && !errors.As(err, &busy) {
				f.skipRun(id, err)
				*pending = (*pending)[1:]
			}
			return total, fmt.Errorf("retry of a deadline-failed batch: %w", err)
		}
		f.skipRun(id, err)
		*pending = (*pending)[1:]
	}
	return total, nil
}

// fold materializes ids as one batch. When the batch fails on its own it retries run by run, and
// a run that fails on its own is skipped for the TTL so one poison run cannot stall the lane.
// When the batch fails because the lane deadline expired, the ids are remembered in *retry and
// retried run by run first on the next tick, so a run whose fold outlasts the lane budget cannot
// block its neighbours forever; a run that times out alone is skipped. The error is returned only
// for a failure the lane itself suffers (deadline), never for a run that was individually skipped.
func (f *UsageFolder) fold(ctx context.Context, ids []uuid.UUID, retry *[]uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	full := f.fullBudget(ctx)
	n, err := f.materialize(ctx, ids)
	if err == nil {
		return n, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		var busy *materializerBusyError
		if len(ids) == 1 {
			if full && !errors.As(err, &busy) {
				f.skipRun(ids[0], err)
			}
		} else {
			*retry = append([]uuid.UUID(nil), ids...)
		}
		return 0, fmt.Errorf("batch of %d: %w", len(ids), err)
	}
	if len(ids) == 1 {
		f.skipRun(ids[0], err)
		return 0, nil
	}
	slog.Warn("usage fold batch failed, retrying run by run", "runs", len(ids), "error", err)
	var total int64
	for i, id := range ids {
		n1, err1 := f.materialize(ctx, []uuid.UUID{id})
		if err1 == nil {
			total += n1
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			// Out of time part way: the rest, this run included, go first on the next tick.
			*retry = append([]uuid.UUID(nil), ids[i:]...)
			return total, fmt.Errorf("run-by-run retry: %w", err1)
		}
		f.skipRun(id, err1)
	}
	return total, nil
}

func (f *UsageFolder) skipRun(id uuid.UUID, cause error) {
	slog.Warn("usage fold run skipped", "run_id", id, "ttl", f.opt.SkipTTL.String(), "error", cause)
	now := f.opt.Now()
	f.skipMu.Lock()
	defer f.skipMu.Unlock()
	f.pruneSkipLocked(now)
	if len(f.skip) >= usageFolderMaxSkip {
		var oldest uuid.UUID
		var oldestAt time.Time
		for k, v := range f.skip {
			if oldestAt.IsZero() || v.Before(oldestAt) {
				oldest, oldestAt = k, v
			}
		}
		delete(f.skip, oldest)
	}
	f.skip[id] = now.Add(f.opt.SkipTTL)
}

func (f *UsageFolder) pruneSkipLocked(now time.Time) {
	for k, until := range f.skip {
		if !now.Before(until) {
			delete(f.skip, k)
		}
	}
}

// skipList returns the unexpired skip set (never nil, so the array parameter is empty rather
// than NULL, and NOT (x = ANY(NULL)) would match nothing).
func (f *UsageFolder) skipList() []uuid.UUID {
	f.skipMu.Lock()
	defer f.skipMu.Unlock()
	f.pruneSkipLocked(f.opt.Now())
	out := make([]uuid.UUID, 0, len(f.skip))
	for k := range f.skip {
		out = append(out, k)
	}
	return out
}
