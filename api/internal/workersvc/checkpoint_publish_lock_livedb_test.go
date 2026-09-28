package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_publish_lock_livedb_test.go pins the PRD #1810 publish-path branches the rest of the
// retention suite does not reach: claimCheckpointSlot's backoff arm, and publishTerminalLocked's
// busy-lock retry (still busy after the budget, released inside it, ctx done while waiting) and
// its fence-lost arm.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// publishOldCtx publishes tip as the OLD (terminal) run's worker on ctx.
func (f *supersedeFix) publishOldCtx(t *testing.T, ctx context.Context, tip string) PublishResult {
	t.Helper()
	res, err := f.svc1.Publish(ctx, store.Worker{ID: f.oldWorker(t), UserID: f.e.userID}, f.oldRun, tip, []byte("pack"))
	if err != nil {
		t.Fatalf("old run's Publish: %v", err)
	}
	return res
}

// assertOldRetainedUntouched: no forge call was made and the old run's record and branch ref are
// exactly as the fixture left them.
func (f *supersedeFix) assertOldRetainedUntouched(t *testing.T) {
	t.Helper()
	if pubs, creates := f.forge.calls(); pubs != 0 || creates != 0 {
		t.Fatalf("publish calls = %d, create calls = %d; want 0 and 0", pubs, creates)
	}
	f.assertBranchRetained(t)
	if r := f.row(t); r.State != retentionRetained || r.Tip != retentionTestTip {
		t.Fatalf("record = {state %q tip %q}, want retained at %s", r.State, r.Tip, retentionTestTip)
	}
}

// TestClaimSlotBackoffRefusesWithoutSupersessionLiveDB: a LIVE run's publish meets another run's
// retained record still inside its retry backoff (next_attempt_at in the future). The publish is
// the not_descendant skip, no supersession is attempted and nothing is pushed: the sweeper owns
// that record's retries.
func TestClaimSlotBackoffRefusesWithoutSupersessionLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() + interval '1 hour' WHERE run_id = $1`, f.oldRun)
	res := f.publishNew(t, f.svc1)
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("new run's Publish = %+v, want the not_descendant skip (the record is backing off)", res)
	}
	f.assertOldRetainedUntouched(t)
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("a recovery ref was created for a record in its backoff")
	}
}

// shrinkTerminalLockRetry makes svc's terminal-publish lock retry fast for a test.
func shrinkTerminalLockRetry(svc *Service, budget, interval time.Duration) {
	svc.terminalLockRetryBudget = budget
	svc.terminalLockRetryInterval = interval
}

// TestTerminalPublishLockBusyPastBudgetLiveDB: another session holds the terminal run's
// retention lock for longer than the retry budget. The publish is the not_descendant skip with
// no forge call, after retrying for about the budget.
func TestTerminalPublishLockBusyPastBudgetLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	shrinkTerminalLockRetry(f.svc1, 400*time.Millisecond, 50*time.Millisecond)
	release := f.rf.lockFromOtherSession(t, f.oldRun)
	defer release()
	start := time.Now()
	res := f.publishOldCtx(t, f.e.ctx, reviewLateTip)
	elapsed := time.Since(start)
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("old run's Publish = %+v, want the not_descendant skip (lock busy past the budget)", res)
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("Publish returned after %v, want it to retry for about the 400ms budget", elapsed)
	}
	f.assertOldRetainedUntouched(t)
}

// TestTerminalPublishLockReleasedWithinBudgetLiveDB: the lock is busy when the terminal publish
// starts and released inside the retry window, so the retry acquires it and the publish lands
// (the run's last checkpoint is not lost to brief contention).
func TestTerminalPublishLockReleasedWithinBudgetLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	shrinkTerminalLockRetry(f.svc1, 20*time.Second, 50*time.Millisecond)
	f.forge.mu.Lock()
	f.forge.descends[reviewLateTip] = retentionTestTip
	f.forge.mu.Unlock()
	release := f.rf.lockFromOtherSession(t, f.oldRun)
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(300 * time.Millisecond)
		release()
	}()
	res := f.publishOldCtx(t, f.e.ctx, reviewLateTip)
	<-released
	if !res.Published {
		t.Fatalf("old run's Publish = %+v, want published once the lock was released", res)
	}
	if pubs, _ := f.forge.calls(); pubs != 1 {
		t.Fatalf("publish calls = %d, want exactly 1 (only the lock is retried, never the push)", pubs)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != reviewLateTip {
		t.Fatalf("branch ref = %q, want the late tip %s", tip, reviewLateTip)
	}
	if r := f.row(t); r.State != retentionRetained || r.Tip != reviewLateTip {
		t.Fatalf("record = {state %q tip %q}, want retained at the late tip", r.State, r.Tip)
	}
}

// TestTerminalPublishLockRetryHonoursCtxLiveDB: with the lock held and a long budget, a caller
// ctx that ends while waiting returns the not_descendant skip promptly, with no forge call.
func TestTerminalPublishLockRetryHonoursCtxLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	shrinkTerminalLockRetry(f.svc1, time.Minute, 50*time.Millisecond)
	release := f.rf.lockFromOtherSession(t, f.oldRun)
	defer release()
	ctx, cancel := context.WithTimeout(f.e.ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	res := f.publishOldCtx(t, ctx, reviewLateTip)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Publish returned after %v, want it to stop when ctx ended (budget 1m)", elapsed)
	}
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("old run's Publish = %+v, want the not_descendant skip (ctx done while retrying)", res)
	}
	f.assertOldRetainedUntouched(t)
}

// TestTerminalPublishFenceLostSkipsPushLiveDB: the terminal publish takes the lock, but the
// pinned session loses it before the push (released here without ending the session). The fence
// before the single push fails, so the publish is the not_descendant skip with no forge call.
func TestTerminalPublishFenceLostSkipsPushLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.forge.mu.Lock()
	f.forge.descends[reviewLateTip] = retentionTestTip
	f.forge.mu.Unlock()
	f.svc1.retentionHooks = &retentionTestHooks{afterLock: func(id uuid.UUID, c *pgxpool.Conn) {
		if id != f.oldRun {
			return
		}
		var ok bool
		if err := c.QueryRow(f.e.ctx, "SELECT pg_advisory_unlock($1, $2)",
			store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(id)).Scan(&ok); err != nil || !ok {
			t.Errorf("unlock the pinned session: ok=%v err=%v", ok, err)
		}
	}}
	res := f.publishOldCtx(t, f.e.ctx, reviewLateTip)
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("old run's Publish = %+v, want the not_descendant skip (lock lost before the push)", res)
	}
	f.assertOldRetainedUntouched(t)
}
