package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// PRD #4206: fences of the cache prune attempt that the broader livedb cases leave open. Skipped
// without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).

// seedOpenCustodyHold gives the worker an open recovery custody hold, as a run's held work does.
func (f *hibFx) seedOpenCustodyHold(id uuid.UUID) {
	f.t.Helper()
	run := f.queuedRun()
	f.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
	            VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, uuid.New(), f.user, f.repo, run, id)
}

// A report is bound to the attempt's own register nonce (the one it was issued under), not to the
// worker's current registration: after a re-register only the attempt's nonce settles it.
func TestHibernationCleanupReportBindsAttemptNonceLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.started()
	n1 := f.row(id).CleanupRegisterNonce.String
	f.register(id, nil)
	w := f.wantPhase(id, CleanupPhaseUnknown)
	n2 := w.SnapshotRegisterNonce.String
	if n1 == "" || n2 == "" || n1 == n2 || w.CleanupRegisterNonce.String != n1 {
		t.Fatalf("nonces: attempt %q, snapshot %q, stored %q", n1, n2, w.CleanupRegisterNonce.String)
	}
	wrong := f.resultRep(id, true, okSteps())
	wrong.RegisterNonce = n2
	f.wantCleanupConflict(id, wrong)
	f.wantPhase(id, CleanupPhaseUnknown)

	f.mustReport(id, f.resultRep(id, true, okSteps())) // carries n1
	f.wantPhase(id, CleanupPhaseSettled)
	f.wantOutcome(id, HibernationCleanupPruned, "")
	f.want(id, "preparing")
}

// The agent's custody evidence must be fresh even when it postdates the request.
func TestHibernationCleanupEvidenceFreshnessBoundLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.dockerWorker()
	f.hibernate(id)
	f.env.exec(`UPDATE workers SET cleanup_requested_at = now() - interval '10 minutes' WHERE id = $1`, id)
	r := f.readyRep(id)
	old := time.Now().Add(-90 * time.Second) // after the request, older than the freshness bound
	r.CustodyCheckedAt = &old
	f.wantCleanupConflict(id, r)
	f.wantPhase(id, CleanupPhaseRequested)

	fresh := time.Now().Add(-5 * time.Second)
	r.CustodyCheckedAt = &fresh
	f.mustReport(id, r)
	f.wantPhase(id, CleanupPhaseStarted)
}

// An open custody hold on the worker is a server-side admission failure. The fixture reports
// failures on its own testing.T, so each path is its own top-level test and stays observable when
// the other is red.
func TestHibernationCleanupCustodyHoldAtReadyLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.dockerWorker()
	f.hibernate(id)
	f.seedOpenCustodyHold(id)
	f.mustReport(id, f.readyRep(id))
	f.wantPhase(id, CleanupPhaseSettled)
	f.wantOutcome(id, HibernationCleanupSkipped, CleanupReasonCustodyHold)
	f.want(id, "waking")
}

func TestHibernationCleanupCustodyHoldAtResultLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.started()
	f.seedOpenCustodyHold(id)
	f.mustReport(id, f.resultRep(id, true, okSteps()))
	f.wantPhase(id, CleanupPhaseSettled)
	f.wantOutcome(id, HibernationCleanupPruned, "")
	f.want(id, "waking")
}
