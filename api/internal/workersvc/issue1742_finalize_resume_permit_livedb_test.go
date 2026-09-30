package workersvc

import (
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestFinalizeResumeAllowanceCompletesOnlyAtNewGenerationLiveDB is the #1742 end-to-end chain over
// the real Register, ClaimRun and completion-permit path: an interlocked run whose executor
// finished at generation 1 (one requeue already spent, budget 1) is re-queued by the attested
// pass, re-claimed by the same worker at generation 2, and can complete only through a permit
// requested at generation 2 (a generation-1 permit request is refused as a stale claim).
func TestFinalizeResumeAllowanceCompletesOnlyAtNewGenerationLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	e.exec(t, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, wid)
	runID := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, `UPDATE runs SET claim_generation = 1, requeue_count = 1, claimed_at = now() WHERE id = $1`, runID)

	snap := &ActiveSnapshot{Active: []ActiveRunEntry{}, FinalizeResume: []FinalizeResumeEntry{{RunID: runID.String(), ClaimGeneration: 1}}}
	if _, _, err := svc.Register(e.ctx, store.Worker{ID: wid, UserID: e.userID}, "v1", "base", nil, nil,
		[]string{"completion_interlock_v1"}, snap); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := e.runStatus(t, runID); got != "queued" {
		t.Fatalf("after Register status = %q, want queued", got)
	}

	var allowanceGeneration int64
	var requeues int32
	if err := e.pool.QueryRow(e.ctx,
		`SELECT COALESCE(finalize_resume_generation, -1), requeue_count FROM runs WHERE id = $1`, runID).
		Scan(&allowanceGeneration, &requeues); err != nil {
		t.Fatalf("read allowance provenance: %v", err)
	}
	if allowanceGeneration != 1 || requeues != 2 {
		t.Fatalf("after Register finalize_resume_generation=%d requeue_count=%d, want 1/2", allowanceGeneration, requeues)
	}

	wkr, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	claimed, err := e.q.ClaimRun(e.ctx, claimRunParamsFor(wkr))
	if err != nil || claimed.ID != runID || claimed.ClaimGeneration != 2 {
		t.Fatalf("ClaimRun = %s gen %d err=%v, want %s at generation 2", claimed.ID, claimed.ClaimGeneration, err, runID)
	}
	// ClaimRun leaves the run 'claimed'; the worker reports it running once the flight starts.
	e.exec(t, `UPDATE runs SET status = 'running' WHERE id = $1`, runID)

	const (
		head   = "1742beef"
		branch = "agent/issue-1"
	)
	stale, fresh := int64(1), int64(2)
	if _, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head, ClaimGeneration: &stale}); !errors.Is(err, ErrCompletionStaleClaim) {
		t.Fatalf("permit at the old generation: err = %v, want ErrCompletionStaleClaim", err)
	}
	p, err := svc.RequestCompletionPermit(e.ctx, wkr, runID,
		CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head, ClaimGeneration: &fresh})
	if err != nil || !p.Granted {
		t.Fatalf("permit at generation 2: %+v err=%v, want granted", p, err)
	}
	run, applied, err := svc.SetState(e.ctx, wkr, runID,
		StateRequest{State: "completed", Head: strPtr(head), Branch: strPtr(branch), ClaimGeneration: &fresh})
	if err != nil || !applied || run.Status != "completed" {
		t.Fatalf("completion at generation 2: applied=%v status=%q err=%v, want completed", applied, run.Status, err)
	}
}
