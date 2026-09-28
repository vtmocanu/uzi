package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_publish_adopt_livedb_test.go pins PRD #1810's same-tip adoption rule against a REAL
// Postgres: a publish whose declared tip origin's checkpoint ref ALREADY holds writes nothing
// (pushbroker's already-current short-circuit), so it proves origin has the tip, not that this run
// wrote it. It counts as this run's publish only when the tip is the run's own persisted
// checkpoint_tip; otherwise it is the benign not_descendant skip and nothing is persisted. Without
// the rule a new run could "publish" another run's tip as a no-op, and that run's settle or the
// attempts arm then CAS-deletes the ref (the SHA matches) while the new run records it as its
// durable checkpoint.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// publishNewTip publishes tip as the NEW (live) run's worker on svc.
func (f *supersedeFix) publishNewTip(t *testing.T, svc *Service, tip string) PublishResult {
	t.Helper()
	res, err := svc.Publish(f.e.ctx, store.Worker{ID: f.newWorker, UserID: f.e.userID}, f.newRun, tip, []byte("pack"))
	if err != nil {
		t.Errorf("new run's Publish: %v", err)
	}
	return res
}

// runTip reads runID's persisted checkpoint_tip.
func (f *supersedeFix) runTip(t *testing.T, runID uuid.UUID) pgtype.Text {
	t.Helper()
	tip, err := f.e.q.GetRunCheckpointTipForRetention(f.e.ctx, runID)
	if err != nil {
		t.Fatalf("GetRunCheckpointTipForRetention: %v", err)
	}
	return tip
}

// settlingOld moves the old run's record to settling at the branch ref (custody released, its CAS
// delete pending in backoff), the one record state claimCheckpointSlot does not drive.
func (f *supersedeFix) settlingOld(t *testing.T) {
	t.Helper()
	f.discardHold(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'settling', next_attempt_at = now() + interval '1 hour'
	             WHERE run_id = $1`, f.oldRun)
}

// assertSkippedUnclaimed: the new run's publish was the benign skip and it claims no tip.
func (f *supersedeFix) assertSkippedUnclaimed(t *testing.T, res PublishResult) {
	t.Helper()
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("new run's publish = %+v, want the not_descendant skip (origin already held another run's tip)", res)
	}
	if tip := f.runTip(t, f.newRun); tip.Valid {
		t.Fatalf("new run persisted checkpoint_tip %q, a tip it never wrote", tip.String)
	}
}

// TestNoOpPublishNeverAdoptsSettlingRefLiveDB: the old run's record is settling at the branch ref
// (its delete in backoff). A new run whose clone tip equals that tip (both seeded from the pushed
// branch head) publishes it: a no-op on origin. It must not count as the new run's publish, so the
// old run's settle deleting the ref leaves the new run claiming nothing origin lacks.
func TestNoOpPublishNeverAdoptsSettlingRefLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.settlingOld(t)

	f.assertSkippedUnclaimed(t, f.publishNewTip(t, f.svc2, retentionTestTip))
	if n := f.attemptCount(t, f.newRun); n != 0 {
		t.Fatalf("new run's attempt rows = %d, want 0 (the no-op push wrote nothing)", n)
	}

	f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() WHERE run_id = $1`, f.oldRun)
	f.reconcileRun(t, f.svc1, f.oldRun)
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatal("setup: the old run's settle did not delete the branch ref")
	}
	if tip := f.runTip(t, f.newRun); tip.Valid && tip.String == retentionTestTip {
		t.Fatal("new run claims a checkpoint origin no longer holds")
	}
}

// TestNoOpPublishRacingAttemptDeleteLiveDB (buddy review B1): the old run's slot was handed on and
// its late push left the branch ref at lateTip. The attempts arm checks that no other run claims
// lateTip, then (paused at its delete) the new run publishes lateTip: a no-op on origin. The arm's
// CAS delete then matches the SHA and removes the ref. The new run must not have recorded it.
func TestNoOpPublishRacingAttemptDeleteLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.handOnSlot(t, lateTip)
	f.discardHold(t)
	f.recordAttempt(t, lateTip)

	var res PublishResult
	fired := false
	f.svc1.retentionHooks = &retentionTestHooks{beforeForgeWrite: func(_ uuid.UUID, op string) {
		if op != "attempt-delete" || fired {
			return
		}
		fired = true
		res = f.publishNewTip(t, f.svc2, lateTip)
	}}
	f.reconcileAttempts(t, f.svc1)
	if !fired {
		t.Fatal("setup: the attempts arm never reached its delete")
	}
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatal("setup: the attempts arm did not delete the late branch ref")
	}
	f.assertSkippedUnclaimed(t, res)
}

// TestNoOpPublishOfOwnPersistedTipSucceedsLiveDB (PRD #1030 M1): a run resumed with no new commits
// re-declares the tip it already published and persisted. The no-op is still a success.
func TestNoOpPublishOfOwnPersistedTipSucceedsLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.discardHold(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'deleted', settled_at = now() WHERE run_id = $1`, f.oldRun)
	f.forge.set(f.branchRef, supersedeNewTip)
	f.e.exec(t, `UPDATE runs SET checkpoint_tip = $2, checkpoint_tip_at = now() WHERE id = $1`, f.newRun, supersedeNewTip)

	res := f.publishNewTip(t, f.svc1, supersedeNewTip)
	if !res.Published {
		t.Fatalf("re-declare of the run's own persisted tip = %+v, want published", res)
	}
	if tip := f.runTip(t, f.newRun); !tip.Valid || tip.String != supersedeNewTip {
		t.Fatalf("new run's checkpoint_tip = %v, want still %s", tip, supersedeNewTip)
	}
	if n := f.attemptCount(t, f.newRun); n != 0 {
		t.Fatalf("new run's attempt rows = %d, want 0", n)
	}
}

// TestNoOpPublishAfterUnlandedAttemptSkipsLiveDB: an earlier attempt row of the new run at the same
// tip is NOT proof it wrote that tip (the row is written before the forge call, and that push may
// never have landed). With another run's ref already at the tip, the re-declare is still the skip,
// and the earlier row is kept for the sweeper.
func TestNoOpPublishAfterUnlandedAttemptSkipsLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.settlingOld(t)
	earlier, err := f.svc1.recordPublishAttempt(f.e.ctx, f.newRun, f.branch, f.branchRef, retentionTestTip)
	if err != nil || earlier == uuid.Nil {
		t.Fatalf("recordPublishAttempt: id %v err %v", earlier, err)
	}

	f.assertSkippedUnclaimed(t, f.publishNewTip(t, f.svc1, retentionTestTip))
	if n := f.attemptCount(t, f.newRun); n != 1 {
		t.Fatalf("new run's attempt rows = %d, want only the earlier one", n)
	}
	if _, err := f.e.q.GetCheckpointPublishAttempt(f.e.ctx, earlier); err != nil {
		t.Fatalf("earlier attempt row: %v, want kept", err)
	}
}
