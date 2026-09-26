package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1399: SetState's `failed` and `completed` arms decide the scope-audit settle from
// `owned`, which for a LEGACY (nil ClaimGeneration) report is the UNLOCKED pre-switch read. A
// scope directive (CreateScopeCeilingInput) that commits after that read but before the
// terminal write must still see its pending run_user_inputs row settled 'declined', decided off
// the post-transition re-read. These inject the directive from inside the terminal store call,
// so it commits while the run is still running and after `owned` was read.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.

// ssDirectiveBeforeTerminal wraps the live Store and commits a scope directive on the pool
// immediately before delegating the terminal write (SetRunFailed / SetRunCompleted).
type ssDirectiveBeforeTerminal struct {
	Store
	e     interlockLiveDB
	t     *testing.T
	runID uuid.UUID
	fired bool
}

func (w *ssDirectiveBeforeTerminal) inject() {
	if !w.fired {
		w.fired = true
		w.e.fpSeedScopeDirected(w.t, w.runID)
	}
}

func (w *ssDirectiveBeforeTerminal) SetRunFailed(ctx context.Context, arg store.SetRunFailedParams) (int64, error) {
	w.inject()
	return w.Store.SetRunFailed(ctx, arg)
}

func (w *ssDirectiveBeforeTerminal) SetRunCompleted(ctx context.Context, arg store.SetRunCompletedParams) (int64, error) {
	w.inject()
	return w.Store.SetRunCompleted(ctx, arg)
}

// ssService builds a Service over the wrapped live store, with the pool as the tx beginner and
// background work dropped, mirroring permitService.
func (e interlockLiveDB) ssService(t *testing.T, q Store) *Service {
	t.Helper()
	svc := New(q, newBox(t), testParams())
	svc.SetTxBeginner(e.pool)
	svc.SetBackground(func(func()) {})
	return svc
}

// TestLegacyFailedScopeDirectiveAfterSnapshotSettlesDeclinedLiveDB: a legacy `failed` report
// whose run gains a scope directive after SetState's unlocked read still settles the row
// 'declined' on the terminal fail.
func TestLegacyFailedScopeDirectiveAfterSnapshotSettlesDeclinedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil) // no capabilities: legacy, unfenced report
	runID := e.seedLegacyRunningRun(t, w)
	hook := &ssDirectiveBeforeTerminal{Store: e.q, e: e, t: t, runID: runID}
	svc := e.ssService(t, hook)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "failed"})
	if err != nil || !applied {
		t.Fatalf("SetState(failed): applied=%v err=%v", applied, err)
	}
	if !hook.fired {
		t.Fatal("SetRunFailed hook never fired: the scope directive was not injected before the terminal write")
	}
	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "declined" {
		t.Fatalf("scope disposition = %q (settled=%v), want declined for a directive committed after the unlocked read", disp, settled)
	}
}

// TestLegacyCompletedScopeDirectiveAfterSnapshotSettlesDeclinedLiveDB: a legacy (non-interlocked)
// `completed` report whose run gains a scope directive after SetState's unlocked read still
// settles the row 'declined' on the committed completion.
func TestLegacyCompletedScopeDirectiveAfterSnapshotSettlesDeclinedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)             // no capabilities: legacy, unfenced report
	runID := e.seedLegacyRunningRun(t, w) // completion_contract_version NULL: direct SetRunCompleted
	hook := &ssDirectiveBeforeTerminal{Store: e.q, e: e, t: t, runID: runID}
	svc := e.ssService(t, hook)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "completed"})
	if err != nil || !applied {
		t.Fatalf("SetState(completed): applied=%v err=%v", applied, err)
	}
	if !hook.fired {
		t.Fatal("SetRunCompleted hook never fired: the scope directive was not injected before the terminal write")
	}
	if run.Status != "completed" {
		t.Fatalf("run status = %q, want completed", run.Status)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "declined" {
		t.Fatalf("scope disposition = %q (settled=%v), want declined for a directive committed after the unlocked read", disp, settled)
	}
}
