package workersvc

import (
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Resume revokes execution, not custody. A real claim opens the next generation;
// an old report must stay stale on both sides of that claim.
func TestDiskHoldResumeExecutionFenceAcrossClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	f.svc.p.RunDiskParkMax = 1
	id := f.seedRun("running", 1)
	env.exec("UPDATE runs SET worker_id=$2,disk_park_count=1 WHERE id=$1", id, f.workerID)
	env.exec(`INSERT INTO recovery_custody_holds
 (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES ($1,$2,$3,1,'open',$4,'fixture',$4,$3,true)`, f.userID, f.repoID, id, f.workerID)
	held, applied, err := f.svc.SetState(env.ctx, f.wkr, id, dataVolumeFullReport(1, nil))
	if err != nil || !applied || !isDataVolumeFullHeld(held) {
		t.Fatalf("hold: applied=%v status=%s err=%v", applied, held.Status, err)
	}
	// Even a duplicate disk report must supply its required generation.
	if _, applied, err := f.svc.SetState(env.ctx, f.wkr, id, StateRequest{State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseDataVolumeFull)}); !errors.Is(err, ErrInvalidState) || applied {
		t.Fatalf("missing held generation: applied=%v err=%v", applied, err)
	}
	if _, err := env.q.ResumeDataVolumeFullHold(env.ctx, store.ResumeDataVolumeFullHoldParams{ID: id, UserID: f.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	stale := func() {
		t.Helper()
		for _, state := range []string{"failed", "completed", "awaiting_input", "awaiting_followup", "recovery_wait"} {
			req := StateRequest{State: state, ClaimGeneration: i64Ptr(1)}
			if state == "recovery_wait" {
				req = dataVolumeFullReport(1, nil)
			}
			if _, applied, err := f.svc.SetState(env.ctx, f.wkr, id, req); !errors.Is(err, ErrStaleClaim) || applied {
				t.Fatalf("old %s: applied=%v err=%v", state, applied, err)
			}
		}
	}
	stale()
	got, g := f.claim(t)
	if got != id || g != 2 {
		t.Fatalf("real claim=%s/%d want %s/2", got, g, id)
	}
	stale()
	if _, applied, err := f.svc.SetState(env.ctx, f.wkr, id, StateRequest{State: "running", ClaimGeneration: i64Ptr(g)}); err != nil || !applied {
		t.Fatalf("fresh execution: %v/%v", applied, err)
	}
	run, applied, err := f.svc.SetState(env.ctx, f.wkr, id, dataVolumeFullReport(g, nil))
	if err != nil || !applied || !isDataVolumeFullHeld(run) || run.DiskParkCount != 1 {
		t.Fatalf("rehold: applied=%v status=%s count=%d err=%v", applied, run.Status, run.DiskParkCount, err)
	}
	if n := f.ownOpenHolds(t, id); n != 2 {
		t.Fatalf("custody holds=%d want both retained generations", n)
	}
}
