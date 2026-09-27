package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_settlement_review_livedb_test.go pins the PRD #1810 M4 review fixes against a REAL
// Postgres: the custody-aware post-settlement audit (a recovery ref found at the tip while a hold
// is open is reopened, never deleted), a terminal run's late publish tracked by its record (and
// refused once its slot was handed to a newer run), the stuck-supersession exit's own-tip branch
// delete, and the backfill watermark's overlap window.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

const reviewLateTip = "6666666666666666666666666666666666666666"

// publishOld publishes tip as the OLD (terminal) run's worker.
func (f *supersedeFix) publishOld(t *testing.T, tip string) PublishResult {
	t.Helper()
	res, err := f.svc1.Publish(f.e.ctx, store.Worker{ID: f.oldWorker(t), UserID: f.e.userID}, f.oldRun, tip, []byte("pack"))
	if err != nil {
		t.Fatalf("old run's Publish: %v", err)
	}
	return res
}

// makeAuditDue moves the old run's verify_after into the past.
func (f *supersedeFix) makeAuditDue(t *testing.T) {
	t.Helper()
	f.e.exec(t, `UPDATE checkpoint_retentions SET verify_after = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
}

// --- FIX 1: the audit never deletes the only copy of a tip while custody is open ----------------

// TestAuditKeepsRecoveryRefWhileCustodyOpenLiveDB is the reviewer's interleaving: instance 1's
// recovery-ref create passed its fence and lost its session; instance 2's re-drive lists neither
// ref and closes the record as tip-gone WITH THE HOLD OPEN; instance 1's late create lands. The
// audit must not delete that ref (the only copy of the tip custody protects): it reopens the record
// as superseded, and ordinary settlement deletes the ref once the hold is discarded.
func TestAuditKeepsRecoveryRefWhileCustodyOpenLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'superseding', recovery_ref = $2 WHERE run_id = $1`, f.oldRun, f.recoveryRef)
	f.forge.mu.Lock()
	delete(f.forge.refs, f.branchRef)
	f.forge.mu.Unlock()
	if _, _, err := f.svc2.supersedeRetainedCheckpoint(f.e.ctx, f.oldRun); err != nil {
		t.Fatalf("supersedeRetainedCheckpoint: %v", err)
	}
	r := f.row(t)
	if r.State != "deleted" || !r.VerifyAfter.Valid || r.VerifiedAt.Valid {
		t.Fatalf("record after the tip-gone close = {state %q verify_after %v verified %v}, want deleted and unverified", r.State, r.VerifyAfter, r.VerifiedAt)
	}
	if s := mhHoldState(t, f.e, f.oldHold); s != "open" {
		t.Fatalf("hold = %q, want still open", s)
	}

	// Instance 1's late create lands at the recorded tip; the audit comes due.
	f.forge.set(f.recoveryRef, retentionTestTip)
	f.makeAuditDue(t)
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 1 {
		t.Fatalf("audit pass progressed %d, want 1 (the reopen)", n)
	}
	if tip, ok := f.forge.ref(f.recoveryRef); !ok || tip != retentionTestTip {
		t.Fatalf("recovery ref = %q (present %v), want kept at the tip while custody is open", tip, ok)
	}
	if _, del := f.forge.counts(f.recoveryRef); del != 0 {
		t.Fatalf("recovery ref deletes = %d, want 0 while custody is open", del)
	}
	r = f.row(t)
	if r.State != retentionSuperseded || r.Ref != f.recoveryRef || r.Tip != retentionTestTip ||
		r.SettledAt.Valid || r.VerifyAfter.Valid || r.VerifiedAt.Valid {
		t.Fatalf("record = {state %q ref %q tip %q settled %v verify_after %v verified %v}, want reopened superseded at the recovery ref",
			r.State, r.Ref, r.Tip, r.SettledAt, r.VerifyAfter, r.VerifiedAt)
	}
	// A second pass with the hold still open changes nothing.
	f.reconcileRun(t, f.svc2, f.oldRun)
	if _, ok := f.forge.ref(f.recoveryRef); !ok {
		t.Fatalf("recovery ref deleted by a later pass with the hold still open")
	}

	// The hold is discarded through its real writer: the settle trigger deletes the ref.
	if ok, err := f.recoverySvc().DiscardHold(f.e.ctx, f.e.userID, f.oldRun, f.oldHold); err != nil || !ok {
		t.Fatalf("DiscardHold = %v, %v", ok, err)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("recovery ref left on origin after the last hold was discarded")
	}
	if r := f.row(t); r.State != "deleted" || r.Ref != f.recoveryRef {
		t.Fatalf("record = {state %q ref %q}, want deleted at the recovery ref", r.State, r.Ref)
	}
}

// --- FIX 2: a terminal run's late publish is tracked, or refused once superseded -----------------

// TestTerminalLatePublishAfterSettleReopensLiveDB: the old run's record settled (branch ref
// deleted), then a worker still bound to the run publishes again, re-creating the branch ref. The
// record is reopened settling at the new tip and settled again, so the ref does not outlive it and
// a new run on the branch publishes without a supersession.
func TestTerminalLatePublishAfterSettleReopensLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.discardHold(t)
	f.svc1.settleRetainedCheckpoint(f.e.ctx, f.oldRun)
	f.assertBranchSettled(t)

	if res := f.publishOld(t, reviewLateTip); !res.Published {
		t.Fatalf("old run's late Publish = %+v, want published (its latest work)", res)
	}
	r := f.row(t)
	if r.State != "deleted" || r.Tip != reviewLateTip || r.Ref != f.branchRef || !r.SettledAt.Valid {
		t.Fatalf("record = {state %q tip %q ref %q settled %v}, want reopened and settled again at the late tip",
			r.State, r.Tip, r.Ref, r.SettledAt)
	}
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatalf("the late publish's branch ref outlived its settled record")
	}
	if _, del := f.forge.counts(f.branchRef); del != 2 {
		t.Fatalf("branch ref deletes = %d, want 2 (the settle and the re-settle)", del)
	}
	if res := f.publishNew(t, f.svc2); !res.Published {
		t.Fatalf("new run's Publish = %+v, want published (not blocked)", res)
	}
	if _, creates := f.forge.calls(); creates != 0 {
		t.Fatalf("create calls = %d, want 0 (no supersession needed)", creates)
	}
}

// TestTerminalLatePublishHeldLiveDB: with the hold open, a terminal run's late publish leaves its
// record retained at the new tip, whether the record was missing or had been closed.
func TestTerminalLatePublishHeldLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, setup string }{
		{"no record", `DELETE FROM checkpoint_retentions WHERE run_id = $1`},
		{"deleted record", `UPDATE checkpoint_retentions SET state = 'deleted', settled_at = now() WHERE run_id = $1`},
		{"abandoned record", `UPDATE checkpoint_retentions SET state = 'abandoned', settled_at = now() WHERE run_id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupersedeFix(t)
			f.e.exec(t, tc.setup, f.oldRun)
			f.forge.mu.Lock()
			f.forge.descends[reviewLateTip] = retentionTestTip
			f.forge.mu.Unlock()
			if res := f.publishOld(t, reviewLateTip); !res.Published {
				t.Fatalf("old run's late Publish = %+v, want published", res)
			}
			r := f.row(t)
			if r.State != retentionRetained || r.Tip != reviewLateTip || r.Ref != f.branchRef || r.SettledAt.Valid {
				t.Fatalf("record = {state %q tip %q ref %q settled %v}, want retained at the late tip", r.State, r.Tip, r.Ref, r.SettledAt)
			}
			if tip, _ := f.forge.ref(f.branchRef); tip != reviewLateTip {
				t.Fatalf("branch ref = %q, want retained at the late tip", tip)
			}
		})
	}
}

// TestTerminalPublishRefusedOnceSupersededLiveDB: once the old run's branch slot was handed to a
// newer run (superseded, and later settled with its recovery ref), a publish by a worker still
// bound to the old run is a benign "superseded" skip with no forge call.
func TestTerminalPublishRefusedOnceSupersededLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	if res := f.publishNew(t, f.svc1); !res.Published {
		t.Fatalf("new run's Publish = %+v, want published after supersession", res)
	}
	f.assertSuperseded(t)
	check := func(label string) {
		t.Helper()
		before, _ := f.forge.calls()
		res := f.publishOld(t, reviewLateTip)
		if res.Published || res.Skipped != "superseded" {
			t.Fatalf("%s: old run's Publish = %+v, want the superseded skip", label, res)
		}
		if after, _ := f.forge.calls(); after != before {
			t.Fatalf("%s: publish calls %d -> %d, want no forge call", label, before, after)
		}
		if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
			t.Fatalf("%s: branch ref = %q, want the new run's tip", label, tip)
		}
	}
	check("superseded")

	f.discardHold(t)
	f.svc1.settleRetainedCheckpoint(f.e.ctx, f.oldRun)
	if r := f.row(t); r.State != "deleted" || r.Ref != f.recoveryRef {
		t.Fatalf("record = {state %q ref %q}, want deleted at the recovery ref", r.State, r.Ref)
	}
	check("settled after supersession")
}

// TestLiveRunPublishRetentionUnchangedLiveDB: a live run's publish never inserts or reopens a
// record; a retained record it carries (from an earlier terminal transition) only advances its tip.
func TestLiveRunPublishRetentionUnchangedLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	w := f.e.seedWorker(t, nil)
	publish := func(runID uuid.UUID) {
		t.Helper()
		f.forge.mu.Lock()
		f.forge.descends[reviewLateTip] = retentionTestTip
		f.forge.mu.Unlock()
		res, err := f.svc1.Publish(f.e.ctx, store.Worker{ID: w, UserID: f.e.userID}, runID, reviewLateTip, []byte("pack"))
		if err != nil || !res.Published {
			t.Fatalf("live Publish = %+v, %v; want published", res, err)
		}
	}

	noRecord, _ := f.seedIssueRun(t, "running", w, 1)
	publish(noRecord)
	if _, ok := f.record(t, noRecord); ok {
		t.Fatalf("a live run's publish inserted a retention record")
	}

	deleted, ref := f.seedIssueRun(t, "running", w, 1)
	f.insertRecord(t, deleted, ref, "deleted")
	publish(deleted)
	if r, _ := f.record(t, deleted); r.State != "deleted" || r.Tip != retentionTestTip {
		t.Fatalf("live run's deleted record = {state %q tip %q}, want untouched", r.State, r.Tip)
	}

	retained, ref := f.seedIssueRun(t, "running", w, 1)
	f.insertRecord(t, retained, ref, retentionRetained)
	publish(retained)
	if r, _ := f.record(t, retained); r.State != retentionRetained || r.Tip != reviewLateTip {
		t.Fatalf("live run's retained record = {state %q tip %q}, want retained at the new tip", r.State, r.Tip)
	}
}

// --- FIX 3: the stuck exit deletes a branch ref at the run's own latest tip ----------------------

// TestStuckSupersessionTipLagOwnPublishExitLiveDB: the branch ref holds a later tip that
// runs.checkpoint_tip proves is the old run's own publish; with the hold discarded the exit
// CAS-deletes it at that tip, so the new run on the branch is not blocked.
func TestStuckSupersessionTipLagOwnPublishExitLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	const t2 = "5555555555555555555555555555555555555555"
	f.forge.set(f.branchRef, t2)
	f.e.exec(t, `UPDATE runs SET checkpoint_tip = $2 WHERE id = $1`, f.oldRun, t2)
	f.stopSupersession(t)
	f.discardHold(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 1 {
		t.Fatalf("reconcile progressed %d, want 1", n)
	}
	if r := f.row(t); r.State != "deleted" {
		t.Fatalf("record = %q, want deleted", r.State)
	}
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatalf("branch ref at the run's own latest tip left on origin")
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("a recovery ref exists after the exit")
	}
	if res := f.publishNew(t, f.svc2); !res.Published {
		t.Fatalf("new run's Publish = %+v, want published (not blocked)", res)
	}
}

// --- FIX 4: the backfill watermark --------------------------------------------------------------

// TestBackfillWatermarkOverlapLiveDB: an advancing pass moves the persisted watermark up to now; a
// run whose transaction committed late (status_since inside the 10-minute overlap below the
// watermark) is still backfilled; one below the overlap is outside the scan. The watermark never
// moves back.
func TestBackfillWatermarkOverlapLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	// The meta row is a singleton shared with every other test on this database: restore it.
	var savedEnabled, savedThrough pgtype.Timestamptz
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT enabled_at, backfilled_through FROM checkpoint_retention_meta`).
		Scan(&savedEnabled, &savedThrough); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.e.pool.Exec(f.e.ctx, `UPDATE checkpoint_retention_meta SET enabled_at = $1, backfilled_through = $2`, savedEnabled, savedThrough)
	})
	watermark := func() time.Time {
		t.Helper()
		var w pgtype.Timestamptz
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT backfilled_through FROM checkpoint_retention_meta`).Scan(&w); err != nil || !w.Valid {
			t.Fatalf("read watermark: %v (valid %v)", err, w.Valid)
		}
		return w.Time
	}
	w := f.e.seedWorker(t, nil)

	first, firstRef := f.seedIssueRun(t, "failed", w, 1)
	start := time.Now()
	if _, err := f.svc2.backfillCheckpointRetentions(f.e.ctx, pgconv.UUID(first), true); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	f.assertRunSettledOrSettling(t, first, firstRef)
	wm := watermark()
	if wm.Before(start.Add(-time.Minute)) {
		t.Fatalf("watermark = %v, want advanced to about now (%v)", wm, start)
	}

	// enabled_at is lowered so it is the watermark, not enabled_at, that bounds the scan below.
	f.e.exec(t, `UPDATE checkpoint_retention_meta SET enabled_at = LEAST(enabled_at, $1)`, wm.Add(-time.Hour))
	late, _ := f.seedIssueRun(t, "failed", w, 1)
	f.e.exec(t, `UPDATE runs SET status_since = $2 WHERE id = $1`, late, wm.Add(-5*time.Minute))
	if _, err := f.svc2.backfillCheckpointRetentions(f.e.ctx, pgconv.UUID(late), false); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if _, ok := f.record(t, late); !ok {
		t.Fatalf("a run committed late inside the overlap window was not backfilled")
	}

	old, _ := f.seedIssueRun(t, "failed", w, 1)
	f.e.exec(t, `UPDATE runs SET status_since = $2 WHERE id = $1`, old, wm.Add(-20*time.Minute))
	if _, err := f.svc2.backfillCheckpointRetentions(f.e.ctx, pgconv.UUID(old), false); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if _, ok := f.record(t, old); ok {
		t.Fatalf("a run below the watermark's overlap was scanned")
	}

	if _, err := f.e.q.AdvanceCheckpointRetentionBackfillWatermark(f.e.ctx, pgconv.Time(wm.Add(-time.Hour))); err != nil {
		t.Fatalf("advance watermark: %v", err)
	}
	if got := watermark(); !got.Equal(wm) {
		t.Fatalf("watermark moved back: %v -> %v", wm, got)
	}
}

// assertRunSettledOrSettling: runID has a record at ref (the backfill recorded it).
func (f *supersedeFix) assertRunSettledOrSettling(t *testing.T, runID uuid.UUID, ref string) {
	t.Helper()
	if r, ok := f.record(t, runID); !ok || r.Ref != ref {
		t.Fatalf("record = %+v (present %v), want one at %s", r, ok, ref)
	}
}
