package handler

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func cleanupSeedWorker(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, codeCap bool) (uuid.UUID, string) {
	t.Helper()
	id, token := uuid.New(), "cleanup-"+uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	caps := []string{}
	if codeCap {
		caps = append(caps, "cross_check_code_v1")
	}
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'cleanup',$3,'online',$4)", id, user, hash[:], caps)
	return id, token
}

func TestCodeSnapshotCleanupMetadataLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 21971, true)
	worker, token := cleanupSeedWorker(t, pool, owner, true)
	foreign, foreignToken := cleanupSeedWorker(t, pool, owner, true)
	_, noCapToken := cleanupSeedWorker(t, pool, owner, false)
	_, otherUserToken := cleanupSeedWorker(t, pool, cliSeedUser(t, pool, false), true)
	lead := rmSeedRun(t, pool, owner, repo, "completed")
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", lead, worker)
	child := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,status,claim_generation,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES($1,$2,$3,$4,'cross_check',$5,'codex','completed',7,true,1800,'cleanup','candidate')`, child, owner, repo, worker, lead)
	cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,base_commit,head_commit,candidate_digest,verdict,outcome,code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,deadline_at)
 VALUES($1,$2,'codex','code',1,1,$3,$4,$5,'failed','pending','{}','','',sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),now()-interval '30 minutes')`, lead, child, strings.Repeat("a", 40), strings.Repeat("b", 40), make([]byte, 32))
	call := func(auth, suffix string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/worker/runs/"+lead.String()+"/ownership"+suffix, nil)
		req.Header.Set("Authorization", "Bearer "+auth)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	snapshot := func() string {
		t.Helper()
		var state string
		err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object('lead',to_jsonb(l),'child',to_jsonb(c),'check',to_jsonb(x))::text FROM runs l JOIN runs c ON c.id=$2 JOIN cross_checks x ON x.lead_run_id=l.id AND x.stage='code' WHERE l.id=$1`, lead, child).Scan(&state)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := snapshot()
	ordinary := call(token, "")
	var ordinaryBody map[string]any
	if ordinary.Code != 200 || json.Unmarshal(ordinary.Body.Bytes(), &ordinaryBody) != nil || ordinaryBody["status"] != "completed" || ordinaryBody["protocol"] != nil || ordinaryBody["inventory_guarded"] == nil {
		t.Fatalf("ordinary probe changed: %d %s", ordinary.Code, ordinary.Body.String())
	}
	assertMetadata := func(owned bool, outcome string) {
		t.Helper()
		rec := call(token, "?purpose=code_snapshot")
		var body apitypes.CodeSnapshotCleanup
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Protocol != "code_snapshot_cleanup_v1" || body.LeadRunID != lead.String() || body.HeadCommit == nil || *body.HeadCommit != strings.Repeat("b", 40) || body.Outcome != outcome || body.LeadStatus != "completed" || body.OwnedByWorker != owned || body.CheckerRunID != child.String() || body.CheckerClaimGeneration != 7 || body.CheckerStatus != "completed" {
			t.Fatalf("metadata: %d %s", rec.Code, rec.Body.String())
		}
		var fields map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil || len(fields) != 9 {
			t.Fatalf("unexpected schema: %s", rec.Body.String())
		}
	}
	assertMetadata(true, "pending")
	if after := snapshot(); after != before {
		t.Fatal("read changed rows, expired evidence, or credited wait")
	}
	for _, auth := range []string{foreignToken, noCapToken, otherUserToken} {
		if rec := call(auth, "?purpose=code_snapshot"); rec.Code != 404 {
			t.Fatalf("unauthorized metadata: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, suffix := range []string{"?purpose=", "?purpose=other", "?purpose=code_snapshot&purpose=code_snapshot", "?purpose=code_snapshot&purpose=other", "?purpose=code_snapshot&bad=%zz"} {
		if rec := call(token, suffix); rec.Code != 400 {
			t.Fatalf("malformed purpose: %s %d", suffix, rec.Code)
		}
	}
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", lead, foreign)
	assertMetadata(false, "failed")
	if rec := call(token, ""); rec.Code != 404 {
		t.Fatal("ordinary probe read foreign lead")
	}
	cliMustExec(t, pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", lead)
	assertMetadata(false, "failed")
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_released_at=now() WHERE id=$1", lead, worker)
	assertMetadata(false, "failed")
	cliMustExec(t, pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", child)
	if rec := call(token, "?purpose=code_snapshot"); rec.Code != 404 {
		t.Fatal("cleared child affinity accepted")
	}
	cliMustExec(t, pool, "UPDATE cross_checks SET checker_run_id=NULL WHERE lead_run_id=$1 AND stage='code'", lead)
	if rec := call(token, "?purpose=code_snapshot"); rec.Code != 404 {
		t.Fatal("cleared child mapping accepted")
	}
	cliMustExec(t, pool, "DELETE FROM runs WHERE id=$1", child)
	if rec := call(token, "?purpose=code_snapshot"); rec.Code != 404 {
		t.Fatal("missing child accepted")
	}
	// Plan-only evidence on another lead must never establish code lineage.
	planLead := rmSeedRun(t, pool, owner, repo, "running")
	planChild := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,status,report_only,budget_wall_seconds,issue_title,issue_description) VALUES($1,$2,$3,$4,'cross_check',$5,'codex','running',true,1800,'plan','candidate')`, planChild, owner, repo, worker, planLead)
	cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,verdict,deadline_at,plan_md,milestones,size_class,base_commit,candidate_digest) VALUES($1,$2,'codex','plan',1,1,'pending',now()+interval '30 minutes','plan','[]','small',$3,$4)`, planLead, planChild, strings.Repeat("a", 40), make([]byte, 32))
	lead = planLead
	if rec := call(token, "?purpose=code_snapshot"); rec.Code != 404 {
		t.Fatal("plan child accepted")
	}
}
