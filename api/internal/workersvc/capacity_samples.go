package workersvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// capacity_samples.go is the sweeper pass that records worker capacity samples (issue #4693).
// One pass takes the cross-replica session advisory lock on a pinned connection, runs up to a
// few bounded batches of the set-based insert, and releases the lock, all inside a fixed total
// deadline so a slow database cannot stall the sweeper tick.

const (
	capacitySampleBatchSize      = 1000
	capacitySampleBatchBudget    = 4
	capacitySampleTotalDeadline  = 2 * time.Second
	capacitySampleCleanupReserve = 250 * time.Millisecond
	// A second, server-side bound per statement. The client work deadline (total minus reserve,
	// from pass start) normally fires first; pgx then breaks the connection, the unlock cannot be
	// proven, and the lock ends with the backend (an accepted gap, see the PRD).
	capacitySampleStatementTimeout = 1750 * time.Millisecond
	capacitySampleLockTimeout      = 300 * time.Millisecond
)

// capacitySession is the slice of store.WorkerCapacitySampleSession the pass uses; tests fake it.
type capacitySession interface {
	SampleBatch(ctx context.Context, arg store.SampleWorkerCapacityBatchParams) (store.SampleWorkerCapacityBatchRow, error)
	Close(ctx context.Context) (unlocked bool, err error)
}

// capacityOpener opens the pinned session; (nil, false, nil) means another replica holds the lock.
type capacityOpener func(ctx context.Context) (capacitySession, bool, error)

// CapacitySampler runs the worker_capacity_samples sweeper pass.
type CapacitySampler struct {
	open      capacityOpener
	now       func() time.Time
	stale     time.Duration
	batchSize int32
	budget    int
	total     time.Duration
	reserve   time.Duration
}

// NewCapacitySampler builds the production sampler over pool. A non-positive stale window is
// refused (fail closed): it would classify every worker as stale or none at all.
func NewCapacitySampler(pool *pgxpool.Pool, stale time.Duration) (*CapacitySampler, error) {
	open := func(ctx context.Context) (capacitySession, bool, error) {
		s, ok, err := store.OpenWorkerCapacitySampleSession(ctx, pool, capacitySampleStatementTimeout, capacitySampleLockTimeout)
		if err != nil || !ok {
			return nil, false, err
		}
		return s, true, nil
	}
	return newCapacitySampler(open, time.Now, stale)
}

func newCapacitySampler(open capacityOpener, now func() time.Time, stale time.Duration) (*CapacitySampler, error) {
	if stale <= 0 {
		return nil, fmt.Errorf("worker capacity samples: heartbeat stale window must be positive, got %s", stale)
	}
	return &CapacitySampler{
		open: open, now: now, stale: stale,
		batchSize: capacitySampleBatchSize, budget: capacitySampleBatchBudget,
		total: capacitySampleTotalDeadline, reserve: capacitySampleCleanupReserve,
	}, nil
}

// Pass is the sweeper pass: it returns the number of samples inserted. It ends at the first
// failed batch (no further batch this tick) and always closes the session it opened.
func (c *CapacitySampler) Pass(ctx context.Context) (int64, error) {
	deadline := c.now().Add(c.total)
	workDeadline := deadline.Add(-c.reserve)

	openCtx, cancelOpen := context.WithDeadline(ctx, workDeadline)
	sess, ok, err := c.open(openCtx)
	cancelOpen()
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}

	var inserted int64
	var errs []error
	workCtx, cancelWork := context.WithDeadline(ctx, workDeadline)
	for i := 0; i < c.budget && c.now().Before(workDeadline); i++ {
		row, berr := sess.SampleBatch(workCtx, store.SampleWorkerCapacityBatchParams{
			// ObservedAt stays NULL: the statement stamps the database clock.
			StaleAfter: pgtype.Interval{Microseconds: c.stale.Microseconds(), Valid: true},
			BatchSize:  c.batchSize,
		})
		if berr != nil {
			errs = append(errs, fmt.Errorf("worker capacity samples: batch: %w", berr))
			break
		}
		inserted += row.Inserted
		if row.Scanned < int64(c.batchSize) {
			break
		}
	}
	cancelWork()

	closeCtx, cancelClose := context.WithDeadline(ctx, deadline)
	_, cerr := sess.Close(closeCtx)
	cancelClose()
	if cerr != nil {
		errs = append(errs, cerr)
	}
	return inserted, errors.Join(errs...)
}
