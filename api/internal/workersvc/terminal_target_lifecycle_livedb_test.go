package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// termClaim runs the real ClaimRun for the worker in its own transaction (worker row locked first,
// as the claim path does) and returns the run it claimed.
func termClaim(t *testing.T, e interlockLiveDB, workerID uuid.UUID) store.Run {
	t.Helper()
	now := time.Now()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	q := store.New(tx)
	if _, err := q.GetWorkerForUpdate(e.ctx, workerID); err != nil {
		t.Fatal(err)
	}
	run, err := q.ClaimRun(e.ctx, store.ClaimRunParams{
		WorkerID: pgconv.UUID(workerID), UserID: e.userID,
		WorkerProtocolCaps: []string{capability.RecoveryArchiveV1},
		RecoveryCapable:    true, WorkerIdentity: "terminal-target-test",
		AffinityCutoff:        pgconv.Time(now.Add(-time.Hour)),
		SpreadCutoff:          pgconv.Time(now.Add(-time.Hour)),
		HeartbeatCutoff:       pgconv.Time(now.Add(-time.Minute)),
		BackgroundGraceCutoff: pgconv.Time(now.Add(-time.Hour)),
		CrossCheckEvaluatedAt: pgconv.Time(now),
	})
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	return run
}

// A paused ci_fix is held (left alone by the poller policy), resumes, is claimed again at a new
// generation, and the publication backstop then stops it before it publishes.
func TestTerminalTargetResumedHeldCIFixLiveDB(t *testing.T) {
	p := newTermPub(t, "ci_fix", forge.MRStateMerged)
	e := p.f.e
	c := &termCancelEnv{e: e, svc: p.f.svc, rec: newReadmitRecorder(), worker: p.f.w.ID, wkr: p.f.w}
	e.exec(t, "UPDATE runs SET status='paused', status_since=now() WHERE id=$1", p.id)
	r := termRun{id: p.id, kind: "ci_fix", mr: 7, branch: p.branch, repo: e.repoID}
	c.requireNoOp(t, r, func() error { return c.cancel(t, r) })

	if _, err := e.q.ResumePausedRun(e.ctx, store.ResumePausedRunParams{
		ID: p.id, UserID: e.userID, GlobalTimeoutSeconds: 7200,
		StaleCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st, _, _, _, _, _ := c.fields(t, p.id); st != "queued" {
		t.Fatalf("status after resume = %s", st)
	}
	claimed := termClaim(t, e, p.f.w.ID)
	if claimed.ID != p.id || claimed.ClaimGeneration != 2 {
		t.Fatalf("claimed %s generation %d, want %s generation 2", claimed.ID, claimed.ClaimGeneration, p.id)
	}
	got := p.f.svc.ResolvePublicationContext(e.ctx, p.f.w, p.id,
		PublicationContextRequest{ClaimGeneration: 2, Branch: p.branch, CandidateHead: "cccccccccccccccccccccccccccccccccccccccc"})
	wantUnknown(t, got, "target_terminal")
	gen := int64(2)
	res, err := p.f.svc.SetStateReportWithReconciliation(e.ctx, p.f.w, p.id, StateRequest{State: "failed", ClaimGeneration: &gen,
		FailureReason: strPtr("worker text"), FailOrigin: strPtr("agent_failure")})
	if err != nil || !res.Applied {
		t.Fatalf("failed report: %+v %v", res, err)
	}
	if st, sk, sr, failure, origin, _ := c.fields(t, p.id); st != "cancelled" || sk != "cancelled" || sr != termMergedReason || failure != "" || origin != "" {
		t.Fatalf("terminal: %s %s %q %q %q", st, sk, sr, failure, origin)
	}
}

// An operator cancel stamped while the worker was live survives the worker going stale: neither
// the poller entry nor the legacy rework entry writes, the sweep requeues the run, a reclaim
// drains the operator's cancel and the run ends cancelled with the operator's reason.
func TestTerminalTargetOperatorCancelSurvivesStaleWorkerLiveDB(t *testing.T) {
	p := newTermPub(t, "mr_rework", forge.MRStateMerged)
	e := p.f.e
	c := &termCancelEnv{e: e, svc: p.f.svc, rec: newReadmitRecorder(), worker: p.f.w.ID, wkr: p.f.w}
	r := termRun{id: p.id, kind: "mr_rework", mr: 7, branch: p.branch, repo: e.repoID}
	c.heartbeat(t, true)

	res, err := p.f.svc.SubmitInputWithOptions(e.ctx, e.userID, p.id, "cancel", "operator reason", nil, SubmitInputOptions{})
	if err != nil || res.ServerSide {
		t.Fatalf("operator cancel: %+v %v (want enqueued for the live worker)", res, err)
	}
	if st, sk, sr, _, _, inputs := c.fields(t, p.id); st != "running" || sk != "cancelled" || sr != "operator reason" || inputs != 1 {
		t.Fatalf("after operator cancel: %s %s %q inputs=%d", st, sk, sr, inputs)
	}

	c.heartbeat(t, false)
	c.requireNoOp(t, r, func() error { return c.cancel(t, r) })
	c.requireNoOp(t, r, func() error { return p.f.svc.CancelReworkForMR(e.ctx, e.repoID, 7, termMergedReason) })

	if _, err := p.f.svc.Sweep(e.ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	st, sk, sr, _, _, _ := c.fields(t, p.id)
	if st != "queued" || sk != "cancelled" || sr != "operator reason" {
		t.Fatalf("after sweep: %s %s %q (want a requeued run keeping the operator's stop)", st, sk, sr)
	}
	c.heartbeat(t, true)
	claimed := termClaim(t, e, p.f.w.ID)
	if claimed.ID != p.id || claimed.ClaimGeneration != 2 {
		t.Fatalf("claimed %s generation %d", claimed.ID, claimed.ClaimGeneration)
	}
	gen := int64(2)
	got, err := p.f.svc.ConsumeInputs(e.ctx, p.f.w, p.id, &gen)
	if err != nil {
		t.Fatal(err)
	}
	drained := got.TerminalCancelled != nil
	for _, in := range got.Inputs {
		drained = drained || in.Kind == "cancel"
	}
	if !drained {
		t.Fatalf("the reclaimed flight did not receive the operator's cancel: %+v", got)
	}
	out, err := p.f.svc.SetStateReportWithReconciliation(e.ctx, p.f.w, p.id, StateRequest{State: "failed", ClaimGeneration: &gen,
		FailureReason: strPtr("worker text"), FailOrigin: strPtr("agent_failure")})
	if err != nil || !out.Applied {
		t.Fatalf("failed report: %+v %v", out, err)
	}
	// The operator path never touched failure_reason (the seeded stale text is not ours to clear).
	if st, sk, sr, _, origin, _ := c.fields(t, p.id); st != "cancelled" || sk != "cancelled" || sr != "operator reason" || origin != "" {
		t.Fatalf("terminal: %s %s %q %q", st, sk, sr, origin)
	}
}
