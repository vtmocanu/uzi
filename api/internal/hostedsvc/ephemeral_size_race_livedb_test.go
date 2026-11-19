package hostedsvc

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4213 M3: the ephemeral provisioner's FOR SHARE on the default size row against a size
// delete, in both orders, over a real database (run via e2e/run-store-it.sh). The delete side
// runs the real store.DeleteWorkerSizeGuarded under the size advisory lock, exactly as the
// admin handler does.

const ephemeralRaceSize = "zz-u9e"

const raceWait = 15 * time.Second

// stepHold parks the first transaction that reaches (stage, name) and records its backend pid
// (read on the held transaction itself).
type stepHold struct {
	locked, release chan struct{}
	once, relOnce   sync.Once
	pid             int32 // written before locked is closed
	pidErr          error // likewise
}

func holdStep(t *testing.T, stage, name string) *stepHold {
	t.Helper()
	h := &stepHold{locked: make(chan struct{}), release: make(chan struct{})}
	store.SetWorkerSizeStepHookForTest(t, func(s string, names []string, db store.DBTX) {
		if s != stage || !slices.Contains(names, name) {
			return
		}
		first := false
		h.once.Do(func() {
			first = true
			h.pidErr = db.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&h.pid)
			close(h.locked)
		})
		if first {
			<-h.release
		}
	})
	// Registered after the hook, so it runs first: release before the hook is cleared.
	t.Cleanup(h.rel)
	return h
}

func (h *stepHold) rel() { h.relOnce.Do(func() { close(h.release) }) }

func (h *stepHold) waitLocked(t *testing.T) {
	t.Helper()
	select {
	case <-h.locked:
	case <-time.After(raceWait):
		t.Fatal("the held transaction never reached its lock step")
	}
	if h.pidErr != nil {
		t.Fatalf("read the held transaction's backend pid: %v", h.pidErr)
	}
}

// waitBlockedBy polls until some backend's pg_blocking_pids contains the holder's backend pid,
// within a bounded time.
func waitBlockedBy(t *testing.T, pool *pgxpool.Pool, holderPid int32) {
	t.Helper()
	deadline := time.Now().Add(raceWait)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND $1::int = ANY(pg_blocking_pids(pid))`, holderPid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend was ever blocked by backend pid %d", holderPid)
}

func awaitRace[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(raceWait):
		t.Fatalf("%s did not finish", what)
		panic("unreachable")
	}
}

func seedRaceSize(t *testing.T, e *wakeEnv) {
	t.Helper()
	clean := func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM workers WHERE hosted_size = $1`, ephemeralRaceSize)
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM worker_sizes WHERE name = $1`, ephemeralRaceSize)
	}
	clean()
	e.exec(`INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib, data_gib, dind_data_gib, display_order)
		VALUES ($1, 500, 2000, 1024, 2048, 10, 10, 95)`, ephemeralRaceSize)
	t.Cleanup(clean)
}

// guardedDelete is the admin delete's transaction: size advisory lock, then the guarded delete.
func guardedDelete(ctx context.Context, pool *pgxpool.Pool, name string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, 0)", store.WorkerSizesLockClass); err != nil {
		return err
	}
	if err := store.DeleteWorkerSizeGuarded(ctx, tx, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func provisionAt(e *wakeEnv, run uuid.UUID) (bool, error) {
	return e.prov.provisionOne(e.ctx, e.user, run, "base", false, false,
		ephemeralKnobs{maxPerUser: 5, defaultSize: ephemeralRaceSize})
}

type provOut struct {
	ok  bool
	err error
}

func TestEphemeralProvisionRacesSizeDeleteLiveDB(t *testing.T) {
	t.Run("provision holds FOR SHARE: delete blocks, then refuses as in use", func(t *testing.T) {
		e := newWakeEnv(t, true)
		seedRaceSize(t, e)
		run := e.queuedRun()
		hold := holdStep(t, store.WorkerSizeStepShared, ephemeralRaceSize)

		pDone := make(chan provOut, 1)
		go func() { ok, err := provisionAt(e, run); pDone <- provOut{ok, err} }()
		hold.waitLocked(t)

		dDone := make(chan error, 1)
		go func() { dDone <- guardedDelete(e.ctx, e.pool, ephemeralRaceSize) }()
		waitBlockedBy(t, e.pool, hold.pid)
		select {
		case err := <-dDone:
			t.Fatalf("the delete finished while the provision held its lock: %v", err)
		case <-time.After(150 * time.Millisecond):
		}

		hold.rel()
		if p := awaitRace(t, pDone, "the provision"); p.err != nil || !p.ok {
			t.Fatalf("provision = %+v, want created", p)
		}
		var refusal *store.WorkerSizeRefusal
		err := awaitRace(t, dDone, "the delete")
		if !asRefusal(err, &refusal) || refusal.Reason != store.WorkerSizeRefusedInUse || refusal.Workers != 1 {
			t.Fatalf("delete after a committed provision = %v, want in_use with 1 worker", err)
		}
		if e.count(`SELECT count(*) FROM worker_sizes WHERE name = $1`, ephemeralRaceSize) != 1 {
			t.Fatal("the size was deleted although a worker uses it")
		}
	})

	t.Run("delete holds FOR UPDATE: provision blocks, then skips", func(t *testing.T) {
		e := newWakeEnv(t, true)
		seedRaceSize(t, e)
		run := e.queuedRun()
		hold := holdStep(t, store.WorkerSizeStepDeleteLocked, ephemeralRaceSize)

		dDone := make(chan error, 1)
		go func() { dDone <- guardedDelete(e.ctx, e.pool, ephemeralRaceSize) }()
		hold.waitLocked(t)

		pDone := make(chan provOut, 1)
		go func() { ok, err := provisionAt(e, run); pDone <- provOut{ok, err} }()
		waitBlockedBy(t, e.pool, hold.pid)
		select {
		case p := <-pDone:
			t.Fatalf("the provision finished while the delete held its lock: %+v", p)
		case <-time.After(150 * time.Millisecond):
		}

		hold.rel()
		if err := awaitRace(t, dDone, "the delete"); err != nil {
			t.Fatalf("delete = %v, want success", err)
		}
		// Skipped (false, nil): never an error, never a worker, and the run is untouched.
		if p := awaitRace(t, pDone, "the provision"); p.err != nil || p.ok {
			t.Fatalf("provision after the delete = %+v, want skipped (false, nil)", p)
		}
		if e.count(`SELECT count(*) FROM workers WHERE user_id = $1 AND ephemeral`, e.user) != 0 {
			t.Fatal("a worker was created on a deleted size")
		}
	})
}

func asRefusal(err error, target **store.WorkerSizeRefusal) bool {
	return errors.As(err, target)
}
