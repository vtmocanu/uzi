package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1399: the forge pre-clone park's TERMINAL transitions (cap-fail, stamped-cancel, and
// the nil-txBeginner failForgeUnsettleable fail-safe) settle the run's pending #634 scope audit
// row 'declined', exactly as SetState's `failed` arm does for a scope-directed run that ends
// abnormally. A NORMAL forge park is not terminal, so its scope row stays pending. These drive
// the real Service against a REAL Postgres (real *store.Queries + the pool as the tx beginner,
// except the degraded case, which deliberately wires none).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.

// fpSeedScopeDirected gives the run a live #634 scope_ceiling directive through the same
// atomic CreateScopeCeilingInput SubmitInput's scope path uses (runs.scope_ceiling set AND a
// pending kind='scope' audit row written), then asserts that row is pending, so a later
// non-NULL disposition can only have been written by the forge-park SetState under test.
func (e interlockLiveDB) fpSeedScopeDirected(t *testing.T, runID uuid.UUID) {
	t.Helper()
	if _, err := e.q.CreateScopeCeilingInput(e.ctx, store.CreateScopeCeilingInputParams{
		RunID:        runID,
		Body:         pgconv.TextOrNull("1"),
		ScopeCeiling: pgInt4(1),
	}); err != nil {
		t.Fatalf("CreateScopeCeilingInput: %v", err)
	}
	if disp, settled := e.scopeDisposition(t, runID); settled {
		t.Fatalf("scope audit row must be pending (NULL disposition) before the forge park; got %q", disp)
	}
}

// fpForgeParkReport is the worker's forge_unreachable pre-clone park report at generation 1.
func fpForgeParkReport() StateRequest {
	return StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)}
}

// TestForgeParkCapFailSettlesScopeDeclinedLiveDB: the (cap+1)th forge park fails the run, and
// the terminal fail settles the pending scope row 'declined'.
func TestForgeParkCapFailSettlesScopeDeclinedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 2) // cap 2

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.exec(t, `UPDATE runs SET forge_park_count = 2 WHERE id = $1`, runID)
	mhOpenHold(t, e, runID, 1, w)
	e.fpSeedScopeDirected(t, runID)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, fpForgeParkReport())
	if err != nil || !applied {
		t.Fatalf("SetState(forge park past cap): applied=%v err=%v", applied, err)
	}
	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed (past the forge cap)", run.Status)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "declined" {
		t.Fatalf("scope disposition = %q (settled=%v), want declined on the terminal cap-fail", disp, settled)
	}
}

// fpDirectiveOnBegin is a TxBeginner whose FIRST Begin commits a scope directive on the pool
// before delegating. On the recovery_wait forge path that first Begin is parkForgeUnreachable's:
// after SetState's unlocked `owned` read, before the locked reread.
type fpDirectiveOnBegin struct {
	e     interlockLiveDB
	t     *testing.T
	runID uuid.UUID
	fired bool
}

func (b *fpDirectiveOnBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	if !b.fired {
		b.fired = true
		b.e.fpSeedScopeDirected(b.t, b.runID)
	}
	return b.e.pool.Begin(ctx)
}

// TestForgeParkScopeDirectiveDuringParkSettlesDeclinedLiveDB: a scope directive that commits
// between SetState's unlocked read and the forge park's FOR UPDATE reread is still settled
// 'declined' by the terminal cap-fail (the settle reads the post-transition row, not `owned`).
func TestForgeParkScopeDirectiveDuringParkSettlesDeclinedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 2) // cap 2

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.exec(t, `UPDATE runs SET forge_park_count = 2 WHERE id = $1`, runID)
	mhOpenHold(t, e, runID, 1, w)
	hook := &fpDirectiveOnBegin{e: e, t: t, runID: runID}
	svc.SetTxBeginner(hook)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, fpForgeParkReport())
	if err != nil || !applied {
		t.Fatalf("SetState(forge park past cap): applied=%v err=%v", applied, err)
	}
	if !hook.fired {
		t.Fatal("Begin hook never fired: the scope directive was not injected mid-park")
	}
	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed (past the forge cap)", run.Status)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "declined" {
		t.Fatalf("scope disposition = %q (settled=%v), want declined for a directive committed mid-park", disp, settled)
	}
}

// TestForgeParkCancelSettlesScopeDeclinedLiveDB: a cancel stamped during the clone retries wins
// the park (status cancelled), and that terminal cancel settles the scope row 'declined'.
func TestForgeParkCancelSettlesScopeDeclinedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.exec(t, `UPDATE runs SET stop_kind = 'cancelled' WHERE id = $1`, runID) // cancel stamped, status still running
	mhOpenHold(t, e, runID, 1, w)
	e.fpSeedScopeDirected(t, runID)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, fpForgeParkReport())
	if err != nil || !applied {
		t.Fatalf("SetState(cancel-during-forge-park): applied=%v err=%v", applied, err)
	}
	if run.Status != "cancelled" {
		t.Fatalf("run status = %q, want cancelled (a stamped cancel wins the park)", run.Status)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "declined" {
		t.Fatalf("scope disposition = %q (settled=%v), want declined on the terminal cancel", disp, settled)
	}
}

// TestForgeParkNormalParkLeavesScopePendingLiveDB: an ordinary forge park (under the cap, no
// stamped verdict) is NOT terminal, so the run may still apply its scope cap and the scope row
// stays pending.
func TestForgeParkNormalParkLeavesScopePendingLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.forgeParkService(t, 6)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	mhOpenHold(t, e, runID, 1, w)
	e.fpSeedScopeDirected(t, runID)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, fpForgeParkReport())
	if err != nil || !applied {
		t.Fatalf("SetState(forge park): applied=%v err=%v", applied, err)
	}
	if run.Status != "recovery_wait" {
		t.Fatalf("run status = %q, want recovery_wait (an ordinary forge park)", run.Status)
	}
	if disp, settled := e.scopeDisposition(t, runID); settled {
		t.Fatalf("scope disposition = %q, want still pending (NULL) on a non-terminal park", disp)
	}
}

// TestForgeParkNilTxBeginnerFailSettlesScopeDeclinedLiveDB: with no tx beginner wired the forge
// park takes the failForgeUnsettleable fail-safe (status failed), and that terminal fail settles
// the scope row 'declined'. The recovery_wait state skips SetState's outer fence, so this path
// needs no transaction.
func TestForgeParkNilTxBeginnerFailSettlesScopeDeclinedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := New(e.q, newBox(t), testParams()) // deliberately NO SetTxBeginner
	svc.SetBackground(func(func()) {})

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.fpSeedScopeDirected(t, runID)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, fpForgeParkReport())
	if err != nil || !applied {
		t.Fatalf("SetState(forge park, nil txBeginner): applied=%v err=%v", applied, err)
	}
	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed (the nil-txBeginner fail-safe)", run.Status)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "declined" {
		t.Fatalf("scope disposition = %q (settled=%v), want declined on the terminal fail-safe", disp, settled)
	}
}
