package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// checkpoint_retention_pass_livedb_test.go pins the checkpoint-retention pass's time bound
// (PRD #1810, CodeRabbit on PR #1819) against a REAL Postgres: with a forge that hangs until its
// context ends, the pass stops starting records once its budget is spent, each record it starts
// gives up after the sweeper's per-record timeout, the timed-out record still gets its backoff
// written, and Sweep's later passes run in the same tick.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// hangingForge is a forge whose every call blocks until its context is done (it honours ctx, as
// the pushbroker seams do) or the test releases it, counting the calls.
type hangingForge struct {
	release chan struct{}
	calls   atomic.Int32
}

func newHangingForge(t *testing.T) *hangingForge {
	t.Helper()
	h := &hangingForge{release: make(chan struct{})}
	t.Cleanup(h.unblock)
	return h
}

// unblock releases every call still hanging, and every later one; safe to call more than once.
func (h *hangingForge) unblock() {
	select {
	case <-h.release:
	default:
		close(h.release)
	}
}

func (h *hangingForge) hang(ctx context.Context) error {
	h.calls.Add(1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.release:
		return errors.New("hangingForge: released by the test")
	}
}

// newHangingRetentionSvc is a Service on e's database with every retention seam wired to h, the
// pass budget and per-record timeout set as given.
func newHangingRetentionSvc(t *testing.T, e interlockLiveDB, h *hangingForge, p Params, budget, opTimeout time.Duration) *Service {
	t.Helper()
	box := newBox(t)
	pat := strings.Join([]string{"bot", "pat", "budget", uuid.NewString()}, "-")
	sealed, err := box.Seal([]byte(pat))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	e.exec(t, `UPDATE forge_connections SET token_ciphertext = $1
	           WHERE id = (SELECT connection_id FROM repos WHERE id = $2)`, sealed, e.repoID)
	svc := New(e.q, box, p)
	svc.SetTxBeginner(e.pool)
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	svc.SetBackground(func(fn func()) { fn() })
	svc.SetRetentionLockPool(e.pool)
	svc.SetPublishFn(func(ctx context.Context, _ pushbroker.Options) (pushbroker.Result, error) {
		return pushbroker.Result{}, h.hang(ctx)
	})
	svc.SetDeleteCheckpointFn(func(ctx context.Context, _ pushbroker.DeleteOptions) error { return h.hang(ctx) })
	svc.SetCreateRefFn(func(ctx context.Context, _ pushbroker.CreateRefOptions) error { return h.hang(ctx) })
	svc.SetListRefTipsFn(func(ctx context.Context, _ pushbroker.ListRefsOptions, _ ...string) (map[string]string, error) {
		return nil, h.hang(ctx)
	})
	svc.retentionPassBudget = budget
	svc.retentionSweepOpTimeout = opTimeout
	return svc
}

// seedSettlingRun seeds a failed issue run that published retentionTestTip and has no custody
// hold, so migration 00265's trigger records it `settling` (due now) in its terminal transaction.
func seedSettlingRun(t *testing.T, e interlockLiveDB) uuid.UUID {
	t.Helper()
	w := e.seedWorker(t, nil)
	runID := e.seedLegacyRunningRun(t, w)
	e.exec(t, `UPDATE runs SET checkpoint_tip = $2, checkpoint_tip_at = now(), claim_generation = 1 WHERE id = $1`, runID, retentionTestTip)
	e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, runID)
	var state string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM checkpoint_retentions WHERE run_id = $1`, runID).Scan(&state); err != nil {
		t.Fatalf("setup: read the terminal trigger's record: %v", err)
	}
	if state != retentionSettling {
		t.Fatalf("setup: record = %q, want settling", state)
	}
	return runID
}

// retentionBookkeeping reads a record's failure bookkeeping.
type retentionBookkeeping struct {
	attempts  int32
	future    bool // next_attempt_at is after now()
	lastError pgtype.Text
	state     string
}

func readRetentionBookkeeping(t *testing.T, e interlockLiveDB, runID uuid.UUID) retentionBookkeeping {
	t.Helper()
	var b retentionBookkeeping
	if err := e.pool.QueryRow(e.ctx, `SELECT attempts, next_attempt_at > now(), last_error, state
	                                  FROM checkpoint_retentions WHERE run_id = $1`, runID).
		Scan(&b.attempts, &b.future, &b.lastError, &b.state); err != nil {
		t.Fatalf("read record bookkeeping: %v", err)
	}
	return b
}

// TestSweepRetentionPassBudgetLiveDB: with a forge that hangs until its context ends and several
// due settling records (each settle calls the hanging delete), Sweep returns within the pass budget
// plus one per-record timeout plus generous slack, and a LATER pass of the same Sweep ran (the
// upload-retry-window expiry flipped a stalled capture). Sweep runs the UNCONFINED pass, so any
// leftover records of a reused database are driven against the hanging forge too; the bound holds
// regardless, since every record, leftover or not, is subject to the same budget. On the code
// before the budget each record took the 2-minute retentionOpTimeout and Sweep did not return
// within the bound.
func TestSweepRetentionPassBudgetLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	h := newHangingForge(t)
	p := testParams()
	p.RecoveryUploadRetryWindow = 24 * time.Hour
	const (
		budget    = 300 * time.Millisecond
		opTimeout = 200 * time.Millisecond
		bound     = 5 * time.Second // budget + opTimeout + generous slack for the other passes
	)
	svc := newHangingRetentionSvc(t, e, h, p, budget, opTimeout)

	const seeded = 4
	runs := make([]uuid.UUID, 0, seeded)
	for range seeded {
		runs = append(runs, seedSettlingRun(t, e))
	}

	// A capture stalled well past the retry window, on a live run's open hold: the pass AFTER the
	// retention pass flips it to needs_action.
	w := e.seedWorker(t, nil)
	holdRun := e.seedLegacyRunningRun(t, w)
	hold := mhOpenHold(t, e, holdRun, 1, w)
	captureID := uuid.New()
	e.exec(t, `INSERT INTO recovery_captures
	             (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state, created_at)
	           VALUES ($1, $2, $3, $4, 'ident', 'H', $5, 'preparing', now() - interval '2 days')`,
		captureID, hold, holdRun, e.userID, uuid.NewString())

	type sweepOut struct {
		res SweepResult
		err error
	}
	done := make(chan sweepOut, 1)
	start := time.Now()
	go func() {
		res, err := svc.Sweep(e.ctx)
		done <- sweepOut{res, err}
	}()
	var out sweepOut
	select {
	case out = <-done:
	case <-time.After(bound):
		t.Errorf("Sweep did not return within %s (pass budget %s, per-record timeout %s): the retention pass is not bounded",
			bound, budget, opTimeout)
		h.unblock()
		out = <-done
	}
	elapsed := time.Since(start)
	if out.err != nil {
		t.Fatalf("Sweep: %v", out.err)
	}
	t.Logf("Sweep returned after %s; hanging forge calls: %d", elapsed.Round(time.Millisecond), h.calls.Load())

	if h.calls.Load() == 0 {
		t.Fatal("the retention pass never reached the hanging forge; the bound was not exercised")
	}
	var captureState string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM recovery_captures WHERE id = $1`, captureID).Scan(&captureState); err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if captureState != "needs_action" || out.res.RecoveryStalled < 1 {
		t.Fatalf("stalled capture = %q, RecoveryStalled = %d; want needs_action and >= 1 (the pass after the retention pass ran)",
			captureState, out.res.RecoveryStalled)
	}
	// The budget, not the page, ended the pass: at most a couple of hanging records fit in it, so
	// some seeded record was never started (attempts still 0, no last_error).
	untouched := 0
	for _, r := range runs {
		if b := readRetentionBookkeeping(t, e, r); b.attempts == 0 && !b.lastError.Valid {
			untouched++
		}
	}
	if untouched == 0 {
		t.Fatalf("every one of the %d seeded records was started; want the spent budget to leave some for the next tick", seeded)
	}
}

// TestRetentionForgeTimeoutRecordsBackoffLiveDB: a settle whose forge delete runs into the sweeper's
// per-record timeout still writes its backoff (attempts+1, next_attempt_at in the future, the
// deadline as last_error), on a fresh bookkeeping context, so the next pass moves past the record
// instead of starting with it again. Before the fix the failure write ran on the expired operation
// context and wrote nothing.
func TestRetentionForgeTimeoutRecordsBackoffLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	h := newHangingForge(t)
	svc := newHangingRetentionSvc(t, e, h, testParams(), time.Minute, 200*time.Millisecond)
	runID := seedSettlingRun(t, e)

	if _, err := svc.reconcileCheckpointRetentions(e.ctx, pgconv.UUID(runID)); err != nil {
		t.Fatalf("reconcileCheckpointRetentions: %v", err)
	}
	if n := h.calls.Load(); n != 1 {
		t.Fatalf("hanging forge calls = %d, want exactly 1 (the settle's delete)", n)
	}
	b := readRetentionBookkeeping(t, e, runID)
	if b.state != retentionSettling || b.attempts != 1 || !b.future ||
		!b.lastError.Valid || !strings.Contains(b.lastError.String, context.DeadlineExceeded.Error()) {
		t.Fatalf("record = {state %q attempts %d next_attempt_at-in-future %v last_error %q}, "+
			"want settling, 1, true and the deadline recorded despite the expired operation context",
			b.state, b.attempts, b.future, b.lastError.String)
	}

	// The next pass does not start the record again: it is not due.
	if _, err := svc.reconcileCheckpointRetentions(e.ctx, pgconv.UUID(runID)); err != nil {
		t.Fatalf("second reconcileCheckpointRetentions: %v", err)
	}
	if n := h.calls.Load(); n != 1 {
		t.Fatalf("hanging forge calls after a second pass = %d, want still 1 (the record's backoff moved it past)", n)
	}
}
