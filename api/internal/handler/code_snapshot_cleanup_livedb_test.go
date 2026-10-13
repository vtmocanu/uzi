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
	fetch := func(auth string) (apitypes.CodeSnapshotCleanup, map[string]any, *httptest.ResponseRecorder) {
		t.Helper()
		rec := call(auth, "?purpose=code_snapshot")
		var body apitypes.CodeSnapshotCleanup
		var fields map[string]any
		if rec.Code == 200 && (json.Unmarshal(rec.Body.Bytes(), &body) != nil || json.Unmarshal(rec.Body.Bytes(), &fields) != nil || len(fields) != 12) {
			t.Fatalf("unexpected schema: %s", rec.Body.String())
		}
		return body, fields, rec
	}
	assertMetadata := func(owned bool, outcome string) {
		t.Helper()
		body, _, rec := fetch(token)
		if rec.Code != 200 || body.Protocol != "code_snapshot_cleanup_v1" || body.LeadRunID != lead.String() || !body.RowPresent ||
			body.LeadClaimGeneration != 1 || body.HeadCommit == nil || *body.HeadCommit != strings.Repeat("b", 40) ||
			body.Outcome == nil || *body.Outcome != outcome || body.LeadStatus != "completed" || body.OwnedByWorker != owned ||
			!body.ChildPresent || body.CheckerRunID == nil || *body.CheckerRunID != child.String() ||
			body.CheckerClaimGeneration != 7 || body.CheckerStatus == nil || *body.CheckerStatus != "completed" {
			t.Fatalf("metadata: %d %s", rec.Code, rec.Body.String())
		}
	}
	assertMetadata(true, "pending")
	if after := snapshot(); after != before {
		t.Fatal("read changed rows, expired evidence, or credited wait")
	}
	// Scope is the worker's user: another worker of the same user answers too, and sees the
	// lead as not owned by it. That is the foreign-lead sweep path; it needs no child here.
	if body, _, rec := fetch(foreignToken); rec.Code != 200 || body.OwnedByWorker {
		t.Fatalf("same-user worker must read the lead as foreign: %d %s", rec.Code, rec.Body.String())
	}
	for _, auth := range []string{noCapToken, otherUserToken} {
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
	// A child on another worker (or unassigned) is still reported: the LEFT JOIN must not
	// turn a checker that can still run elsewhere into "no child".
	cliMustExec(t, pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", child)
	if body, _, rec := fetch(token); rec.Code != 200 || !body.ChildPresent || body.CheckerRunID == nil || *body.CheckerRunID != child.String() {
		t.Fatalf("child without worker affinity hidden: %d %s", rec.Code, rec.Body.String())
	}
	// A never-claimed child: generation 0 and its real status, not an error.
	cliMustExec(t, pool, "UPDATE runs SET claim_generation=0,status='queued' WHERE id=$1", child)
	if body, _, rec := fetch(token); rec.Code != 200 || !body.ChildPresent || body.CheckerClaimGeneration != 0 || body.CheckerStatus == nil || *body.CheckerStatus != "queued" {
		t.Fatalf("never-claimed child: %d %s", rec.Code, rec.Body.String())
	}
	// A child whose lineage does not match the lead is not a child; the mapped id with no
	// resolvable child is a lookup failure (500), never a valid "no child".
	cliMustExec(t, pool, "UPDATE runs SET target_run_id=$2 WHERE id=$1", child, rmSeedRun(t, pool, owner, repo, "failed"))
	if rec := call(token, "?purpose=code_snapshot"); rec.Code != 500 {
		t.Fatalf("unresolvable mapped child answered as a valid state: %d %s", rec.Code, rec.Body.String())
	}
	cliMustExec(t, pool, "UPDATE runs SET target_run_id=$2 WHERE id=$1", child, lead)
	// An explicit "no child": the code row exists and maps no checker.
	cliMustExec(t, pool, "UPDATE cross_checks SET checker_run_id=NULL WHERE lead_run_id=$1 AND stage='code'", lead)
	noChild, noChildFields, rec := fetch(token)
	if rec.Code != 200 || !noChild.RowPresent || noChild.ChildPresent || noChild.CheckerRunID != nil || noChild.CheckerStatus != nil ||
		noChild.CheckerClaimGeneration != 0 || noChild.LeadClaimGeneration != 1 || noChild.HeadCommit == nil || noChildFields["checker_run_id"] != nil {
		t.Fatalf("no-child answer: %d %s", rec.Code, rec.Body.String())
	}
	cliMustExec(t, pool, "UPDATE cross_checks SET checker_run_id=$2 WHERE lead_run_id=$1 AND stage='code'", lead, child)
	// A deleted child clears the mapping and reads the same way.
	cliMustExec(t, pool, "DELETE FROM runs WHERE id=$1", child)
	if body, _, rec := fetch(token); rec.Code != 200 || !body.RowPresent || body.ChildPresent || body.CheckerRunID != nil {
		t.Fatalf("deleted child: %d %s", rec.Code, rec.Body.String())
	}
	// No code row at all (no 404): a plan-only lead and a lead with no cross-check rows.
	planLead := rmSeedRun(t, pool, owner, repo, "running")
	planChild := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,status,report_only,budget_wall_seconds,issue_title,issue_description) VALUES($1,$2,$3,$4,'cross_check',$5,'codex','running',true,1800,'plan','candidate')`, planChild, owner, repo, worker, planLead)
	cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,verdict,deadline_at,plan_md,milestones,size_class,base_commit,candidate_digest) VALUES($1,$2,'codex','plan',1,1,'pending',now()+interval '30 minutes','plan','[]','small',$3,$4)`, planLead, planChild, strings.Repeat("a", 40), make([]byte, 32))
	bareLead := rmSeedRun(t, pool, owner, repo, "failed")
	for name, id := range map[string]uuid.UUID{"plan-only lead": planLead, "lead without rows": bareLead} {
		lead = id
		body, fields, rec := fetch(token)
		if rec.Code != 200 || body.RowPresent || body.LeadRunID != id.String() || body.LeadClaimGeneration != 0 || body.HeadCommit != nil ||
			body.Outcome != nil || body.ChildPresent || body.CheckerRunID != nil || body.CheckerStatus != nil || body.CheckerClaimGeneration != 0 ||
			fields["row_present"] != false || fields["child_present"] != false {
			t.Fatalf("%s must answer an explicit no-row: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// The pre-existing lead scope still holds for no-row answers: another user's token,
	// a worker without the capability, and an unknown lead are all 404.
	for _, auth := range []string{noCapToken, otherUserToken} {
		if rec := call(auth, "?purpose=code_snapshot"); rec.Code != 404 {
			t.Fatalf("no-row answer leaked outside scope: %d", rec.Code)
		}
	}
	lead = uuid.New()
	if rec := call(token, "?purpose=code_snapshot"); rec.Code != 404 {
		t.Fatalf("unknown lead: %d", rec.Code)
	}
}
