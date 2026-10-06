package handler

import (
	"github.com/google/uuid"
	"net/http"
	"testing"
)

func TestCrossCheckActiveRepoDeleteRetainsRowsLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, owner)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 991, false)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	child := uuid.New()
	cc := uuid.New()
	cliMustExec(t, pool, `UPDATE runs SET harness='claude',plan_cross_check_required=true WHERE id=$1`, lead)
	cliMustExec(t, pool, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES ($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','candidate')`, child, owner, repo, lead)
	cliMustExec(t, pool, `INSERT INTO cross_checks (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,deadline_at)
 VALUES ($1,$2,$3,'plan',1,0,'plan','[]','s',repeat('a',40),$4,now()+interval '30 minutes')`, cc, lead, child, []byte("digest"))
	rec := cookieReq(t, router, http.MethodDelete, "/api/repos/"+repo.String(), jwt, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("active repo delete: %d %s", rec.Code, rec.Body.String())
	}
	var intact bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM cross_checks WHERE id=$1 AND lead_run_id=$2 AND checker_run_id=$3)
 AND EXISTS(SELECT 1 FROM runs WHERE id=$2) AND EXISTS(SELECT 1 FROM runs WHERE id=$3)`, cc, lead, child).Scan(&intact); err != nil || !intact {
		t.Fatalf("refusal removed cross-check rows: intact=%v err=%v", intact, err)
	}
	// Retention is asserted before cleanup; the queued checker must not survive
	// into subsequent tests that query provisionable runs across all users.
	t.Cleanup(func() {
		cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner)
	})
}
