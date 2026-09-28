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
// hold, so migration 00266's trigger records it `settling` (due now) in its terminal transaction.
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

// stuckExitFix is a superseding record of the old run whose supersession stopped (the recovery ref
// sits at another tip) and whose hold is discarded, so the work arm's next pass takes the stuck
// exit: one list of both refs, then the branch-ref delete behind the fence.
func stuckExitFix(t *testing.T) *supersedeFix {
	t.Helper()
	f := newSupersedeFix(t)
	f.forge.set(f.recoveryRef, supersedeOtherTip)
	f.stopSupersession(t)
	f.discardHold(t)
	return f
}

// TestRetentionExpiredFenceRecordsBackoffLiveDB (#1810 review N2): the stuck exit's list returns
// just AFTER the sweeper's per-record deadline, so the fence before the branch-ref delete runs on
// the expired context and fails. That is not a lost lock and not a forge failure, and the step
// records nothing for it; lockedRetentionStep's expiry bookkeeping must still push the record out
// (attempts+1, next_attempt_at in the future), so the next pass does not start it first again.
// Before the fix the record stayed due with its attempts unchanged, and the next pass listed again.
func TestRetentionExpiredFenceRecordsBackoffLiveDB(t *testing.T) {
	f := stuckExitFix(t)
	before := f.row(t)
	svc := f.svc2
	svc.retentionSweepOpTimeout = 300 * time.Millisecond
	var lists atomic.Int32
	svc.SetListRefTipsFn(func(ctx context.Context, o pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
		lists.Add(1)
		<-ctx.Done() // the forge answers just after the operation's deadline, with a good answer
		return f.forge.listRefTips(context.Background(), o, refs...)
	})

	f.reconcileRun(t, svc, f.oldRun)
	if n := lists.Load(); n != 1 {
		t.Fatalf("lists = %d, want 1 (the stuck exit's)", n)
	}
	if tip, ok := f.forge.ref(f.branchRef); !ok || tip != retentionTestTip {
		t.Fatalf("branch ref = %q (present %v), want left at %s: the fence on the expired context must stop the delete",
			tip, ok, retentionTestTip)
	}
	r := f.row(t)
	if r.State != retentionSuperseding || r.Attempts != before.Attempts+1 || !r.NextAttemptAt.Time.After(time.Now()) ||
		!strings.Contains(r.LastError.String, "operation timed out") {
		t.Fatalf("record = {state %q attempts %d (before %d) next_attempt_at %v last_error %q}, want superseding, "+
			"one more attempt, a future next attempt and the timeout recorded", r.State, r.Attempts, before.Attempts,
			r.NextAttemptAt.Time, r.LastError.String)
	}

	// Not due any more: the next pass does not start the record again.
	f.reconcileRun(t, svc, f.oldRun)
	if n := lists.Load(); n != 1 {
		t.Fatalf("lists after a second pass = %d, want still 1 (the backoff moved the record past)", n)
	}
}

// TestRetentionLockLostLiveRecordsNothingLiveDB (#1810 review N2, the other side): a lock
// genuinely lost while the operation's deadline is still live (released on the pinned session just
// before the stuck exit's fence) stops the delete and records NOTHING: another holder may own the
// record now, so neither the step nor the expiry bookkeeping writes to it.
func TestRetentionLockLostLiveRecordsNothingLiveDB(t *testing.T) {
	f := stuckExitFix(t)
	before := f.row(t)
	unlock := f.holdLockLost(t)
	f.svc1.retentionHooks.beforeForgeWrite = func(id uuid.UUID, op string) {
		if id == f.oldRun && op == "exit-branch" {
			unlock()
		}
	}

	f.reconcileRun(t, f.svc1, f.oldRun)
	if tip, ok := f.forge.ref(f.branchRef); !ok || tip != retentionTestTip {
		t.Fatalf("branch ref = %q (present %v), want left at %s: the lost lock must stop the delete", tip, ok, retentionTestTip)
	}
	r := f.row(t)
	if r.State != before.State || r.Attempts != before.Attempts || !r.NextAttemptAt.Time.Equal(before.NextAttemptAt.Time) ||
		r.LastError != before.LastError {
		t.Fatalf("record = {state %q attempts %d next %v last_error %q}, want untouched {%q %d %v %q}",
			r.State, r.Attempts, r.NextAttemptAt.Time, r.LastError.String,
			before.State, before.Attempts, before.NextAttemptAt.Time, before.LastError.String)
	}
}

// TestRetentionLockLostThenExpiredRecordsNothingLiveDB (#1810 rework review N4): the lock is lost
// mid-operation (released on the pinned session at the stuck exit's branch-ref write, as a forge
// call running into the deadline could see it lost), and the step's fence then runs only after the
// operation's deadline, so it fails on the expired context and the step is classified expired, not
// lost-live. The expiry bookkeeping's fence re-check, on a fresh bookkeeping context, must find the
// lock gone and write NOTHING: another holder may own the record. Before the re-check the record
// got attempts+1 and a backoff without the lock.
func TestRetentionLockLostThenExpiredRecordsNothingLiveDB(t *testing.T) {
	const opTimeout = 300 * time.Millisecond
	f := stuckExitFix(t)
	before := f.row(t)
	unlock := f.holdLockLost(t)
	f.svc1.retentionSweepOpTimeout = opTimeout
	var reached atomic.Int32
	f.svc1.retentionHooks.beforeForgeWrite = func(id uuid.UUID, op string) {
		if id == f.oldRun && op == "exit-branch" {
			reached.Add(1)
			unlock()
			time.Sleep(opTimeout + 200*time.Millisecond) // the fence then runs on the expired context
		}
	}

	f.reconcileRun(t, f.svc1, f.oldRun)
	if n := reached.Load(); n != 1 {
		t.Fatalf("exit-branch write reached %d times, want 1", n)
	}
	if tip, ok := f.forge.ref(f.branchRef); !ok || tip != retentionTestTip {
		t.Fatalf("branch ref = %q (present %v), want left at %s: the failed fence must stop the delete", tip, ok, retentionTestTip)
	}
	r := f.row(t)
	if r.State != before.State || r.Attempts != before.Attempts || !r.NextAttemptAt.Time.Equal(before.NextAttemptAt.Time) ||
		r.LastError != before.LastError {
		t.Fatalf("record = {state %q attempts %d next %v last_error %q}, want untouched {%q %d %v %q}",
			r.State, r.Attempts, r.NextAttemptAt.Time, r.LastError.String,
			before.State, before.Attempts, before.NextAttemptAt.Time, before.LastError.String)
	}
}

// TestAttemptsArmChecksBudgetBeforeOwedSettleLiveDB (#1810 review N3): the attempts arm re-records
// a late-landed tip on a settling record, which owes a settle (a second locked operation). When the
// comparison itself ran past the pass budget, the arm must not start that settle: the ref stays for
// the work arm's later pass. The control subtest, with an ample budget, shows the same setup does
// reach the settle and deletes the ref.
func TestAttemptsArmChecksBudgetBeforeOwedSettleLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name       string
		budget     time.Duration
		listDelay  time.Duration
		wantSettle bool
	}{
		{"the comparison spends the budget", 200 * time.Millisecond, 300 * time.Millisecond, false},
		// A whole locked operation (lock, reads, list, re-record transaction) must fit in the budget
		// under -race: 3s, not the spent case's 200ms.
		{"control: budget left", 3 * time.Second, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupersedeFix(t)
			f.discardHold(t)
			// Settling but not due, so only the attempts arm's owed settle can delete the ref.
			f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'settling', next_attempt_at = now() + interval '1 hour'
			             WHERE run_id = $1`, f.oldRun)
			f.recordAttempt(t, lateTip)
			f.forge.set(f.branchRef, lateTip)
			svc := f.svc2
			svc.SetListRefTipsFn(func(ctx context.Context, o pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
				time.Sleep(tc.listDelay)
				return f.forge.listRefTips(ctx, o, refs...)
			})

			pass := &retentionPass{deadline: time.Now().Add(tc.budget), opTimeout: 30 * time.Second}
			if _, err := svc.reconcilePublishAttempts(f.e.ctx, pgconv.UUID(f.oldRun), pass); err != nil {
				t.Fatalf("reconcilePublishAttempts: %v", err)
			}
			if r := f.row(t); r.Tip != lateTip {
				t.Fatalf("record tip = %s, want the late tip %s re-recorded", r.Tip, lateTip)
			}
			if n := f.attemptCount(t, f.oldRun); n != 0 {
				t.Fatalf("attempt rows = %d, want 0 (resolved by the re-record)", n)
			}
			_, deletes := f.forge.counts(f.branchRef)
			if settled := deletes == 1; settled != tc.wantSettle {
				t.Fatalf("branch ref deletes = %d, want the owed settle to run: %v", deletes, tc.wantSettle)
			}
		})
	}
}

// TestLockedRetentionStepExpiryRuleLiveDB (#1810 review N2, N4): lockedRetentionStep's expiry
// bookkeeping runs for a step that failed after the deadline because its fence ran on the expired
// context while the lock is still held, and never when the lock is gone: a fence that started
// while the deadline was live found it lost (even when that step only returns after the deadline),
// or the lock was lost mid-operation and the step's first fence ran only after the deadline (the
// expiry path's own fence re-check, on a fresh bookkeeping context, finds it gone). Either way the
// error still wraps ErrRetentionLockLost.
func TestLockedRetentionStepExpiryRuleLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		// loseLock releases the lock mid-step; fenceLive runs the step's fence before the deadline.
		loseLock, fenceLive bool
		wantExpiries        int32
	}{
		{"the fence runs on the expired context", false, false, 1},
		{"the lock is lost while live and the step returns after the deadline", true, true, 0},
		{"the lock is lost, then the fence runs on the expired context", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupersedeFix(t)
			unlock := f.holdLockLost(t)
			var (
				expiries  atomic.Int32
				opDone    time.Time
				bookkeepD time.Time
			)
			_, acquired, err := f.svc1.lockedRetentionStep(f.e.ctx, f.oldRun, 300*time.Millisecond,
				func(ctx context.Context, _ uuid.UUID, fence func(context.Context) error) (bool, error) {
					if tc.loseLock {
						unlock() // lost mid-operation (say, during a forge call)
					}
					if tc.fenceLive {
						ferr := fence(ctx) // started while the deadline is live
						<-ctx.Done()
						return false, ferr
					}
					<-ctx.Done()
					opDone = time.Now()
					// The step's own failure bookkeeping would run here; the expiry bookkeeping
					// that follows must still end within ONE window past the operation's deadline.
					time.Sleep(time.Second)
					return false, fence(ctx) // first fenced after the deadline
				},
				func(bctx context.Context, _ uuid.UUID, _ string) {
					expiries.Add(1)
					bookkeepD, _ = bctx.Deadline()
				})
			if !acquired || !errors.Is(err, ErrRetentionLockLost) {
				t.Fatalf("lockedRetentionStep = acquired %v, err %v; want acquired and a failed fence", acquired, err)
			}
			if n := expiries.Load(); n != tc.wantExpiries {
				t.Fatalf("expiry bookkeeping ran %d times, want %d", n, tc.wantExpiries)
			}
			// #1810 rework review N2: the expiry bookkeeping's context ends within
			// retentionRecordTimeout of the operation's deadline, not a fresh window after the
			// step's late return (which here comes a second after the deadline).
			if tc.wantExpiries == 1 {
				if limit := opDone.Add(retentionRecordTimeout + 500*time.Millisecond); bookkeepD.IsZero() || bookkeepD.After(limit) {
					t.Fatalf("expiry bookkeeping deadline = %v, want by %v (the operation's deadline + %s)", bookkeepD, limit, retentionRecordTimeout)
				}
			}
		})
	}
}

// TestAttemptsArmExpiryRuleLiveDB (#1810 review N2, the attempts arm): an attempt row whose locked
// step reaches the attempt-delete fence is deferred when that fence failed only because it ran on
// the expired context (the step is held just before it until the deadline has passed), and left
// untouched when the lock is not held: lost while the deadline was live, or lost and only then
// found by a fence or a database read that ran after the deadline (the expiry path's fence
// re-check sees it gone, and its error wraps ErrRetentionLockLost, so the arm does not defer the
// row after the unlock either). Another holder may be reconciling the row.
//
// The expired-context subtest reaches the fence itself: the comparison's list and reads all run
// before the deadline, and only the beforeForgeWrite hook outlasts it (review N3: a list delayed
// past the deadline failed the tip-claims read first and never reached the fence). Passing nil
// instead of the expiry callback in reconcilePublishAttempts fails that subtest.
func TestAttemptsArmExpiryRuleLiveDB(t *testing.T) {
	const opTimeout = 300 * time.Millisecond
	for _, tc := range []struct {
		name string
		// loseLock releases the lock at the attempt-delete hook; pastDeadline then holds the step
		// there until the operation's deadline has passed, so the fence runs on the expired context.
		// inList instead releases it inside the comparison's list, which answers only after the
		// deadline, so a database read (not a fence) is the first to fail on the expired context.
		loseLock, pastDeadline, inList bool
		wantCheck                      int32
		wantFenceReached               int32
	}{
		{"the fence runs on the expired context", false, true, false, 1, 1},
		{"the lock is lost, then the fence runs on the expired context", true, true, false, 0, 1},
		{"the lock is lost while live", true, false, false, 0, 1},
		{"the lock is lost, then a read fails on the expired context", false, false, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupersedeFix(t)
			f.discardHold(t)
			id := f.recordAttempt(t, lateTip)
			f.handOnSlot(t, lateTip)
			svc := f.svc1
			unlock := func() {}
			switch {
			case tc.loseLock:
				unlock = f.holdLockLost(t)
			case tc.inList:
				lose := f.holdLockLost(t)
				svc.SetListRefTipsFn(func(ctx context.Context, o pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
					lose()
					<-ctx.Done()
					return f.forge.listRefTips(context.Background(), o, refs...)
				})
			default:
				svc.retentionHooks = &retentionTestHooks{}
			}
			var reached atomic.Int32
			svc.retentionHooks.beforeForgeWrite = func(rid uuid.UUID, op string) {
				if rid != f.oldRun || op != "attempt-delete" {
					return
				}
				reached.Add(1)
				unlock()
				if tc.pastDeadline {
					time.Sleep(opTimeout + 200*time.Millisecond) // the fence then runs on the expired context
				}
			}
			before := f.attemptRow(t, id)
			pass := &retentionPass{deadline: time.Now().Add(time.Minute), opTimeout: opTimeout}
			if _, err := svc.reconcilePublishAttempts(f.e.ctx, pgconv.UUID(f.oldRun), pass); err != nil {
				t.Fatalf("reconcilePublishAttempts: %v", err)
			}
			if n := reached.Load(); n != tc.wantFenceReached {
				t.Fatalf("attempt-delete fence reached %d times, want %d", n, tc.wantFenceReached)
			}
			if tip, ok := f.forge.ref(f.branchRef); !ok || tip != lateTip {
				t.Fatalf("branch ref = %q (present %v), want left at %s: the failed fence must stop the delete", tip, ok, lateTip)
			}
			a := f.attemptRow(t, id)
			if a.Checks != tc.wantCheck {
				t.Fatalf("attempt checks = %d, want %d", a.Checks, tc.wantCheck)
			}
			if tc.wantCheck == 0 && (!a.NextCheckAt.Time.Equal(before.NextCheckAt.Time) || a.LastError != before.LastError) {
				t.Fatalf("attempt = {next %v last_error %q}, want untouched {%v %q}",
					a.NextCheckAt.Time, a.LastError.String, before.NextCheckAt.Time, before.LastError.String)
			}
			if tc.wantCheck == 1 && (!a.NextCheckAt.Time.After(time.Now()) || !strings.Contains(a.LastError.String, "operation timed out")) {
				t.Fatalf("attempt = {next %v last_error %q}, want deferred into the future with the timeout recorded",
					a.NextCheckAt.Time, a.LastError.String)
			}
		})
	}
}
