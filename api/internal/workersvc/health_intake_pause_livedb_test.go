package workersvc

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestIntakePauseHeldProjectionLiveDB: the intake_pause_held projection is true only for a queued,
// not-admitted run while the pause is on, and queuedReason then reports reasonMaintenance. An
// admitted queued run (claim_generation > 0) is never held; lifting the pause clears the hold.
func TestIntakePauseHeldProjectionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	root := newCodexClaimFix(t, env, false)
	env.exec(`UPDATE runs SET claim_generation = 0, status = 'queued' WHERE id = $1`, root.runID)
	adm := newCodexClaimFix(t, env, false) // fixture leaves claim_generation = 1 and status queued

	setPause := func(active bool) {
		t.Helper()
		tx, err := env.pool.Begin(env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.SetInstanceIntakePause(env.ctx, store.New(tx), active, "u2 test", pgtype.UUID{}, "test"); err != nil {
			_ = tx.Rollback(env.ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(env.ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { setPause(false) })

	if healthRowForRun(t, env, root.runID).IntakePauseHeld {
		t.Fatal("run held with no pause")
	}
	setPause(true)
	rootRow := healthRowForRun(t, env, root.runID)
	if !rootRow.IntakePauseHeld {
		t.Fatal("not-admitted queued run not held while paused")
	}
	if got := root.svc.queuedReason(env.ctx, time.Now(), rootRow, thWithLimit(testCustodyLimit)); got != reasonMaintenance {
		t.Fatalf("queuedReason = %q, want %q", got, reasonMaintenance)
	}
	admRow := healthRowForRun(t, env, adm.runID)
	if admRow.IntakePauseHeld {
		t.Fatal("admitted (claim_generation > 0) run must not be held")
	}
	if got := adm.svc.queuedReason(env.ctx, time.Now(), admRow, thWithLimit(testCustodyLimit)); got == reasonMaintenance {
		t.Fatal("admitted run got reasonMaintenance")
	}
	setPause(false)
	if healthRowForRun(t, env, root.runID).IntakePauseHeld {
		t.Fatal("hold persisted after the pause lifted")
	}
}
