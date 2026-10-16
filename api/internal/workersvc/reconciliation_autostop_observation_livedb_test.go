package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReconciliationAutoStopDelayedObservationLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	now := time.Now()
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
	if err != nil || claim == nil || claim.ClaimGeneration != 2 {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "running", ClaimGeneration: i64(2)}); err != nil || !applied {
		t.Fatalf("running: %v %v", applied, err)
	}
	f.svc.now = func() time.Time { return now.Add(-autoStopWindow - time.Second) }
	for i := 0; i < autoStopStreak; i++ {
		if err := f.svc.AppendMessagesForClaim(f.ctx, f.wkr, id, []IncomingMessage{{Seq: 1, Kind: "tool_result", Payload: []byte(`{"n":1e1000000}`)}}, i64(2)); err == nil {
			t.Fatal("poison accepted")
		}
	}
	f.svc.now = func() time.Time { return now }
	f.svc.persistFail.recordSuccess(uuid.New(), now)
	before := f.svc.persistFail.stats(id)
	if before.generation != 2 || before.streak != autoStopStreak {
		t.Fatalf("current evidence: %+v", before)
	}
	// Observations retain the generation from the originating locked lookup,
	// even when the request finished long before the recorder obtained its lock.
	for _, status := range []string{"running", "queued"} {
		for _, failure := range []error{nil, ErrUnstorableMessage} {
			f.svc.recordAppendObservation(f.ctx, f.wkr, id, appendObservation{resolved: true, status: status, generation: 1}, failure)
			if got := f.svc.persistFail.stats(id); got != before {
				t.Fatalf("stale %s/%v changed new evidence: %+v", status, failure, got)
			}
		}
	}
	f.svc.persistFail.markStopRequested(id, now, 1)
	f.svc.persistFail.evict(id, 1) // owner cleanup arriving after a successor recorded
	if got := f.svc.persistFail.stats(id); got != before {
		t.Fatalf("old bookkeeping changed successor: %+v", got)
	}
	if f.svc.persistFail.shouldLogDecline(id, now, autoStopWindow, 1) {
		t.Fatal("old decline marked successor")
	}
	candidates := f.svc.persistFail.candidates(now)
	if len(candidates) != 1 || !f.svc.evaluateAutoStop(f.ctx, now, candidates[0]) {
		t.Fatalf("current candidate refused: %+v", candidates)
	}
	requested := f.svc.persistFail.stats(id)
	if requested.generation != 2 || requested.stopReqAt.IsZero() {
		t.Fatalf("current escalation not marked: %+v", requested)
	}
	f.svc.persistFail.markStopRequested(id, now.Add(time.Hour), 1)
	if got := f.svc.persistFail.stats(id); got != requested {
		t.Fatalf("old escalation changed new clock: %+v", got)
	}
	f.svc.now = func() time.Time { return now.Add(autoStopEscalateAfter + time.Second) }
	f.svc.persistFail.recordSuccess(uuid.New(), f.svc.now())
	candidates = f.svc.persistFail.candidates(f.svc.now())
	if len(candidates) != 1 || !f.svc.evaluateAutoStop(f.ctx, f.svc.now(), candidates[0]) {
		t.Fatal("current escalation refused")
	}
	if got := reconciliationRow(t, f, id); got.Status != "failed" {
		t.Fatalf("escalation status: %s", got.Status)
	}
}

func TestReconciliationAutoStopReleasedRunningLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	// Exercise the durable release predicate independently of status. A stale
	// running view must never authorize either auto-stop mutation after release.
	f.exec("UPDATE runs SET status='running' WHERE id=$1", id)
	now := time.Now()
	for i := 0; i < autoStopStreak; i++ {
		f.svc.persistFail.recordFailure(id, persistFailUnstorable, 0, now.Add(-autoStopWindow-time.Second), 1)
	}
	f.svc.persistFail.recordSuccess(uuid.New(), now)
	c := f.svc.persistFail.candidates(now)[0]
	if f.svc.evaluateAutoStop(f.ctx, now, c) {
		t.Fatal("released generation acted")
	}
	if got := reconciliationRow(t, f, id); got.Status != "running" || got.StopKind.Valid {
		t.Fatalf("released run changed: %s %v", got.Status, got.StopKind)
	}
}
