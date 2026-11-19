package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Helper-level LiveDB tests for the terminal-ref consumer gate (ADR-4007 T3, issue #4488).

const tcgClassID = 1970959459 // 0x757A7463, "uztc"

func tcgBegin(ctx context.Context, t *testing.T, pool *pgxpool.Pool, opts pgx.TxOptions) pgx.Tx {
	t.Helper()
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// tcgHold takes the gate key exclusively (blocking) in a fresh READ COMMITTED tx.
func tcgHold(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo uuid.UUID) pgx.Tx {
	t.Helper()
	tx := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", TerminalConsumerGateLockClass, TerminalConsumerGateObjID(repo)); err != nil {
		t.Fatalf("hold exclusive: %v", err)
	}
	return tx
}

// tcgAwaitShareWaiter polls pg_locks until a not-granted ShareLock waiter exists on the
// gate key of repo, failing the test if none appears in 5s.
func tcgAwaitShareWaiter(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo uuid.UUID) {
	t.Helper()
	objID := uint32(TerminalConsumerGateObjID(repo)) //nolint:gosec // reinterpret the int4 lock key as pg_locks' oid
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND classid = $1::oid AND objid = $2::oid AND objsubid = 2
			  AND mode = 'ShareLock' AND NOT granted`, int64(tcgClassID), int64(objID)).Scan(&n)
		if err != nil {
			t.Fatalf("pg_locks: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ShareLock waiter on gate key classid=%d objid=%d", tcgClassID, objID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tcgLockAsync runs LockTerminalConsumerGateShared on tx in a goroutine.
func tcgLockAsync(ctx context.Context, tx pgx.Tx, repo uuid.UUID, wait time.Duration) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- New(tx).LockTerminalConsumerGateShared(ctx, repo, wait) }()
	return ch
}

func tcgRecv(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for gate helper")
		return nil
	}
}

func tcgTry(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo uuid.UUID) (bool, pgx.Tx) {
	t.Helper()
	tx := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	ok, err := New(tx).TryLockTerminalConsumerGate(ctx, repo)
	if err != nil {
		t.Fatalf("try: %v", err)
	}
	return ok, tx
}

func TestTerminalConsumerGateSharedHoldersDoNotConflict(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	repo := uuid.New()
	a, b := tcgBegin(ctx, t, pool, pgx.TxOptions{}), tcgBegin(ctx, t, pool, pgx.TxOptions{})
	if err := New(a).LockTerminalConsumerGateShared(ctx, repo, time.Second); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := New(b).LockTerminalConsumerGateShared(ctx, repo, time.Second); err != nil {
		t.Fatalf("b: %v", err)
	}
}

func TestTerminalConsumerGateExclusiveTryVsSharedHolder(t *testing.T) {
	for _, release := range []string{"commit", "rollback"} {
		t.Run(release, func(t *testing.T) {
			ctx, pool, _ := openRepoActivityLiveDB(t)
			repo := uuid.New()
			shared := tcgBegin(ctx, t, pool, pgx.TxOptions{})
			if err := New(shared).LockTerminalConsumerGateShared(ctx, repo, time.Second); err != nil {
				t.Fatal(err)
			}
			if ok, tx := tcgTry(ctx, t, pool, repo); ok {
				t.Fatal("exclusive try succeeded while a shared holder exists")
			} else {
				_ = tx.Rollback(ctx)
			}
			var err error
			if release == "commit" {
				err = shared.Commit(ctx)
			} else {
				err = shared.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok, _ := tcgTry(ctx, t, pool, repo); !ok {
				t.Fatalf("exclusive try failed after shared holder %s", release)
			}
		})
	}
}

func TestTerminalConsumerGateWaiterVisibleInPgLocksAndBlocksExclusiveTry(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	repo := uuid.New()
	holder := tcgHold(ctx, t, pool, repo)
	waiter := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	ch := tcgLockAsync(ctx, waiter, repo, 10*time.Second)
	tcgAwaitShareWaiter(ctx, t, pool, repo)

	// Waiter priority: release the exclusive holder and, WITHOUT first receiving the waiter's
	// result, try-take the gate exclusively. PostgreSQL grants the queued shared waiter inside
	// the holder's lock release (before Commit returns), so the try must fail. Without a queued
	// waiter this try would succeed, so the assertion is not vacuous.
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, tx := tcgTry(ctx, t, pool, repo); ok {
		t.Fatal("a try-exclusive after the holder released beat the queued shared waiter")
	} else {
		_ = tx.Rollback(ctx)
	}
	if err := tcgRecv(t, ch); err != nil {
		t.Fatalf("waiter after release: %v", err)
	}
	// The waiter now holds the gate shared, so an exclusive try still fails until it ends.
	if ok, tx := tcgTry(ctx, t, pool, repo); ok {
		t.Fatal("exclusive try succeeded while the granted waiter holds shared")
	} else {
		_ = tx.Rollback(ctx)
	}
	if err := waiter.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := tcgTry(ctx, t, pool, repo); !ok {
		t.Fatal("exclusive try failed after waiter committed")
	}
}

func TestTerminalConsumerGateTimeoutAbortsTxWithoutRestore(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	repo := uuid.New()
	holder := tcgHold(ctx, t, pool, repo)
	waiter := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	start := time.Now()
	err := New(waiter).LockTerminalConsumerGateShared(ctx, repo, 200*time.Millisecond)
	if !errors.Is(err, ErrTerminalConsumerGateBusy) {
		t.Fatalf("err = %v, want ErrTerminalConsumerGateBusy", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %v", d)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("err = %v, want underlying 55P03 (a 25P02 would mean a restore ran on the aborted tx)", err)
	}
	var probe int
	err = waiter.QueryRow(ctx, "SELECT 1").Scan(&probe)
	if !errors.As(err, &pgErr) || pgErr.Code != "25P02" {
		t.Fatalf("follow-up statement err = %v, want 25P02 (tx aborted)", err)
	}
	// Recovery after contention clears: once the holder is gone, a fresh transaction admits.
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	fresh := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	if err := New(fresh).LockTerminalConsumerGateShared(ctx, repo, time.Second); err != nil {
		t.Fatalf("fresh tx after the holder released: %v", err)
	}
}

func TestTerminalConsumerGateAlreadyCancelledContextIsBusy(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	tx := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := New(tx).LockTerminalConsumerGateShared(cctx, uuid.New(), time.Second); !errors.Is(err, ErrTerminalConsumerGateBusy) {
		t.Fatalf("err = %v, want ErrTerminalConsumerGateBusy", err)
	}
}

// lateCancelCtx reports a nil Err() on its first call (the helper's entry check) and
// context.Canceled afterwards, with Done() already closed, so the helper's preamble
// statements (read session settings, set lock_timeout) are the ones that see the ended context.
type lateCancelCtx struct {
	context.Context
	calls atomic.Int32
	done  chan struct{}
}

func (c *lateCancelCtx) Done() <-chan struct{} { return c.done }
func (c *lateCancelCtx) Err() error {
	if c.calls.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

func TestTerminalConsumerGateContextEndedDuringPreambleIsBusy(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	tx := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	lc := &lateCancelCtx{Context: ctx, done: make(chan struct{})}
	close(lc.done)
	err := New(tx).LockTerminalConsumerGateShared(lc, uuid.New(), time.Second)
	if !errors.Is(err, ErrTerminalConsumerGateBusy) {
		t.Fatalf("err = %v, want ErrTerminalConsumerGateBusy", err)
	}
}

func TestTerminalConsumerGateContextCancelDuringWait(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	repo := uuid.New()
	_ = tcgHold(ctx, t, pool, repo)
	waiter := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := tcgLockAsync(cctx, waiter, repo, 30*time.Second)
	tcgAwaitShareWaiter(ctx, t, pool, repo)
	cancel()
	if err := tcgRecv(t, ch); !errors.Is(err, ErrTerminalConsumerGateBusy) {
		t.Fatalf("err = %v, want ErrTerminalConsumerGateBusy", err)
	}
}

func TestTerminalConsumerGateRestoresLockTimeout(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	repo := uuid.New()
	tx := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	if _, err := tx.Exec(ctx, "SELECT set_config('lock_timeout', '7777ms', true)"); err != nil {
		t.Fatal(err)
	}
	if err := New(tx).LockTerminalConsumerGateShared(ctx, repo, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := tx.QueryRow(ctx, "SELECT current_setting('lock_timeout')").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "7777ms" && got != "7.777s" {
		t.Fatalf("lock_timeout after gate = %q, want the pre-gate 7777ms", got)
	}
}

func TestTerminalConsumerGateReleasedByCommitAndRollback(t *testing.T) {
	for _, release := range []string{"commit", "rollback"} {
		t.Run(release, func(t *testing.T) {
			ctx, pool, _ := openRepoActivityLiveDB(t)
			repo := uuid.New()
			tx := tcgBegin(ctx, t, pool, pgx.TxOptions{})
			if err := New(tx).LockTerminalConsumerGateShared(ctx, repo, time.Second); err != nil {
				t.Fatal(err)
			}
			if ok, tx2 := tcgTry(ctx, t, pool, repo); ok {
				t.Fatal("gate not held")
			} else {
				_ = tx2.Rollback(ctx)
			}
			var err error
			if release == "commit" {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok, _ := tcgTry(ctx, t, pool, repo); !ok {
				t.Fatalf("gate still held after %s", release)
			}
		})
	}
}

func TestTerminalConsumerGatePostWaitFreshness(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	repo := uuid.New()
	holder := tcgHold(ctx, t, pool, repo)
	waiter := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	ch := tcgLockAsync(ctx, waiter, repo, 5*time.Second)
	tcgAwaitShareWaiter(ctx, t, pool, repo)

	user := uuid.New()
	t.Cleanup(func() { raExec(context.Background(), t, pool, `DELETE FROM users WHERE id = $1`, user) })
	if _, err := holder.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, "tcg-"+user.String()+"@e2e"); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tcgRecv(t, ch); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := waiter.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("row committed by the exclusive holder not visible after the gate: count=%d", n)
	}
}

func TestTerminalConsumerGateRefusesPoolBound(t *testing.T) {
	ctx, _, q := openRepoActivityLiveDB(t)
	repo := uuid.New()
	if q.InTx() {
		t.Fatal("pool-bound Queries reports InTx")
	}
	if err := q.LockTerminalConsumerGateShared(ctx, repo, time.Second); !errors.Is(err, ErrTerminalConsumerGateNoTx) {
		t.Fatalf("shared err = %v, want NoTx", err)
	}
	if ok, err := q.TryLockTerminalConsumerGate(ctx, repo); ok || !errors.Is(err, ErrTerminalConsumerGateNoTx) {
		t.Fatalf("try = %v, %v; want false, NoTx", ok, err)
	}
}

func TestTerminalConsumerGateRefusesRepeatableRead(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	tx := tcgBegin(ctx, t, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err := New(tx).LockTerminalConsumerGateShared(ctx, uuid.New(), time.Second); !errors.Is(err, ErrTerminalConsumerGateIsolation) {
		t.Fatalf("err = %v, want ErrTerminalConsumerGateIsolation", err)
	}
}

func TestRunRepoForConsumerGate(t *testing.T) {
	ctx, pool, q := openRepoActivityLiveDB(t)
	user, conn := raSeedUserConn(ctx, t, pool, true)
	repo := raSeedRepo(ctx, t, pool, conn, "tcg4488-"+uuid.NewString(), true)
	run := raSeedRun(ctx, t, pool, user, repo, 1, "completed", time.Now().UTC())

	got, err := q.RunRepoForConsumerGate(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Valid || uuid.UUID(got.Bytes) != repo {
		t.Fatalf("repo = %v, want %v", got, repo)
	}
	if _, err := q.RunRepoForConsumerGate(ctx, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing run err = %v, want pgx.ErrNoRows", err)
	}
}

func TestQueriesBeginTx(t *testing.T) {
	ctx, pool, q := openRepoActivityLiveDB(t)
	tx := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	if _, err := New(tx).BeginTx(ctx); !errors.Is(err, ErrTerminalConsumerGateNestedTx) {
		t.Fatalf("tx-bound BeginTx err = %v, want NestedTx", err)
	}
	ntx, err := q.BeginTx(ctx)
	if err != nil {
		t.Fatalf("pool-bound BeginTx: %v", err)
	}
	if err := ntx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

// A wait beyond lock_timeout's int4-milliseconds range is clamped instead of failing the
// set_config (SQLSTATE 22023) and aborting the admission.
func TestTerminalConsumerGateClampsHugeWait(t *testing.T) {
	ctx, pool, _ := openRepoActivityLiveDB(t)
	tx := tcgBegin(ctx, t, pool, pgx.TxOptions{})
	if err := New(tx).LockTerminalConsumerGateShared(ctx, uuid.New(), 30*24*time.Hour); err != nil {
		t.Fatalf("huge wait: %v", err)
	}
	var got string
	if err := tx.QueryRow(ctx, "SELECT current_setting('lock_timeout')").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "0" {
		t.Fatalf("lock_timeout after gate = %q, want the restored default 0", got)
	}
}
