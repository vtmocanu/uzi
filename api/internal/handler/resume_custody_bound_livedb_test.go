package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestResumeNowCustodyRunHoldBoundLiveDB (issue #4162): POST /api/runs/{id}/resume-now on a
// custody-parked run whose OWN open holds equal the limit as group-eligible same-worker holds
// (so the owner's admission-counted units stay below the limit). It pins the route's part only:
// the resume succeeds, requeues the run and leaves the hold rows untouched, so the admission read
// still reports RunHoldBoundReached. The claim-time refusal is not exercised here (this package
// has no claim fixture); TestCustodyRunHoldBoundOwnerResumeVerbsLiveDB and
// TestCustodyRunHoldBoundPublicationResumeLiveDB in workersvc cover it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).
func TestResumeNowCustodyRunHoldBoundLiveDB(t *testing.T) {
	const limit = 3
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	ownerUzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	repoID := cliSeedOwnedRepo(t, pool, owner)

	workerID := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at)
	  VALUES ($1,$2,'bound',$3,'offline',now() - interval '24 hours')`, workerID, owner, workerID[:])

	runID := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, claim_generation, recovery_wait_cause)
		 VALUES ($1, $2, $3, 4162, 'bound', 'desc', 'recovery_wait', 'issue', $4, 'retained_recovery_exhausted')`, runID, owner, repoID, limit)
	t.Cleanup(func() {
		cliMustExec(t, pool, `DELETE FROM recovery_custody_holds WHERE user_id = $1`, owner)
	})
	base := time.Now().Add(-time.Hour)
	for g := 1; g <= limit; g++ {
		cliMustExec(t, pool, `INSERT INTO recovery_custody_holds
		  (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,created_at)
		  VALUES ($1,$2,$3,$4,$5,'open',$6,'fixture',$6,$4,$7)`,
			uuid.New(), owner, repoID, runID, g, workerID, base.Add(time.Duration(g)*time.Second))
	}
	snapshot := func() string {
		var s string
		if err := pool.QueryRow(context.Background(), `SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY h.id),'[]'::jsonb)::text
		  FROM recovery_custody_holds h WHERE h.run_id=$1`, runID).Scan(&s); err != nil {
			t.Fatalf("snapshot holds: %v", err)
		}
		return s
	}
	before := snapshot()

	rec := bearerReq(router, http.MethodPost, "/api/runs/"+runID.String()+"/resume-now", ownerUzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume-now = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "queued" {
		t.Fatalf("status after resume-now = %q, want queued", status)
	}

	cutoff := time.Now().Add(-time.Minute)
	q := store.New(pool)
	adm, err := q.GetCustodyAdmissionForRun(context.Background(), store.GetCustodyAdmissionForRunParams{
		UserID: owner, RunID: runID, CustodyHoldLimit: limit, HeartbeatCutoff: pgconv.Time(cutoff),
	})
	if err != nil {
		t.Fatalf("GetCustodyAdmissionForRun: %v", err)
	}
	if !adm.RunHoldBoundReached || adm.ContinuationExempt || adm.OwnOpenHolds != limit {
		t.Fatalf("admission after resume-now = %+v, want bound reached, not exempt, own %d", adm, limit)
	}
	if adm.AdmissionCountedHolds >= limit {
		t.Fatalf("owner units = %d, want below %d (the group is one unit, so only the per-run bound refuses)", adm.AdmissionCountedHolds, limit)
	}
	if after := snapshot(); after != before {
		t.Fatalf("resume-now changed hold rows:\nbefore=%s\nafter=%s", before, after)
	}
}
