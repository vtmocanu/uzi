package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3454 U2: the instance intake pause must keep the ephemeral provisioner from spending
// workers on runs ClaimRun will refuse. Live-DB because the filter is SQL (fn_instance_intake_paused
// and fn_run_admitted_during_intake_pause) and the recheck runs inside provisionOne's real tx.
//
// The pause is instance-global; run-store-it.sh runs packages with -p 1, and each test lifts it on
// cleanup so a sibling never sees a stale pause.

// setIntakePause flips the singleton through the production store helper (tx-bound).
func (fx *ephemeralFixture) setIntakePause(active bool) {
	fx.t.Helper()
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		fx.t.Fatalf("begin pause tx: %v", err)
	}
	if _, _, err := store.SetInstanceIntakePause(fx.ctx, store.New(tx), active, "u2 test", pgtype.UUID{}, "test"); err != nil {
		_ = tx.Rollback(fx.ctx)
		fx.t.Fatalf("SetInstanceIntakePause(%v): %v", active, err)
	}
	if err := tx.Commit(fx.ctx); err != nil {
		fx.t.Fatalf("commit pause tx: %v", err)
	}
}

func (fx *ephemeralFixture) pauseIntakeForTest() {
	fx.t.Helper()
	fx.setIntakePause(true)
	fx.t.Cleanup(func() {
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			return
		}
		if _, _, err := store.SetInstanceIntakePause(fx.ctx, store.New(tx), false, "", pgtype.UUID{}, "test"); err != nil {
			_ = tx.Rollback(fx.ctx)
			return
		}
		_ = tx.Commit(fx.ctx)
	})
}

// admit marks a queued run as a replacement (claim_generation > 0), which the pause admits.
func (fx *ephemeralFixture) admit(runID uuid.UUID) {
	fx.t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET claim_generation = 1 WHERE id = $1`, runID); err != nil {
		fx.t.Fatalf("admit run: %v", err)
	}
}

// TestEphemeralProvisionPassIntakePauseBatchLiveDB: more not-admitted roots than the bounded batch
// (50) must not crowd the one admitted replacement out of the demand window, and no root gets a
// worker while paused; lifting the pause provisions roots again.
func TestEphemeralProvisionPassIntakePauseBatchLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	fx.onlineWorker("base-only", false) // cannot satisfy docker
	const roots = 55                    // > ephemeralProvisionBatch (50)
	rootIDs := make(map[uuid.UUID]bool, roots)
	for i := 0; i < roots; i++ {
		rootIDs[fx.queuedRun([]string{"docker"})] = true
	}
	admitted := fx.queuedRun([]string{"docker"})
	fx.admit(admitted)

	// 55 queued docker roots would otherwise crowd the LIMIT window of sibling suites' demand queries.
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM workers WHERE user_id = $1`, fx.userID)
		_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE user_id = $1`, fx.userID)
	})
	fx.pauseIntakeForTest()
	// Cap well above the run count so the per-user cap never explains the outcome.
	prov := fx.provisioner(true, 100)
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (paused): %v", err)
	}
	rows := fx.ephemeralRows()
	if len(rows) != 1 || rows[0].runID != admitted {
		t.Fatalf("paused pass provisioned %d workers (%v), want exactly one for the admitted run %s", len(rows), rows, admitted)
	}

	fx.setIntakePause(false)
	// Roots drain across ticks at 50 per pass; two passes cover 55 plus the already-served one.
	for i := 0; i < 2; i++ {
		if _, err := prov.ProvisionPass(fx.ctx); err != nil {
			t.Fatalf("ProvisionPass (lifted) %d: %v", i, err)
		}
	}
	got := 0
	for _, r := range fx.ephemeralRows() {
		if rootIDs[r.runID] {
			got++
		}
	}
	if got == 0 {
		t.Fatal("after the pause lifted no root run was provisioned")
	}
}

// TestEphemeralProvisionOneRechecksIntakePauseLiveDB: the pause is enabled AFTER the demand query
// selected the run but BEFORE provisionOne takes its lock. The test holds the user's provision
// advisory lock so the pass blocks inside provisionOne, enables the pause, then releases the lock.
// provisionOne's recheck must then skip: no worker is created.
func TestEphemeralProvisionOneRechecksIntakePauseLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	fx.onlineWorker("base-only", false)
	fx.queuedRun([]string{"docker"})
	fx.t.Cleanup(func() { fx.setIntakePause(false) })

	holder, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(fx.ctx) //nolint:errcheck // no-op after Rollback below
	key := hostedProvisionLockObjID(fx.userID)
	if _, err := holder.Exec(fx.ctx, "SELECT pg_advisory_xact_lock($1, $2)", store.HostedProvisionLockClass, key); err != nil {
		t.Fatalf("take holder lock: %v", err)
	}

	prov := fx.provisioner(true, 100)
	done := make(chan error, 1)
	go func() { _, err := prov.ProvisionPass(fx.ctx); done <- err }()

	// Wait until the pass is parked on our lock, i.e. it already selected the run.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := fx.pool.QueryRow(fx.ctx,
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
			   AND classid::bigint = $1 AND objid::bigint = $2`,
			int64(uint32(store.HostedProvisionLockClass)), int64(uint32(key))).Scan(&waiting); err != nil {
			t.Fatalf("probe pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("pass finished before blocking on the lock (err=%v); the run was not selected", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("pass never blocked on the provision advisory lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

	fx.setIntakePause(true)
	if err := holder.Rollback(fx.ctx); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProvisionPass: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("pass did not finish after the lock was released")
	}
	if rows := fx.ephemeralRows(); len(rows) != 0 {
		t.Fatalf("provisionOne created %d workers after the pause was enabled mid-pass, want 0", len(rows))
	}
}
