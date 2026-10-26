package handler

import (
	"fmt"
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

// tryIntakePause flips the singleton through the production store helper (tx-bound).
func (fx *ephemeralFixture) tryIntakePause(active bool) error {
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		return fmt.Errorf("begin pause tx: %w", err)
	}
	if _, _, err := store.SetInstanceIntakePause(fx.ctx, store.New(tx), active, "u2 test", pgtype.UUID{}, "test"); err != nil {
		_ = tx.Rollback(fx.ctx)
		return fmt.Errorf("SetInstanceIntakePause(%v): %w", active, err)
	}
	if err := tx.Commit(fx.ctx); err != nil {
		return fmt.Errorf("commit pause tx: %w", err)
	}
	return nil
}

func (fx *ephemeralFixture) setIntakePause(active bool) {
	fx.t.Helper()
	if err := fx.tryIntakePause(active); err != nil {
		fx.t.Fatal(err)
	}
}

// liftPauseOnCleanup guarantees the instance-global pause is lifted when the test ends and reports
// a failure to do so, since a stale pause would silently starve every sibling suite.
func (fx *ephemeralFixture) liftPauseOnCleanup() {
	fx.t.Helper()
	fx.t.Cleanup(func() {
		if err := fx.tryIntakePause(false); err != nil {
			fx.t.Errorf("cleanup: lifting the intake pause failed, siblings may see a stale pause: %v", err)
		}
	})
}

func (fx *ephemeralFixture) pauseIntakeForTest() {
	fx.t.Helper()
	fx.setIntakePause(true)
	fx.liftPauseOnCleanup()
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
// worker while paused; lifting the pause provisions every root. One case per demand query that
// carries the pause conjunct: capability gap (Unplaceable), saturation and isolated lane.
func TestEphemeralProvisionPassIntakePauseBatchLiveDB(t *testing.T) {
	const roots = 55 // > ephemeralProvisionBatch (50)
	cases := []struct {
		name  string
		delay time.Duration
		// setup seeds the fleet and returns the seeder of one queued run the lane's demand query selects.
		setup func(fx *ephemeralFixture) (root func() uuid.UUID)
		// finish runs after all runs exist.
		finish func(fx *ephemeralFixture)
	}{
		{
			name: "capability gap",
			setup: func(fx *ephemeralFixture) func() uuid.UUID {
				fx.onlineWorker("base-only", false) // cannot satisfy docker
				return func() uuid.UUID { return fx.queuedRun([]string{"docker"}) }
			},
		},
		{
			name:  "saturation",
			delay: time.Minute,
			setup: func(fx *ephemeralFixture) func() uuid.UUID {
				// The only capable worker is at its run cap, so every queued root is slot-blocked.
				fx.activeRunOn(fx.onlineWorkerWithCap("persistent busy", false, 1))
				return func() uuid.UUID { return fx.queuedRun([]string{}) }
			},
			finish: func(fx *ephemeralFixture) {
				if _, err := fx.pool.Exec(fx.ctx,
					`UPDATE runs SET status_since = now() - interval '2 hours' WHERE user_id = $1 AND status = 'queued'`, fx.userID); err != nil {
					fx.t.Fatalf("age queued runs past the saturation delay: %v", err)
				}
			},
		},
		{
			name: "isolated lane",
			setup: func(fx *ephemeralFixture) func() uuid.UUID {
				prof := fx.boundProfile()
				return func() uuid.UUID { return fx.boundQueuedRun(prof, nil) }
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEphemeralFixture(t, true)
			// Lowest-registered cleanup runs last: drop the 56 queued runs so they never crowd the LIMIT
			// window of sibling suites' demand queries.
			t.Cleanup(func() {
				if _, err := fx.pool.Exec(fx.ctx, `DELETE FROM workers WHERE user_id = $1`, fx.userID); err != nil {
					t.Errorf("cleanup workers: %v", err)
				}
				if _, err := fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE user_id = $1`, fx.userID); err != nil {
					t.Errorf("cleanup runs: %v", err)
				}
			})
			root := tc.setup(fx)
			rootIDs := make(map[uuid.UUID]bool, roots)
			for i := 0; i < roots; i++ {
				rootIDs[root()] = true
			}
			admitted := root()
			fx.admit(admitted)
			if tc.finish != nil {
				tc.finish(fx)
			}

			fx.pauseIntakeForTest()
			// Cap well above the run count so the per-user cap never explains the outcome.
			prov := fx.provisionerWithDelay(true, 100, tc.delay)
			if _, err := prov.ProvisionPass(fx.ctx); err != nil {
				t.Fatalf("ProvisionPass (paused): %v", err)
			}
			rows := fx.ephemeralRows()
			if len(rows) != 1 || rows[0].runID != admitted {
				t.Fatalf("paused pass provisioned %d workers (%v), want exactly one for the admitted run %s", len(rows), rows, admitted)
			}

			fx.setIntakePause(false)
			// Roots drain at 50 per pass; three passes leave slack for sibling rows and must provision
			// every root exactly once (the per-user cap is far above 56).
			for i := 0; i < 3; i++ {
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
			if got != roots {
				t.Fatalf("after the pause lifted %d of %d root runs were provisioned", got, roots)
			}
		})
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
	fx.liftPauseOnCleanup()

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
			int64(store.HostedProvisionLockClass)&0xFFFFFFFF, int64(key)&0xFFFFFFFF).Scan(&waiting); err != nil {
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
