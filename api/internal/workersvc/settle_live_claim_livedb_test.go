package workersvc

import (
	"testing"

	"github.com/google/uuid"
)

// TestCrossWorkerRecoveredRunClaimsAtCapLiveDB (issue #1751 M2): a live settle is same-worker
// only, so a run requeued after ANOTHER worker's generation left its custody hold open cannot
// have that hold settled live by the new claimant (the handler live-DB suite pins that answer
// as not_eligible). The recovered run must still be claimable by the other worker at the
// owner's custody cap: ClaimRun's continuation exemption (M1) keys on the run's own open hold,
// not on which worker took it. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres.
func TestCrossWorkerRecoveredRunClaimsAtCapLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	run := f.seedRun("queued", 1)
	// The generation-1 hold was taken by a DIFFERENT (crashed) worker, not the claimant.
	otherWorker := uuid.New()
	f.env.exec(`INSERT INTO recovery_custody_holds
	              (user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity)
	            VALUES ($1, $2, $3, 1, 'open', $4, 'crashed-worker-identity')`,
		f.userID, f.repoID, run, otherWorker)
	f.seedUnrelatedOpenHolds(custodyHoldLimit - 1)
	if got := f.openHolds(t); got != custodyHoldLimit {
		t.Fatalf("precondition: open holds = %d, want %d (owner at the cap)", got, custodyHoldLimit)
	}
	got, gen := f.claim(t)
	if got != run {
		t.Fatalf("claimed %s, want the recovered run %s (another worker's hold still exempts it at the cap)", got, run)
	}
	if gen != 2 {
		t.Fatalf("claim generation = %d, want 2", gen)
	}
}
