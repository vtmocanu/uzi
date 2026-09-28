package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_publish_attempt_review_livedb_test.go pins the review fixes of PRD #1810 D2 residual 2
// against a REAL Postgres: the attempts arm never moves a run's tip backwards over a concurrent
// newer publish, never deletes a branch ref while custody is open, another run's tip, or one a
// running supersession owns; every row it cannot resolve is pushed out; Publish keeps an untracked
// push's row and refuses a push whose row could not be written; the live budget covers the insert.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

const newerTip = "7777777777777777777777777777777777777777"

// reconcileAttempts runs the attempts arm alone on svc, confined to the old run.
func (f *supersedeFix) reconcileAttempts(t *testing.T, svc *Service) {
	t.Helper()
	if _, err := svc.reconcilePublishAttempts(f.e.ctx, pgconv.UUID(f.oldRun)); err != nil {
		t.Fatalf("reconcilePublishAttempts: %v", err)
	}
}

// recordAttempt writes an outstanding attempt of the old run at tip, as pushOnce does.
func (f *supersedeFix) recordAttempt(t *testing.T, tip string) uuid.UUID {
	t.Helper()
	id, err := f.svc1.recordPublishAttempt(f.e.ctx, f.oldRun, f.branch, f.branchRef, tip)
	if err != nil || id == uuid.Nil {
		t.Fatalf("recordPublishAttempt: id %v err %v", id, err)
	}
	return id
}

func (f *supersedeFix) attemptRow(t *testing.T, id uuid.UUID) store.CheckpointPublishAttempt {
	t.Helper()
	a, err := f.e.q.GetCheckpointPublishAttempt(f.e.ctx, id)
	if err != nil {
		t.Fatalf("GetCheckpointPublishAttempt: %v", err)
	}
	return a
}

// assertDeferred: the row is kept, one comparison later, its next one in the future.
func (f *supersedeFix) assertDeferred(t *testing.T, id uuid.UUID, note string) {
	t.Helper()
	a := f.attemptRow(t, id)
	if a.Checks != 1 || !a.NextCheckAt.Time.After(time.Now()) {
		t.Fatalf("attempt = {checks %d next %v}, want deferred once into the future", a.Checks, a.NextCheckAt.Time)
	}
	if note != "" && (!a.LastError.Valid || !strings.Contains(a.LastError.String, note)) {
		t.Fatalf("attempt last_error = %v, want it to mention %q", a.LastError, note)
	}
}

// handOnSlot makes the old run's record superseded (its recovery ref at the retained tip) and puts
// the branch ref at branchTip, as a late push of the run (or another run's push) left it.
func (f *supersedeFix) handOnSlot(t *testing.T, branchTip string) {
	t.Helper()
	f.forge.set(f.recoveryRef, retentionTestTip)
	f.forge.set(f.branchRef, branchTip)
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'superseded', recovery_ref = $2, ref = $2 WHERE run_id = $1`,
		f.oldRun, f.recoveryRef)
}

// TestReconcileNeverMovesTipBackwardsLiveDB (review B1): the old run's attempt at lateTip landed
// with an unknown outcome. While the attempts arm is listing origin (after it saw lateTip), a NEWER
// publish of the run, routed while the run was live and so without the retention lock, lands,
// persists and tracks newerTip. The arm's re-record is compare-and-set on what it read before the
// list, so it moves nothing: the record and runs.checkpoint_tip stay at the newer tip.
func TestReconcileNeverMovesTipBackwardsLiveDB(t *testing.T) {
	f, oldGate, oldOut := newInFlightFix(t, true)
	close(oldGate.release)
	if o := recvPublish(t, oldOut); o.err == nil {
		t.Fatalf("old run's timed-out publish = %+v, want an error", o.res)
	}
	if !f.forge.land(f.branchRef, oldGate.fetched, lateTip) {
		t.Fatal("setup: the late landing was refused")
	}
	var once sync.Once
	f.svc1.SetListRefTipsFn(func(ctx context.Context, o pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
		tips, err := f.forge.listRefTips(ctx, o, refs...)
		once.Do(func() {
			if !f.forge.land(f.branchRef, lateTip, newerTip) {
				t.Error("the newer publish did not land")
			}
			// What publishOutcome does after that push returned success.
			if _, err := f.e.q.SetRunCheckpointTip(f.e.ctx, store.SetRunCheckpointTipParams{
				CheckpointTip: pgtype.Text{String: newerTip, Valid: true}, ID: f.oldRun,
			}); err != nil {
				t.Errorf("persist the newer tip: %v", err)
			}
			if _, tracked := f.svc2.trackPublishedCheckpoint(f.e.ctx, f.oldRun, false, f.branch, f.branchRef, newerTip); !tracked {
				t.Error("the newer publish was not tracked")
			}
		})
		return tips, err
	})

	f.reconcileAttempts(t, f.svc1)
	if r := f.row(t); r.Tip != newerTip || r.State != retentionRetained {
		t.Fatalf("record = {state %q tip %s}, want retained at the NEWER tip (never moved backwards)", r.State, r.Tip)
	}
	if tip, err := f.e.q.GetRunCheckpointTipForRetention(f.e.ctx, f.oldRun); err != nil || tip.String != newerTip {
		t.Fatalf("runs.checkpoint_tip = %q (err %v), want the NEWER tip", tip.String, err)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != newerTip {
		t.Fatalf("branch ref = %q, want the newer tip", tip)
	}
	if n := f.attemptCount(t, f.oldRun); n != 1 {
		t.Fatalf("attempt rows = %d, want 1 (deferred and re-checked, never written over the newer publish)", n)
	}

	// The re-check sees origin past the attempted tip: nothing to do, the record stays.
	f.e.exec(t, `UPDATE checkpoint_publish_attempts SET next_check_at = now() WHERE run_id = $1`, f.oldRun)
	f.reconcileAttempts(t, f.svc1)
	if r := f.row(t); r.Tip != newerTip {
		t.Fatalf("record tip = %s after the re-check, want the newer tip", r.Tip)
	}
}

// TestPublishAttemptDeleteWaitsForCustodyLiveDB (review N4): the run's slot was handed on (its
// recovery ref holds the OLDER retained tip) and a late push of the run left the branch ref at the
// attempted tip, the newest copy of its work. While a custody hold is open the arm keeps that ref
// and defers the row; once custody releases it CAS-deletes the ref at exactly the attempted tip.
func TestPublishAttemptDeleteWaitsForCustodyLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.handOnSlot(t, lateTip)
	id := f.recordAttempt(t, lateTip)

	f.reconcileAttempts(t, f.svc1)
	if tip, _ := f.forge.ref(f.branchRef); tip != lateTip {
		t.Fatalf("branch ref = %q with custody open, want kept at the attempted tip", tip)
	}
	f.assertDeferred(t, id, "custody hold is open")

	f.discardHold(t)
	f.e.exec(t, `UPDATE checkpoint_publish_attempts SET next_check_at = now() WHERE id = $1`, id)
	f.reconcileAttempts(t, f.svc1)
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatal("branch ref at the attempted tip survived once custody released")
	}
	if n := f.attemptCount(t, f.oldRun); n != 0 {
		t.Fatalf("attempt rows = %d after the delete, want 0", n)
	}
	if tip, ok := f.forge.ref(f.recoveryRef); !ok || tip != retentionTestTip {
		t.Fatalf("recovery ref = %q (present %v), want untouched by the attempts arm", tip, ok)
	}
}

// TestPublishAttemptTipClaimedByAnotherRunLiveDB (review N2, mutation M1): the branch ref is at a
// tip the old run attempted but the NEW run persisted as its own. The arm drops the row and never
// deletes another run's tip, even with the old run's slot handed on and no custody open.
func TestPublishAttemptTipClaimedByAnotherRunLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.handOnSlot(t, supersedeNewTip)
	f.discardHold(t)
	f.e.exec(t, `UPDATE runs SET checkpoint_tip = $2, checkpoint_tip_at = now() WHERE id = $1`, f.newRun, supersedeNewTip)
	f.recordAttempt(t, supersedeNewTip)

	f.reconcileAttempts(t, f.svc1)
	if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
		t.Fatalf("branch ref = %q, want the new run's tip left alone", tip)
	}
	if n := f.attemptCount(t, f.oldRun); n != 0 {
		t.Fatalf("attempt rows = %d, want 0 (dropped: the tip is another run's)", n)
	}
}

// TestPublishAttemptDeferredWhileSupersedingLiveDB (review N2, mutation M6): a supersession of the
// run's record is under way (superseding, its recovery ref named). The arm leaves the branch ref
// at the attempted tip to that supersession (its post-delete list and stuck exit), even with no
// custody open, and defers the row.
func TestPublishAttemptDeferredWhileSupersedingLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.forge.set(f.branchRef, lateTip)
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'superseding', recovery_ref = $2 WHERE run_id = $1`, f.oldRun, f.recoveryRef)
	f.discardHold(t)
	id := f.recordAttempt(t, lateTip)

	f.reconcileAttempts(t, f.svc1)
	if tip, _ := f.forge.ref(f.branchRef); tip != lateTip {
		t.Fatalf("branch ref = %q, want left to the supersession at the attempted tip", tip)
	}
	f.assertDeferred(t, id, "")
	if r := f.row(t); r.State != retentionSuperseding {
		t.Fatalf("record = %q, want still superseding", r.State)
	}
}

// failingStore injects one failing method into an otherwise real Store.
type failingStore struct {
	Store
	getRun, recordAttempt, tipClaims bool
	recordDelay                      time.Duration
}

var errInjected = errors.New("injected store failure")

func (s failingStore) GetRunByID(ctx context.Context, id uuid.UUID) (store.Run, error) {
	if s.getRun {
		return store.Run{}, errInjected
	}
	return s.Store.GetRunByID(ctx, id)
}

func (s failingStore) RecordCheckpointPublishAttempt(ctx context.Context, arg store.RecordCheckpointPublishAttemptParams) (uuid.UUID, error) {
	if s.recordAttempt {
		return uuid.Nil, errInjected
	}
	if s.recordDelay > 0 {
		time.Sleep(s.recordDelay)
	}
	return s.Store.RecordCheckpointPublishAttempt(ctx, arg)
}

func (s failingStore) CheckpointTipClaimedByOtherRun(ctx context.Context, arg store.CheckpointTipClaimedByOtherRunParams) (bool, error) {
	if s.tipClaims {
		return false, errInjected
	}
	return s.Store.CheckpointTipClaimedByOtherRun(ctx, arg)
}

// TestPublishOutcomeKeepsUntrackedAttemptLiveDB (review N1, N2 mutation M3): a push that returned
// success has its attempt row removed only when its tip is tracked. A terminal run's publish that
// no record tracks (its slot was handed on), or whose run status cannot be re-read, keeps the row.
func TestPublishOutcomeKeepsUntrackedAttemptLiveDB(t *testing.T) {
	outcome := func(t *testing.T, f *supersedeFix) {
		t.Helper()
		id := f.recordAttempt(t, lateTip)
		push := &checkpointPush{s: f.svc1, runID: f.oldRun, branch: f.branch, ref: f.branchRef, landed: id}
		res, _, err := f.svc1.publishOutcome(f.e.ctx, push, false, lateTip, nil)
		if err != nil || !res.Published {
			t.Fatalf("publishOutcome = %+v (err %v), want published", res, err)
		}
	}
	t.Run("tracked: cleared", func(t *testing.T) {
		f := newSupersedeFix(t)
		outcome(t, f)
		if n := f.attemptCount(t, f.oldRun); n != 0 {
			t.Fatalf("attempt rows = %d after a tracked publish, want 0", n)
		}
	})
	t.Run("untracked, slot handed on: kept", func(t *testing.T) {
		f := newSupersedeFix(t)
		f.handOnSlot(t, retentionTestTip)
		outcome(t, f)
		if n := f.attemptCount(t, f.oldRun); n != 1 {
			t.Fatalf("attempt rows = %d after an untracked publish, want 1 (kept for the sweeper)", n)
		}
	})
	t.Run("untracked, run status unreadable: kept", func(t *testing.T) {
		f := newSupersedeFix(t)
		f.handOnSlot(t, retentionTestTip)
		f.svc1.q = failingStore{Store: f.svc1.q, getRun: true}
		outcome(t, f)
		if n := f.attemptCount(t, f.oldRun); n != 1 {
			t.Fatalf("attempt rows = %d when the run could not be re-read, want 1 (treated as untracked)", n)
		}
	})
}

// TestPublishRefusedWhenAttemptNotRecordedLiveDB (review N2, mutation M4): a push whose attempt
// row cannot be written is never sent: the benign not_descendant skip, no forge publish.
func TestPublishRefusedWhenAttemptNotRecordedLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.svc2.q = failingStore{Store: f.svc2.q, recordAttempt: true}
	res := f.publishNew(t, f.svc2)
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("publish with no attempt row = %+v, want the not_descendant skip", res)
	}
	if publishes, _ := f.forge.calls(); publishes != 0 {
		t.Fatalf("forge publishes = %d, want 0 (no push without its attempt row)", publishes)
	}
}

// TestPublishAttemptBookkeepingOnFailureLiveDB (review N5): a row the arm cannot resolve has its
// next comparison pushed out whatever stopped it, so it never pins the head of the bounded page;
// a run still inside its cooling period is not listed at all.
func TestPublishAttemptBookkeepingOnFailureLiveDB(t *testing.T) {
	t.Run("a read error", func(t *testing.T) {
		f := newSupersedeFix(t)
		f.forge.set(f.branchRef, lateTip)
		id := f.recordAttempt(t, lateTip)
		f.svc1.q = failingStore{Store: f.svc1.q, tipClaims: true}
		f.reconcileAttempts(t, f.svc1)
		f.assertDeferred(t, id, "tip claims")
	})
	t.Run("the retention lock held elsewhere", func(t *testing.T) {
		f := newSupersedeFix(t)
		id := f.recordAttempt(t, lateTip)
		conn, err := f.e.pool.Acquire(f.e.ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		lockArgs := []any{store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(f.oldRun)}
		if _, err := conn.Exec(f.e.ctx, "SELECT pg_advisory_lock($1, $2)", lockArgs...); err != nil {
			t.Fatalf("take the retention lock: %v", err)
		}
		defer func() {
			if _, err := conn.Exec(f.e.ctx, "SELECT pg_advisory_unlock($1, $2)", lockArgs...); err != nil {
				t.Errorf("release the retention lock: %v", err)
			}
		}()
		f.reconcileAttempts(t, f.svc1)
		f.assertDeferred(t, id, "retention lock busy")
	})
	t.Run("inside the cooling period: not listed", func(t *testing.T) {
		f := newSupersedeFix(t)
		id := f.recordAttempt(t, lateTip)
		f.e.exec(t, `UPDATE runs SET status_since = now() WHERE id = $1`, f.oldRun)
		f.svc1.checkpointSupersessionCooling = time.Hour
		f.reconcileAttempts(t, f.svc1)
		if a := f.attemptRow(t, id); a.Checks != 0 {
			t.Fatalf("attempt checks = %d inside the cooling period, want 0 (not listed)", a.Checks)
		}
	})
}

// TestLivePublishBudgetCoversAttemptInsertLiveDB (review N7): the live pre-push budget is checked
// after the attempt insert, immediately before the forge call, so a slow insert is inside it: the
// push is refused and its row removed.
func TestLivePublishBudgetCoversAttemptInsertLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.svc2.livePublishPrePushBudget = time.Second
	f.svc2.q = failingStore{Store: f.svc2.q, recordDelay: 2 * time.Second}
	res := f.publishNew(t, f.svc2)
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("publish whose insert outlasted its budget = %+v, want the not_descendant skip", res)
	}
	if publishes, _ := f.forge.calls(); publishes != 0 {
		t.Fatalf("forge publishes = %d, want 0 (no push sent past the budget)", publishes)
	}
	if n := f.attemptCount(t, f.newRun); n != 0 {
		t.Fatalf("new run attempt rows = %d, want 0 (the refused push's row removed)", n)
	}
}
