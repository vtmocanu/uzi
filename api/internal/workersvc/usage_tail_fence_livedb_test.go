package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// usage_tail_fence_livedb_test.go pins RecordRunUsage's locked recheck of the claim fence as the
// LAST statement before Commit (ADR-2014 D9): a release or reclaim that commits after the early
// fence read must still discard the whole post.

func (e usageTailEnv) assertNoUsageRows(t *testing.T) {
	t.Helper()
	for _, tbl := range []string{"run_usage_legs", "run_usage_messages"} {
		if n := e.count(t, tbl); n != 0 {
			t.Errorf("%s has %d rows after a fenced-out post, want 0", tbl, n)
		}
	}
}

func TestUsageTailReleaseAfterEarlyFenceWritesNothingLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	gen := int64(1)
	e.svc.usageAfterFenceHook = func() {
		e.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, e.runID)
	}
	err := e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Legs: []UsageLegMarker{closed(a, 1)}, Messages: []UsageRecord{um(a, "m1", 1)}}, &gen)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("err = %v, want ErrStaleClaim", err)
	}
	e.assertNoUsageRows(t)
}

func TestUsageTailReclaimAfterEarlyFenceWritesNothingLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	gen := int64(1)
	other := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		other, e.userID, "w-"+other.String(), other[:])
	e.svc.usageAfterFenceHook = func() {
		e.exec(`UPDATE runs SET claim_released_at = NULL, claim_generation = $2, worker_id = $3 WHERE id = $1`, e.runID, gen+1, other)
	}
	err := e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Messages: []UsageRecord{um(a, "m1", 1)}}, &gen)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("err = %v, want ErrStaleClaim", err)
	}
	e.assertNoUsageRows(t)
}

// A legacy (generation-less) post is fenced only by the claim being unreleased and still held by
// the posting worker: a reassignment to another worker must discard it.
func TestUsageTailLegacyPostWorkerReassignedWritesNothingLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	other := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		other, e.userID, "w-"+other.String(), other[:])
	e.svc.usageAfterFenceHook = func() {
		e.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, e.runID, other)
	}
	err := e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Messages: []UsageRecord{um(a, "m1", 1)}}, nil)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("err = %v, want ErrStaleClaim", err)
	}
	e.assertNoUsageRows(t)
}

// Once the recheck holds the runs row FOR SHARE, a release UPDATE started before our commit
// blocks (lock_timeout 55P03), and the post then commits.
func TestUsageTailRecheckLocksRunRowUntilCommitLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	gen := int64(1)
	var hookErr error
	e.svc.usageBeforeCommitHook = func() {
		conn, err := e.pool.Acquire(e.ctx)
		if err != nil {
			hookErr = err
			return
		}
		defer conn.Release()
		tx, err := conn.Begin(e.ctx)
		if err != nil {
			hookErr = err
			return
		}
		defer func() { _ = tx.Rollback(e.ctx) }()
		if _, err := tx.Exec(e.ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
			hookErr = err
			return
		}
		_, err = tx.Exec(e.ctx, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, e.runID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			hookErr = errors.New("release UPDATE during the recheck lock: want SQLSTATE 55P03")
			if err != nil {
				hookErr = errors.Join(hookErr, err)
			}
		}
	}
	if err := e.svc.RecordRunUsage(e.ctx, e.wkr, e.runID, UsageRequest{Messages: []UsageRecord{um(a, "m1", 1)}}, &gen); err != nil {
		t.Fatalf("post: %v", err)
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if n := e.count(t, "run_usage_messages"); n != 1 {
		t.Fatalf("run_usage_messages = %d, want 1", n)
	}
}
