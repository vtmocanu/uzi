package workersvc

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// checkpoint_publish_cas_livedb_test.go pins the PUBLISH path's own tip persist and record track
// as compare-and-set on what the push observed immediately before its forge call (PRD #1810 D2,
// residual 2), against a REAL Postgres.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// TestStalledPublishWritesNeverMoveTipBackwardsLiveDB: an OLDER live-routed publish of the old run
// lands lateTip on origin, but its persist and track stall. Meanwhile the run turns terminal, a
// NEWER push lands newerTip over it (its outcome unknown to the api, so only its attempt row names
// it), the cooling period passes, and the sweeper's attempts arm re-records newerTip on the run's
// record and runs.checkpoint_tip and drops the newer attempt row. The stalled writes then arrive:
// they must move nothing (origin carries newerTip, which the record and the run must keep naming),
// and the older push counts as untracked, its attempt row kept for the arm, which finds the branch
// ref past it and defers it.
func TestStalledPublishWritesNeverMoveTipBackwardsLiveDB(t *testing.T) {
	var (
		entered = make(chan struct{})
		release = make(chan struct{})
		out     <-chan publishOut
	)
	f := newSupersedeFixWith(t, func(f *supersedeFix) {
		f.forge.descends[lateTip] = retentionTestTip
		f.svc1.SetPublishFn(func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
			res, err := f.forge.publish(ctx, o)
			if o.DeclaredTip == lateTip {
				// The push landed; the api's writes after it stall here.
				close(entered)
				<-release
			}
			return res, err
		})
		out = f.startOldRunPublish(t, lateTip)
		waitEntered(t, entered)
		f.e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, f.oldRun)
	})
	if tip, _ := f.forge.ref(f.branchRef); tip != lateTip {
		t.Fatalf("setup: branch ref = %q, want the older push landed at %s", tip, lateTip)
	}

	// The newer push lands over it with an unknown outcome: only its attempt row names it.
	if !f.forge.land(f.branchRef, lateTip, newerTip) {
		t.Fatal("setup: the newer push did not land")
	}
	f.recordAttempt(t, newerTip)
	f.coolOff(t)
	f.reconcileAttempts(t, f.svc2)
	if r := f.row(t); r.Tip != newerTip {
		t.Fatalf("setup: record tip = %s after the arm, want the newer tip re-recorded", r.Tip)
	}
	if tip, err := f.e.q.GetRunCheckpointTipForRetention(f.e.ctx, f.oldRun); err != nil || tip.String != newerTip {
		t.Fatalf("setup: runs.checkpoint_tip = %q (err %v), want the newer tip re-recorded", tip.String, err)
	}
	var newerRows int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id = $1 AND tip = $2`,
		f.oldRun, newerTip).Scan(&newerRows); err != nil || newerRows != 0 {
		t.Fatalf("setup: newer attempt rows = %d (err %v), want 0 (dropped by the arm)", newerRows, err)
	}

	// The stalled writes of the older publish arrive now.
	close(release)
	if o := recvPublish(t, out); o.err != nil || !o.res.Published {
		t.Fatalf("older publish = %+v (err %v), want published (its push landed)", o.res, o.err)
	}
	if tip, err := f.e.q.GetRunCheckpointTipForRetention(f.e.ctx, f.oldRun); err != nil || tip.String != newerTip {
		t.Fatalf("runs.checkpoint_tip = %q (err %v), want still the newer tip (never moved backwards)", tip.String, err)
	}
	if r := f.row(t); r.Tip != newerTip || r.State != retentionRetained {
		t.Fatalf("record = {state %q tip %s}, want retained at the newer tip (never moved backwards)", r.State, r.Tip)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != newerTip {
		t.Fatalf("branch ref = %q, want the newer tip", tip)
	}
	if n := f.attemptCount(t, f.oldRun); n != 1 {
		t.Fatalf("attempt rows = %d, want 1 (the older push is untracked: kept for the arm)", n)
	}

	// The arm finds the branch ref past the older tip: the row is deferred, the record untouched.
	f.reconcileAttempts(t, f.svc2)
	if n := f.attemptCount(t, f.oldRun); n != 1 {
		t.Fatalf("attempt rows = %d after the arm, want 1 (deferred until the horizon)", n)
	}
	if r := f.row(t); r.Tip != newerTip {
		t.Fatalf("record tip = %s after the arm, want the newer tip", r.Tip)
	}
}

// TestLiveRunTrackAdvanceIsCompareAndSetLiveDB: a LIVE run's publish advances the tip of a record
// that still names the branch ref (AdvanceCheckpointRetentionTip), compare-and-set on the record
// tip the push observed before its forge call: with nothing in between it advances; once another
// writer moved the record meanwhile it moves nothing (the record is never moved backwards).
func TestLiveRunTrackAdvanceIsCompareAndSetLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		moved   bool
		wantTip string
	}{
		{"nothing in between: advanced", false, lateTip},
		{"a newer tip tracked in between: not moved", true, newerTip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupersedeFix(t)
			// One active run per issue: retire the new run, then read the old one as live again.
			f.e.exec(t, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, f.newRun)
			f.e.exec(t, `UPDATE runs SET status = 'running' WHERE id = $1`, f.oldRun)
			push := &checkpointPush{s: f.svc1, runID: f.oldRun, branch: f.branch, ref: f.branchRef}
			if err := push.observePublishBase(f.e.ctx); err != nil {
				t.Fatalf("observePublishBase: %v", err)
			}
			if tc.moved {
				f.e.exec(t, `UPDATE checkpoint_retentions SET tip = $2 WHERE run_id = $1`, f.oldRun, newerTip)
			}
			if _, tracked := f.svc1.trackPublishedCheckpoint(f.e.ctx, push, false, lateTip); !tracked {
				t.Fatal("a live run's track reported untracked")
			}
			if r := f.row(t); r.Tip != tc.wantTip || r.State != retentionRetained {
				t.Fatalf("record = {state %q tip %s}, want retained at %s", r.State, r.Tip, tc.wantTip)
			}
		})
	}
}

// TestLivePublishCreateAfterSettleReopensRecordLiveDB: the old run's live-routed push reads its
// base (no record yet, runs.checkpoint_tip at the retained tip) and is held inside its forge call.
// The run turns terminal with no custody hold, so the terminal transaction records the tip
// settling, and the settle deletes the branch ref. The push then lands as a CREATE onto the ABSENT
// branch ref. Its track matches the record the terminal transaction inserted from the
// runs.checkpoint_tip the push observed (the expected_run_tip arm), reopens it settling, and the
// settle it owes deletes the landed ref: no branch ref is left untracked, and the push's attempt
// row is cleared.
func TestLivePublishCreateAfterSettleReopensRecordLiveDB(t *testing.T) {
	var (
		gate *publishGate
		out  <-chan publishOut
	)
	f := newSupersedeFixWith(t, func(f *supersedeFix) {
		f.discardHold(t)
		f.forge.descends[lateTip] = retentionTestTip
		gate = gatePublish(f.svc1, f.forge, lateTip, false)
		out = f.startOldRunPublish(t, lateTip)
		waitEntered(t, gate.entered)
		f.e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, f.oldRun)
	})
	if r := f.row(t); r.State != retentionSettling || r.Tip != retentionTestTip {
		t.Fatalf("setup: record = {state %q tip %s}, want settling at the old tip", r.State, r.Tip)
	}
	f.reconcileRun(t, f.svc2, f.oldRun)
	if r := f.row(t); r.State != "deleted" {
		t.Fatalf("setup: record = %q after the settle, want deleted", r.State)
	}
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatal("setup: the settle did not delete the branch ref")
	}

	close(gate.release)
	if o := recvPublish(t, out); o.err != nil || !o.res.Published {
		t.Fatalf("old run's publish = %+v (err %v), want published (a create onto the absent ref)", o.res, o.err)
	}
	if r := f.row(t); r.State != "deleted" || r.Tip != lateTip {
		t.Fatalf("record = {state %q tip %s}, want reopened at the landed tip and settled again", r.State, r.Tip)
	}
	if tip, ok := f.forge.ref(f.branchRef); ok {
		t.Fatalf("branch ref = %q, want deleted by the reopened record's settle (never left untracked)", tip)
	}
	if n := f.attemptCount(t, f.oldRun); n != 0 {
		t.Fatalf("attempt rows = %d, want 0 (the landed tip was persisted and tracked)", n)
	}
}

// TestPersistPublishedTipIsIdempotentLiveDB (#1810 rework review nit 1): a push's tip persist is
// compare-and-set on the runs.checkpoint_tip it observed, and ALSO matches a row already at the
// tip it persists (a retry of the same push, or the attempts arm, recorded it meanwhile): that
// write reports persisted, so the push is tracked instead of logging a moved tip and keeping a
// redundant attempt row. A DIFFERENT tip persisted meanwhile still moves nothing (never
// backwards).
func TestPersistPublishedTipIsIdempotentLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	push := &checkpointPush{s: f.svc1, runID: f.oldRun, branch: f.branch, ref: f.branchRef}
	if err := push.observePublishBase(f.e.ctx); err != nil {
		t.Fatalf("observePublishBase: %v", err)
	}
	if !push.base.runTip.Valid || push.base.runTip.String != retentionTestTip {
		t.Fatalf("setup: observed run tip = %v, want %s", push.base.runTip, retentionTestTip)
	}
	// The same tip lands on runs.checkpoint_tip after the push observed its base.
	f.e.exec(t, `UPDATE runs SET checkpoint_tip = $2 WHERE id = $1`, f.oldRun, lateTip)

	if !f.svc1.persistPublishedTip(f.e.ctx, push, lateTip) {
		t.Fatal("persisting the tip the run already carries reported not persisted; want the idempotent match")
	}
	if tip, err := f.e.q.GetRunCheckpointTipForRetention(f.e.ctx, f.oldRun); err != nil || tip.String != lateTip {
		t.Fatalf("runs.checkpoint_tip = %q (err %v), want %s", tip.String, err, lateTip)
	}
	// Control: another tip, on the same stale base, moves nothing.
	if f.svc1.persistPublishedTip(f.e.ctx, push, newerTip) {
		t.Fatal("persisting a different tip over a moved runs.checkpoint_tip reported persisted; want compare-and-set to refuse")
	}
	if tip, err := f.e.q.GetRunCheckpointTipForRetention(f.e.ctx, f.oldRun); err != nil || tip.String != lateTip {
		t.Fatalf("runs.checkpoint_tip = %q (err %v), want still %s", tip.String, err, lateTip)
	}
}
