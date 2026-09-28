package workersvc

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

	dbNow := func() time.Time {
		t.Helper()
		var n time.Time
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT now()`).Scan(&n); err != nil {
			t.Fatalf("read db clock: %v", err)
		}
		return n
	}

	first, firstRef := f.seedIssueRun(t, "failed", w, 1)
	before := dbNow()
	if _, err := f.svc2.backfillCheckpointRetentions(f.e.ctx, pgconv.UUID(first), true, nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	after := dbNow()
	f.assertRunSettledOrSettling(t, first, firstRef)
	wm := watermark()
	// The partial page proves up to the database clock read before the list: between the two
	// reads around the pass.
	if wm.Before(before) || wm.After(after) {
		t.Fatalf("watermark = %v, want the list-time clock within [%v, %v]", wm, before, after)
	}

	// enabled_at is lowered so it is the watermark, not enabled_at, that bounds the scan below.
	f.e.exec(t, `UPDATE checkpoint_retention_meta SET enabled_at = LEAST(enabled_at, $1)`, wm.Add(-3*time.Hour))
	// keyAt backdates a run's terminal transition and its last publish (its backfill key is the later).
	keyAt := func(runID uuid.UUID, statusSince, tipAt time.Time) {
		f.e.exec(t, `UPDATE runs SET status_since = $2, checkpoint_tip_at = $3 WHERE id = $1`, runID, statusSince, tipAt)
	}
	backfill := func(runID uuid.UUID) bool {
		t.Helper()
		if _, err := f.svc2.backfillCheckpointRetentions(f.e.ctx, pgconv.UUID(runID), false, nil); err != nil {
			t.Fatalf("backfill: %v", err)
		}
		_, ok := f.record(t, runID)
		return ok
	}

	late, _ := f.seedIssueRun(t, "failed", w, 1)
	keyAt(late, wm.Add(-5*time.Minute), wm.Add(-30*time.Minute))
	if !backfill(late) {
		t.Fatalf("a run committed late inside the overlap window was not backfilled")
	}

	// A terminal run whose FIRST publish came long after it went terminal (and whose publish-time
	// record insert failed): its status_since is far below the watermark, its last publish is
	// inside the window. It is keyed at the publish, so it is still backfilled.
	latePublish, _ := f.seedIssueRun(t, "failed", w, 1)
	keyAt(latePublish, wm.Add(-2*time.Hour), wm.Add(-5*time.Minute))
	if !backfill(latePublish) {
		t.Fatalf("a terminal run whose late first publish is inside the window was not backfilled")
	}

	old, _ := f.seedIssueRun(t, "failed", w, 1)
	keyAt(old, wm.Add(-20*time.Minute), wm.Add(-2*time.Hour))
	if backfill(old) {
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

// --- Round 2: a terminal run's publish landing inside a supersession ---------------------------

// TestSupersessionLatePublishInWindowLiveDB pins the supersession's post-delete list. A publish
// Publish routes as terminal cannot land inside the run's supersession (publishTerminalLocked holds
// the same retention lock; TestTerminalPublishSerializedWithSupersessionLiveDB), but one routed as
// LIVE (the status read before the run turned terminal) pushes unlocked and still can, which is the
// window this list covers. This test writes the late tip directly into that window: between the recovery-ref create (step 2) and the branch delete
// (step 3) origin's branch ref moves to the OLD run's later tip T2 and runs.checkpoint_tip follows
// it. The CAS delete on T1 is a benign no-op, and the record must NOT be marked superseded
// (that would leave T2 untracked, blocking every new run): it stays superseding with last_error.
// The late publish's own tracker moves no row and logs a Warn. Once the hold is discarded the stuck
// exit deletes the recovery ref at T1 and the branch ref at T2, and the new run publishes.
func TestSupersessionLatePublishInWindowLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	const t2 = "5555555555555555555555555555555555555555"
	logs := captureSlog(t)
	var once sync.Once
	f.forge.beforeDelete = func(ref string) {
		if ref != f.branchRef {
			return
		}
		once.Do(func() {
			push := &checkpointPush{s: f.svc2, runID: f.oldRun, branch: f.branch, ref: f.branchRef}
			if err := push.observePublishBase(f.e.ctx); err != nil {
				t.Errorf("observePublishBase: %v", err)
			}
			f.forge.set(f.branchRef, t2)
			f.e.exec(t, `UPDATE runs SET checkpoint_tip = $2, checkpoint_tip_at = now() WHERE id = $1`, f.oldRun, t2)
			f.svc2.trackPublishedCheckpoint(f.e.ctx, push, true, t2)
		})
	}

	res := f.publishNew(t, f.svc1)
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("new run's Publish = %+v, want the not_descendant skip (the slot still holds the old run's late tip)", res)
	}
	r := f.row(t)
	if r.State != retentionSuperseding || !r.LastError.Valid || !strings.Contains(r.LastError.String, "own tip") ||
		!time.Now().Before(r.NextAttemptAt.Time) {
		t.Fatalf("record = {state %q last_error %v next %v}, want superseding with last_error and backoff",
			r.State, r.LastError, r.NextAttemptAt.Time)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != t2 {
		t.Fatalf("branch ref = %q, want the late tip %s", tip, t2)
	}
	if tip, _ := f.forge.ref(f.recoveryRef); tip != retentionTestTip {
		t.Fatalf("recovery ref = %q, want the recorded tip %s", tip, retentionTestTip)
	}
	if out := logs.String(); !strings.Contains(out, "tracked by no record") || !strings.Contains(out, f.branch) ||
		!strings.Contains(out, t2) {
		t.Fatalf("no Warn naming the untracked publish (branch %s, tip %s):\n%s", f.branch, t2, out)
	}

	// Hold still open: the re-drive repeats steps 2-3 and stops again, never marks superseded.
	f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
	f.reconcileRun(t, f.svc2, f.oldRun)
	if r := f.row(t); r.State != retentionSuperseding {
		t.Fatalf("record with the hold open = %q, want still superseding", r.State)
	}

	f.discardHold(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 1 {
		t.Fatalf("reconcile progressed %d, want 1 (the stuck exit)", n)
	}
	if r := f.row(t); r.State != "deleted" {
		t.Fatalf("record = %q, want deleted", r.State)
	}
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatalf("branch ref at the old run's late tip left on origin")
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("recovery ref at the recorded tip left on origin")
	}
	if res := f.publishNew(t, f.svc2); !res.Published {
		t.Fatalf("new run's Publish after the exit = %+v, want published", res)
	}
}

// TestTrackTerminalPublishSkipsRecoveryRecordsLiveDB pins TrackTerminalCheckpointPublish's
// `recovery_ref IS NULL` guard by calling it directly: a record that ever named a recovery ref is
// never advanced or reopened, so no row comes back. The superseded case is also excluded by the
// state list; the closed-after-supersession case (deleted, still naming the branch ref, recovery_ref
// set: a tip-gone close or a stuck exit) is excluded by the recovery_ref guard alone.
func TestTrackTerminalPublishSkipsRecoveryRecordsLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, setup string }{
		{"superseded", `UPDATE checkpoint_retentions SET state = 'superseded', recovery_ref = $2, ref = $2 WHERE run_id = $1`},
		{"deleted after a supersession began", `UPDATE checkpoint_retentions SET state = 'deleted', recovery_ref = $2,
		                                          settled_at = now() WHERE run_id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupersedeFix(t)
			f.e.exec(t, tc.setup, f.oldRun, f.recoveryRef)
			before := f.row(t)
			state, err := f.e.q.TrackTerminalCheckpointPublish(f.e.ctx, store.TrackTerminalCheckpointPublishParams{
				RunID: f.oldRun, Branch: f.branch, Ref: f.branchRef, Tip: reviewLateTip,
			})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("TrackTerminalCheckpointPublish = %q, %v; want no row (0 rows moved)", state, err)
			}
			after := f.row(t)
			if after.State != before.State || after.Tip != before.Tip || after.Ref != before.Ref || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
				t.Fatalf("record moved: {state %q tip %q ref %q} -> {state %q tip %q ref %q}",
					before.State, before.Tip, before.Ref, after.State, after.Tip, after.Ref)
			}
		})
	}
}

// TestLiveRoutedLatePublishSupersededLogsWarnLiveDB pins the untracked-publish signal
// trackPublishedCheckpoint documents: a publish Publish routed as LIVE (terminal false, the status read at its top) whose
// run turned terminal while the push was in flight lands inside a supersession of the run's
// record, between the recovery-ref create and the branch CAS-delete, and BEFORE its own
// SetRunCheckpointTip commits. The supersession's post-delete list (branchHeldByOwnPublish) then
// reads the stale runs.checkpoint_tip, takes the late tip for another run's, and marks the record
// superseded. The late publish's tracker moves no row in either statement, so the Warn must fire on
// the run's CURRENT (terminal) status, not the live status Publish routed on, and the publish is
// reported untracked (its attempt row is then kept for the sweeper's reconciliation). A run still
// live at track time logs nothing. The supersession here is driven with no cooling period.
func TestLiveRoutedLatePublishSupersededLogsWarnLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		wantWarn bool
	}{
		{"terminal at track", "failed", true},
		{"live at track", "running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupersedeFix(t)
			const t2 = "7777777777777777777777777777777777777777"
			// The late publish's compare-and-set base, read before its push (the record retained).
			push := &checkpointPush{s: f.svc2, runID: f.oldRun, branch: f.branch, ref: f.branchRef}
			if err := push.observePublishBase(f.e.ctx); err != nil {
				t.Fatalf("observePublishBase: %v", err)
			}
			var once sync.Once
			f.forge.beforeDelete = func(ref string) {
				if ref != f.branchRef {
					return
				}
				// The late push lands; its tip persist has not committed yet.
				once.Do(func() { f.forge.set(f.branchRef, t2) })
			}
			f.publishNew(t, f.svc1)
			if r := f.row(t); r.State != retentionSuperseded {
				t.Fatalf("record = %q, want superseded (the stale checkpoint_tip read the late tip as another run's)", r.State)
			}

			// The late publish's outcome: its tip persists, then its tracker runs routed as live.
			if tc.status == "running" {
				// One active run per issue: retire the new run so the old one can read as live.
				f.e.exec(t, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, f.newRun)
			}
			f.e.exec(t, `UPDATE runs SET status = $2, checkpoint_tip = $3, checkpoint_tip_at = now() WHERE id = $1`,
				f.oldRun, tc.status, t2)
			logs := captureSlog(t)
			settle, tracked := f.svc2.trackPublishedCheckpoint(f.e.ctx, push, false, t2)
			if settle {
				t.Fatalf("trackPublishedCheckpoint settle = true, want false (no row moved)")
			}
			// Untracked (terminal at track) keeps the push's attempt row for the sweeper.
			if tracked == tc.wantWarn {
				t.Fatalf("trackPublishedCheckpoint tracked = %v, want %v", tracked, !tc.wantWarn)
			}
			if r := f.row(t); r.State != retentionSuperseded || r.Tip == t2 {
				t.Fatalf("record = {state %q tip %s}, want superseded at its recorded tip (neither statement moves it)", r.State, r.Tip)
			}
			out := logs.String()
			warned := strings.Contains(out, "tracked by no record")
			if warned != tc.wantWarn {
				t.Fatalf("untracked-publish Warn logged = %v, want %v:\n%s", warned, tc.wantWarn, out)
			}
			if warned && (!strings.Contains(out, f.branch) || !strings.Contains(out, t2) || !strings.Contains(out, f.oldRun.String())) {
				t.Fatalf("Warn does not name the run %s, branch %s and tip %s:\n%s", f.oldRun, f.branch, t2, out)
			}
		})
	}
}
