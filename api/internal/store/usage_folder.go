package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
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
	q   *Queries
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
	f := &UsageFolder{opt: opts, mat: make(chan struct{}, 1), skip: map[uuid.UUID]time.Time{}}
	if db != nil {
		f.q = New(db)
	}
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
		return 0, ctx.Err()
	}
	defer func() { <-f.mat }()
	return f.opt.Materialize(ctx, ids)
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
	for _, l := range lanes {
		if err := pctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("usage fold %s lane: %w", l.name, err))
			continue
		}
		// WithTimeout under pctx yields min(what is left of the parent, LaneTimeout).
		lctx, lcancel := context.WithTimeout(pctx, f.opt.LaneTimeout)
		n, err := l.run(lctx)
		lcancel()
		total += n
		if err != nil {
			errs = append(errs, fmt.Errorf("usage fold %s lane: %w", l.name, err))
		}
	}
	return total, errors.Join(errs...)
}

func (f *UsageFolder) staleLane(ctx context.Context) (int64, error) {
	limit := f.opt.LaneLimit
	ids, err := f.q.ListStaleRunUsageFoldRuns(ctx, ListStaleRunUsageFoldRunsParams{
		After: f.staleCursor, Skip: f.skipList(), Lim: limit,
	})
	if err != nil {
		return 0, err
	}
	// Advance even when the fold below fails or times out: the unfolded runs stay stale and
	// come back on the wrap, so one stuck batch cannot starve the lane.
	if len(ids) < int(limit) {
		f.staleCursor = uuid.Nil
	} else {
		f.staleCursor = ids[len(ids)-1]
	}
	return f.fold(ctx, ids)
}

func (f *UsageFolder) missingLane(ctx context.Context) (int64, error) {
	now := f.opt.Now()
	if now.Before(f.missingBackoff) {
		return 0, nil
	}
	limit := f.opt.LaneLimit
	rows, err := f.q.ListMissingRunUsageFoldRuns(ctx, ListMissingRunUsageFoldRunsParams{
		C:    pgtype.Timestamptz{Time: f.missingC, Valid: true},
		Cid:  f.missingCID,
		Skip: f.skipList(),
		Lim:  limit,
	})
	if err != nil {
		return 0, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	if len(rows) > 0 {
		f.missingFound = true
		last := rows[len(rows)-1]
		f.missingC, f.missingCID = last.CreatedAt.Time, last.ID
	}
	if len(rows) < int(limit) {
		// The traversal reached the end. One that found nothing at all backs off; either way
		// the next one starts from the newest run again.
		if !f.missingFound {
			f.missingBackoff = now.Add(f.opt.MissingBackoff)
		}
		f.missingFound = false
		f.resetMissingCursor()
	}
	return f.fold(ctx, ids)
}

// fold materializes ids as one batch; when the batch fails (and the lane deadline is not the
// cause) it retries run by run, and a run that fails on its own is skipped for the TTL so one
// poison run cannot stall the lane. The error is returned only for a failure the lane itself
// suffers (deadline), never for a run that was individually skipped.
func (f *UsageFolder) fold(ctx context.Context, ids []uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := f.materialize(ctx, ids)
	if err == nil {
		return n, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return 0, fmt.Errorf("batch of %d: %w", len(ids), err)
	}
	if len(ids) == 1 {
		f.skipRun(ids[0], err)
		return 0, nil
	}
	slog.Warn("usage fold batch failed, retrying run by run", "runs", len(ids), "error", err)
	var total int64
	for _, id := range ids {
		n1, err1 := f.materialize(ctx, []uuid.UUID{id})
		if err1 == nil {
			total += n1
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
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
