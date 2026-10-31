package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1324 D5 follow-ups: the custodian is the side row's worker_id, because the resume writers
// clear runs.worker_id on a released claim. Skipped unless UZI_TEST_DATABASE_URL is set.

func (f *publicationParkFixture) assertCustodianOnlyClaim(t *testing.T) {
	t.Helper()
	if r := f.run(t); r.Status != "queued" || r.WorkerID.Valid {
		t.Fatalf("precondition: status=%s worker=%v, want queued with runs.worker_id cleared", r.Status, r.WorkerID)
	}
	if s := f.side(t); !s.WorkerID.Valid || uuid.UUID(s.WorkerID.Bytes) != f.workerID {
		t.Fatalf("side custodian=%v, want %s", s.WorkerID, f.workerID)
	}
	f.env.exec(`UPDATE runs SET updated_at=now()-interval '30 days' WHERE id=$1`, f.runID)
	peer := f.peerWorker(t)
	if got, err := f.svc.Claim(f.env.ctx, peer, nil); err != nil || got != nil {
		t.Fatalf("peer claimed a custodian-bound run: %+v/%v", got, err)
	}
	if got, gen := f.claim(t); got != f.runID || gen != 2 {
		t.Fatalf("custodian claim=%s/%d, want %s/2", got, gen, f.runID)
	}
}

// The over-cap hold releases the claim; the owner's Resume then clears runs.worker_id. The
// custodian must still claim the run, and a peer must not.
func TestPublicationCustodianClaimsAfterOverCapResumeLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET requeue_count=1 WHERE id=$1`, f.runID)
	if _, err := f.env.q.FailWorkerRunsOverCap(f.env.ctx, store.FailWorkerRunsOverCapParams{
		FailureReason: pgtype.Text{String: "x", Valid: true}, WorkerID: pgconv.UUID(f.workerID), MaxRequeues: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if r := f.run(t); r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" {
		t.Fatalf("not held: status=%s cause=%v", r.Status, r.RecoveryWaitCause)
	}
	if _, err := f.env.q.ResumeWorkerRecoveryEpisode(f.env.ctx, store.ResumeWorkerRecoveryEpisodeParams{
		ID: f.runID, UserID: f.userID, GlobalTimeoutSeconds: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	// The released incarnation (D19) cannot reclaim its own parked run; the custodian
	// re-registers, which rotates its nonce.
	f.env.exec(`UPDATE workers SET snapshot_register_nonce='re-registered' WHERE id=$1`, f.workerID)
	f.assertCustodianOnlyClaim(t)
}

// The wall-park twin: the owner's Extend of a server-parked wall hold clears runs.worker_id.
func TestPublicationCustodianClaimsAfterWallParkExtendLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET status='paused', hold_reason='budget_exhausted', status_since=now(), claim_released_at=now() WHERE id=$1`, f.runID)
	if _, err := f.env.q.ExtendAndResumeWallPark(f.env.ctx, store.ExtendAndResumeWallParkParams{
		ID: f.runID, UserID: f.userID, Secs: 3600, Cap: 86400, MaxWallSeconds: 2147483, GlobalTimeoutSeconds: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	f.assertCustodianOnlyClaim(t)
}

// Each park sets the handoff to the newly verified value or NULL, and the custodian to the
// reporting worker; an older handoff must never survive a later park.
func TestPublicationParkReplacesStaleHandoffLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET checkpoint_tip=$2 WHERE id=$1`, f.runID, pubHeadC)
	other := uuid.New()
	f.env.exec(`UPDATE run_publication_resume SET worker_id=$2 WHERE run_id=$1`, f.runID, other)
	req := pubParkReport(1, "git", "push")
	req.HandoffCheckpointTip = strPtr(pubHeadC)
	if _, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, req); err != nil || !applied {
		t.Fatalf("park=%v/%v", applied, err)
	}
	s := f.side(t)
	if len(s.Handoff) == 0 || !s.WorkerID.Valid || uuid.UUID(s.WorkerID.Bytes) != f.workerID {
		t.Fatalf("handoff=%s custodian=%v, want a handoff and the reporting worker", s.Handoff, s.WorkerID)
	}
	for name, tip := range map[string]*string{"absent tip": nil, "mismatched tip": strPtr(pubHeadB)} {
		t.Run(name, func(t *testing.T) {
			f.resumeFlight(t)
			f.clock = f.clock.Add(time.Minute)
			again := pubParkReport(1, "git", "push")
			again.HandoffCheckpointTip = tip
			if _, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, again); err != nil || !applied {
				t.Fatalf("park=%v/%v", applied, err)
			}
			if got := f.side(t); len(got.Handoff) != 0 {
				t.Fatalf("stale handoff survived: %s", got.Handoff)
			}
			// Restore a handoff for the next sub-case.
			f.env.exec(`UPDATE run_publication_resume SET handoff='{"checkpoint_tip":"`+pubHeadC+`"}'::json WHERE run_id=$1`, f.runID)
		})
	}
}

// The completion-hold continue tolerates a deleted worker row: the intent is still retired in the
// same transaction instead of failing on the missing opt-in.
func TestPublicationContinueWithDeletedWorkerLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET status='paused', hold_reason='completion_blocked', status_since=now() WHERE id=$1`, f.runID)
	run := f.run(t)
	run.WorkerID = pgconv.UUID(uuid.New()) // a worker row that no longer exists
	if err := f.svc.continueCompletionRetiringIntent(f.env.ctx, run, f.userID, ""); err != nil {
		t.Fatalf("continue with a deleted worker: %v", err)
	}
	if _, ok := activeIntentSeq(t, f.env, f.runID); ok {
		t.Fatal("intent still active")
	}
	if r := f.run(t); r.Status != "queued" {
		t.Fatalf("status=%s", r.Status)
	}
}

// A failure in the retirement half rolls the decision back too: a decision committed first and a
// retirement second would leave a queued run with a live intent.
func TestPublicationContinueRetireFailureRollsBackDecisionLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET status='paused', hold_reason='completion_blocked', status_since=now() WHERE id=$1`, f.runID)
	f.env.exec(`CREATE FUNCTION zz_inject_retire_failure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state = 'retired' THEN RAISE EXCEPTION 'injected retire failure' USING ERRCODE = 'P0001'; END IF;
    RETURN NEW;
END; $$`)
	f.env.exec(`CREATE TRIGGER zz_inject_retire_failure_trg BEFORE UPDATE ON run_publication_resume FOR EACH ROW EXECUTE FUNCTION zz_inject_retire_failure()`)
	t.Cleanup(func() {
		f.env.exec(`DROP TRIGGER IF EXISTS zz_inject_retire_failure_trg ON run_publication_resume`)
		f.env.exec(`DROP FUNCTION IF EXISTS zz_inject_retire_failure()`)
	})
	if _, err := f.svc.ContinueCompletionDecision(f.env.ctx, f.userID, f.runID, "go on"); err == nil || errors.Is(err, ErrCompletionNotBlocked) {
		t.Fatalf("err=%v, want the injected retire failure", err)
	}
	if r := f.run(t); r.Status != "paused" || r.HoldReason.String != "completion_blocked" {
		t.Fatalf("decision committed without the retirement: status=%s hold=%v", r.Status, r.HoldReason)
	}
	if seq, ok := activeIntentSeq(t, f.env, f.runID); !ok || seq != 1 {
		t.Fatalf("intent=%d/%v, want active seq 1", seq, ok)
	}
}
