package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func manualChange(t *testing.T, f *rollAffinityFixture, worker uuid.UUID, on bool) store.Worker {
	t.Helper()
	w, err := store.ChangeWorkerManualCordon(f.ctx, f.pool, f.userID, worker, "Operator", pgtype.Text{String: "rotation", Valid: true}, on)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func manualParams(f *rollAffinityFixture, worker uuid.UUID) store.ClaimRunParams {
	return store.ClaimRunParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, UserID: f.userID,
		AffinityCutoff:  pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true},
		SpreadCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true}}
}

func manualRead(t *testing.T, f *rollAffinityFixture, worker uuid.UUID) store.Worker {
	t.Helper()
	w, err := f.q.GetWorkerByID(f.ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestManualCordonPersistenceAndIntakeLiveDB(t *testing.T) {
	f := newRollAffinityFixture(t)
	w := f.worker("manual-persistent", 0, false)
	first := manualChange(t, f, w, true)
	if first.ManualCordonedBy.Bytes != [16]byte(f.userID) || first.ManualCordonReason.String != "rotation" {
		t.Fatal("missing attribution")
	}
	f.unclaimedRun()
	if _, err := f.claim(w, false); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale caller snapshot admitted new work: %v", err)
	}
	own := f.pinnedRun(w, 0)
	got, err := f.claim(w, false)
	if err != nil || got.ID != own {
		t.Fatalf("assigned continuation: %s %v", got.ID, err)
	}
	// Registration clears an automatic image drain, not durable owner intent.
	mustExec(f.ctx, t, f.pool, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',draining_since=now(),draining_reason='image_roll' WHERE id=$1", w)
	if _, err = f.q.RegisterWorker(f.ctx, store.RegisterWorkerParams{ID: w}); err != nil {
		t.Fatal(err)
	}
	after := manualRead(t, f, w)
	if !after.ManualCordonedAt.Time.Equal(first.ManualCordonedAt.Time) || !after.ManualCordonedBy.Valid ||
		after.ManualCordonReason.String != "rotation" || after.DrainingSince.Valid || after.DrainingReason.Valid {
		t.Fatalf("registration changed manual/automatic ownership: %+v", after)
	}
	if _, err = f.claim(w, false); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("registration reopened intake: %v", err)
	}
	// A fresh query object models service recreation against the same durable state.
	fresh := store.New(f.pool)
	persisted, err := fresh.GetWorkerByID(f.ctx, w)
	if err != nil || !persisted.ManualCordonedAt.Valid {
		t.Fatalf("recreated reader lost cordon: %v", err)
	}
	manualChange(t, f, w, false)
	if _, err = f.claim(w, false); err != nil {
		t.Fatalf("uncordon did not reopen intake: %v", err)
	}
}

func TestManualCordonAutomaticFenceCompositionLiveDB(t *testing.T) {
	for _, reason := range []string{"image_roll", "pressure_recycle", "maintenance"} {
		for _, establishManualFirst := range []bool{false, true} {
			for _, manualFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/manual-establish-first=%v/manual-release-first=%v", reason, establishManualFirst, manualFirst), func(t *testing.T) {
					f := newRollAffinityFixture(t)
					w := f.worker("combined", 0, false)
					mustExec(f.ctx, t, f.pool, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base' WHERE id=$1", w)
					if establishManualFirst {
						manualChange(t, f, w, true)
					}
					var err error
					switch reason {
					case "image_roll":
						_, err = f.q.CordonHostedWorkerImageRoll(f.ctx, w)
					case "pressure_recycle":
						_, err = f.q.CordonHostedWorkerPressureRecycle(f.ctx, w)
					default:
						tx, e := f.pool.Begin(f.ctx)
						if e != nil {
							t.Fatal(e)
						}
						q := store.New(tx)
						_, err = q.GetWorkerForUpdate(f.ctx, w)
						if err == nil {
							_, err = q.SetDindMaintenance(f.ctx, store.SetDindMaintenanceParams{ID: w, MaintenancePhase: "requested"})
						}
						if err == nil {
							err = tx.Commit(f.ctx)
						} else {
							_ = tx.Rollback(f.ctx)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					if !establishManualFirst {
						manualChange(t, f, w, true)
					}
					if store.WorkerAcceptsNewIntake(manualRead(t, f, w)) {
						t.Fatal("combined fence accepts intake")
					}
					releaseAuto := func() {
						t.Helper()
						switch reason {
						case "image_roll":
							_, err = f.q.UncordonHostedWorkerImageRoll(f.ctx, w)
						case "pressure_recycle":
							_, err = f.q.UncordonHostedWorkerPressureRecycle(f.ctx, w)
						default:
							tx, e := f.pool.Begin(f.ctx)
							if e != nil {
								t.Fatal(e)
							}
							q := store.New(tx)
							_, err = q.GetWorkerForUpdate(f.ctx, w)
							if err == nil {
								_, err = q.SetDindMaintenance(f.ctx, store.SetDindMaintenanceParams{ID: w, MaintenancePhase: "cancelled"})
							}
							if err == nil {
								err = tx.Commit(f.ctx)
							} else {
								_ = tx.Rollback(f.ctx)
							}
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					if manualFirst {
						cleared := manualChange(t, f, w, false)
						if cleared.ManualCordonedAt.Valid || store.WorkerAcceptsNewIntake(cleared) {
							t.Fatal("uncordon released automatic fence")
						}
						releaseAuto()
					} else {
						releaseAuto()
						kept := manualRead(t, f, w)
						if !kept.ManualCordonedAt.Valid || store.WorkerAcceptsNewIntake(kept) {
							t.Fatal("automatic release erased manual intent")
						}
						manualChange(t, f, w, false)
					}
					if !store.WorkerAcceptsNewIntake(manualRead(t, f, w)) {
						t.Fatal("both releases did not reopen intake")
					}
				})
			}
		}
	}

}

var errCordonFault = errors.New("injected cordon transaction failure")

type manualFaultPool struct {
	pool                                    *pgxpool.Pool
	eventFailure, commitBefore, commitAfter bool
	ready                                   chan struct{}
	release                                 <-chan struct{}
	pid                                     chan uint32
}
type manualFaultTx struct {
	pgx.Tx
	owner *manualFaultPool
}

func (p *manualFaultPool) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if p.pid != nil {
		p.pid <- tx.Conn().PgConn().PID()
	}
	return &manualFaultTx{Tx: tx, owner: p}, nil
}
func (tx *manualFaultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.owner.eventFailure && strings.Contains(sql, "INSERT INTO worker_cordon_events") {
		return pgconn.CommandTag{}, errCordonFault
	}
	return tx.Tx.Exec(ctx, sql, args...)
}
func (tx *manualFaultTx) Commit(ctx context.Context) error {
	if tx.owner.ready != nil {
		close(tx.owner.ready)
		select {
		case <-tx.owner.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if tx.owner.commitBefore {
		return errCordonFault
	}
	err := tx.Tx.Commit(ctx)
	if err == nil && tx.owner.commitAfter {
		return errCordonFault
	}
	return err
}

func manualEventCount(t *testing.T, f *rollAffinityFixture, w uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM worker_cordon_events WHERE worker_id=$1", w).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestManualCordonAuditAtomicityLiveDB(t *testing.T) {
	for _, failure := range []string{"event", "before-commit", "lost-commit-response"} {
		t.Run(failure, func(t *testing.T) {
			f := newRollAffinityFixture(t)
			w := f.worker("atomic", 0, false)
			p := &manualFaultPool{pool: f.pool, eventFailure: failure == "event", commitBefore: failure == "before-commit", commitAfter: failure == "lost-commit-response"}
			_, err := store.ChangeWorkerManualCordon(f.ctx, p, f.userID, w, "First", pgtype.Text{String: "original", Valid: true}, true)
			if !errors.Is(err, errCordonFault) {
				t.Fatalf("fault not reported: %v", err)
			}
			expected := 0
			if failure == "lost-commit-response" {
				expected = 1
			}
			if got := manualEventCount(t, f, w); got != expected {
				t.Fatalf("partial audit/state commit: events=%d", got)
			}
			if manualRead(t, f, w).ManualCordonedAt.Valid != (expected == 1) {
				t.Fatal("state/event atomicity broken")
			}
			retry := manualChange(t, f, w, true)
			if manualEventCount(t, f, w) != 1 {
				t.Fatal("retry duplicated audit")
			}
			if failure == "lost-commit-response" && (retry.ManualCordonedByLabel.String != "First" || retry.ManualCordonReason.String != "original") {
				t.Fatal("retry overwrote committed attribution")
			}
			manualChange(t, f, w, false)
			var reason string
			if err := f.pool.QueryRow(f.ctx, "SELECT reason FROM worker_cordon_events WHERE worker_id=$1 AND action='uncordon'", w).Scan(&reason); err != nil {
				t.Fatal(err)
			}
			if reason != retry.ManualCordonReason.String {
				t.Fatalf("uncordon audit reason=%q want previous %q", reason, retry.ManualCordonReason.String)
			}
			// History survives explicit deletion of its worker identity.
			f.deleteWorker(w)
			if manualEventCount(t, f, w) != 2 {
				t.Fatal("worker deletion removed audit history")
			}
		})
	}
}

func TestManualCordonConcurrentRepeatsLiveDB(t *testing.T) {
	f := newRollAffinityFixture(t)
	w := f.worker("repeats", 0, false)
	for _, on := range []bool{true, false} {
		start := make(chan struct{})
		results := make(chan error, 4)
		for range 4 {
			go func() {
				<-start
				_, err := store.ChangeWorkerManualCordon(f.ctx, f.pool, f.userID, w, "Operator", pgtype.Text{}, on)
				results <- err
			}()
		}
		close(start)
		for range 4 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := manualEventCount(t, f, w); n != 2 {
		t.Fatalf("concurrent repeats wrote %d events, want2", n)
	}
}

func waitManualLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waiter, holder uint32) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, "SELECT $1::int = ANY(pg_blocking_pids($2::int))", holder, waiter).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("did not observe the worker-row lock barrier")
		case <-ticker.C:
		}
	}
}

func TestManualCordonClaimSerializationLiveDB(t *testing.T) {
	t.Run("cordon-commits-first", func(t *testing.T) {
		f := newRollAffinityFixture(t)
		w := f.worker("cordon-first", 0, false)
		run := f.unclaimedRun()
		ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		defer cancel()
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		p := &manualFaultPool{pool: f.pool, ready: make(chan struct{}), release: release, pid: make(chan uint32, 1)}
		result := make(chan error, 1)
		go func() {
			_, err := store.ChangeWorkerManualCordon(ctx, p, f.userID, w, "Operator", pgtype.Text{}, true)
			result <- err
		}()
		var holder uint32
		select {
		case holder = <-p.pid:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-p.ready:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		conn, err := f.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		claim := make(chan error, 1)
		go func() { _, e := store.New(conn).ClaimRun(ctx, manualParams(f, w)); claim <- e }()
		waitManualLock(t, ctx, f.pool, conn.Conn().PgConn().PID(), holder)
		unblock()
		if err = <-result; err != nil {
			t.Fatal(err)
		}
		if err = <-claim; !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("post-cordon intake admitted: %v", err)
		}
		var status string
		if err = f.pool.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", run).Scan(&status); err != nil || status != "queued" {
			t.Fatalf("run=%q err=%v", status, err)
		}
	})
	t.Run("claim-commits-first", func(t *testing.T) {
		f := newRollAffinityFixture(t)
		w := f.worker("claim-first", 0, false)
		run := f.unclaimedRun()
		ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		defer cancel()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		got, err := store.New(tx).ClaimRun(ctx, manualParams(f, w))
		if err != nil || got.ID != run {
			t.Fatalf("first claim: %s %v", got.ID, err)
		}
		p := &manualFaultPool{pool: f.pool, pid: make(chan uint32, 1)}
		result := make(chan error, 1)
		go func() {
			_, e := store.ChangeWorkerManualCordon(ctx, p, f.userID, w, "Operator", pgtype.Text{}, true)
			result <- e
		}()
		var waiter uint32
		select {
		case waiter = <-p.pid:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		waitManualLock(t, ctx, f.pool, waiter, tx.Conn().PgConn().PID())
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = <-result; err != nil {
			t.Fatal(err)
		}
		f.unclaimedRun()
		if _, err = f.claim(w, false); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("next intake not fenced: %v", err)
		}
		var status string
		if err = f.pool.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", run).Scan(&status); err != nil || status != "claimed" {
			t.Fatalf("admitted run interrupted: %q %v", status, err)
		}
	})
}
