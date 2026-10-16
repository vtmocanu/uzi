package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1399: a scope directive committed after SetState's initial ownership
// snapshot must settle on the terminal transition. Legacy reports now lock and
// refresh that snapshot, so directive injection runs before the run lock. A
// separate after-commit hook proves the post-transition reread observes a
// concurrent scope-capped completion before either report settles its disposition.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.

// ssDirectiveBeforeTerminal wraps the live Store and commits a scope directive on the pool
// before the transaction acquires the run lock, after SetState's initial ownership read.
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

// ssMutationBeginner interposes only at transaction boundaries: another connection
// can commit before the run lock, or after commit, without waiting on our own lock.
type ssMutationBeginner struct {
	TxBeginner
	beforeRunLock func()
	afterCommit   func()
}

type ssMutationTx struct {
	pgx.Tx
	beforeRunLock func()
	afterCommit   func()
}

func (b ssMutationBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &ssMutationTx{Tx: tx, beforeRunLock: b.beforeRunLock, afterCommit: b.afterCommit}, nil
}

func (tx *ssMutationTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: GetRunOwnedByWorkerForUpdate") && tx.beforeRunLock != nil {
		tx.beforeRunLock()
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *ssMutationTx) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	if tx.afterCommit != nil {
		tx.afterCommit()
	}
	return nil
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
	w := e.seedWorker(t, nil) // no capabilities: legacy nil-generation report
	runID := e.seedLegacyRunningRun(t, w)
	hook := &ssDirectiveBeforeTerminal{Store: e.q, e: e, t: t, runID: runID}
	svc := e.ssService(t, e.q)
	svc.SetTxBeginner(ssMutationBeginner{TxBeginner: e.pool, beforeRunLock: hook.inject})

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "failed"})
	if err != nil || !applied {
		t.Fatalf("SetState(failed): applied=%v err=%v", applied, err)
	}
	if !hook.fired {
		t.Fatal("pre-lock directive hook never fired: the scope directive was not injected before the terminal write")
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
	w := e.seedWorker(t, nil)             // no capabilities: legacy nil-generation report
	runID := e.seedLegacyRunningRun(t, w) // completion_contract_version NULL: direct SetRunCompleted
	hook := &ssDirectiveBeforeTerminal{Store: e.q, e: e, t: t, runID: runID}
	svc := e.ssService(t, e.q)
	svc.SetTxBeginner(ssMutationBeginner{TxBeginner: e.pool, beforeRunLock: hook.inject})

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "completed"})
	if err != nil || !applied {
		t.Fatalf("SetState(completed): applied=%v err=%v", applied, err)
	}
	if !hook.fired {
		t.Fatal("pre-lock directive hook never fired: the scope directive was not injected before the terminal write")
	}
	if run.Status != "completed" {
		t.Fatalf("run status = %q, want completed", run.Status)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "declined" {
		t.Fatalf("scope disposition = %q (settled=%v), want declined for a directive committed after the unlocked read", disp, settled)
	}
}

// TestLegacyCompletedScopeCappedKeepsAppliedLiveDB: a legacy (non-interlocked) `completed`
// report with ScopeCapped=true on a run whose scope directive was committed BEFORE the report
// stamps stop_kind scope_capped, and the arm's own 'applied' decision is what settles the
// pending scope row. The post-transition fallback must not replace it with 'declined'.
func TestLegacyCompletedScopeCappedKeepsAppliedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)             // no capabilities: legacy nil-generation report
	runID := e.seedLegacyRunningRun(t, w) // completion_contract_version NULL: direct SetRunCompleted
	e.fpSeedScopeDirected(t, runID)       // directive visible to the arm's `owned` read
	svc := e.ssService(t, e.q)

	capped := true
	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "completed", ScopeCapped: &capped})
	if err != nil || !applied {
		t.Fatalf("SetState(completed, scope_capped): applied=%v err=%v", applied, err)
	}
	if run.Status != "completed" {
		t.Fatalf("run status = %q, want completed", run.Status)
	}
	if !run.StopKind.Valid || run.StopKind.String != "scope_capped" {
		t.Fatalf("stop_kind = %+v, want scope_capped", run.StopKind)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "applied" {
		t.Fatalf("scope disposition = %q (settled=%v), want applied for a genuine scope_capped completion", disp, settled)
	}
}

// ssCompleteAfterRunning commits a concurrent scope-capped completion after the
// running report commits, but before its reread and either report's disposition settle.
type ssCompleteAfterRunning struct {
	Store
	e        interlockLiveDB
	t        *testing.T
	runID    uuid.UUID
	workerID uuid.UUID
	fired    bool
}

func (w *ssCompleteAfterRunning) inject(ctx context.Context) {
	if !w.fired {
		w.fired = true
		if n, cerr := w.e.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			StopKind: pgconv.TextOrNull("scope_capped"),
			ID:       w.runID, WorkerID: pgconv.UUID(w.workerID),
		}); cerr != nil || n != 1 {
			w.t.Fatalf("concurrent SetRunCompleted: rows=%d err=%v", n, cerr)
		}
	}
}

// TestRunningReportRereadsScopeCappedCompletionSettlesAppliedLiveDB: a non-terminal `running`
// report (rows>0) whose post-transition re-read sees a run a concurrent completed+ScopeCapped
// report has just committed must not settle the scope row 'declined'; the idempotent settle
// would then beat the completer's 'applied'. The fallback keys on the re-read stop_kind.
func TestRunningReportRereadsScopeCappedCompletionSettlesAppliedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil) // no capabilities: legacy nil-generation report
	runID := e.seedLegacyRunningRun(t, w)
	e.fpSeedScopeDirected(t, runID)
	hook := &ssCompleteAfterRunning{Store: e.q, e: e, t: t, runID: runID, workerID: w}
	svc := e.ssService(t, e.q)
	svc.SetTxBeginner(ssMutationBeginner{TxBeginner: e.pool, afterCommit: func() { hook.inject(e.ctx) }})

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, StateRequest{State: "running"})
	if err != nil || !applied {
		t.Fatalf("SetState(running): applied=%v err=%v", applied, err)
	}
	if !hook.fired {
		t.Fatal("post-commit completion hook never fired: the concurrent completion was not injected")
	}
	if run.Status != "completed" || !run.StopKind.Valid || run.StopKind.String != "scope_capped" {
		t.Fatalf("re-read = status %q stop_kind %+v, want completed/scope_capped", run.Status, run.StopKind)
	}
	if disp, settled := e.scopeDisposition(t, runID); !settled || disp != "applied" {
		t.Fatalf("scope disposition = %q (settled=%v), want applied: a concurrent scope_capped completion must not be settled declined", disp, settled)
	}
}
