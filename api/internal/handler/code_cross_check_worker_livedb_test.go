package handler

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestCodeCrossCheckStageDiscriminationLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 21970, true)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	token := "code-contract-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,max_cross_check_slots,protocol_capabilities) VALUES($1,$2,'code-contract',$3,'online',1,ARRAY['cross_check_code_v1','cross_check_lane_v1','codex_harness_v1'])", worker, owner, hash[:])
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='claude',auto_approve=false,code_cross_check_required=true,claim_generation=1 WHERE id=$1", lead, worker)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	path := "/api/worker/runs/" + lead.String() + "/cross-checks"
	for _, body := range []string{
		`{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed","head_commit":"abc"}`,
		`{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed","plan_md":"plan"}`,
		`{"stage":"plan","claim_generation":1,"reason_class":"snapshot_failed"}`,
		`{"stage":"code","claim_generation":1,"base_commit":"` + strings.Repeat("A", 40) + `","head_commit":"` + strings.Repeat("b", 40) + `"}`,
	} {
		rec := call(http.MethodPost, path, body)
		if rec.Code != 400 && rec.Code != 409 {
			t.Fatalf("invalid stage payload accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := call(http.MethodPost, path, `{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed"}`)
	var got apitypes.CodeCrossCheck
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Outcome != "failed" ||
		got.HeadCommit != nil || got.BaseCommit != nil || got.CheckerRunID != nil {
		t.Fatalf("pre-snapshot failure: %d %s", rec.Code, rec.Body.String())
	}
	retry := call(http.MethodPost, path, `{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed"}`)
	if retry.Code != 200 || retry.Body.String() != rec.Body.String() {
		t.Fatal("failure retry changed evidence")
	}
	// A second lead exercises stage-discriminated verdicts through the registered
	// production route. The admission and actual lane claim live in workersvc.
	other := uuid.New()
	cliMustExec(t, pool, "INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind) VALUES($1,$2,$3,8,'Do X','desc','running','issue')", other, owner, repo)
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='claude',auto_approve=false,code_cross_check_required=true,claim_generation=1 WHERE id=$1", other, worker)
	child := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,status,claim_generation,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES($1,$2,$3,$4,'cross_check',$5,'codex','running',1,true,1800,'contract','candidate')`, child, owner, repo, worker, other)
	cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,base_commit,head_commit,candidate_digest,verdict,outcome,code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,deadline_at)
 VALUES($1,$2,'codex','code',1,1,$3,$4,$5,'failed','pending','{}','','',sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),now()+interval '30 minutes')`, other, child, strings.Repeat("a", 40), strings.Repeat("b", 40), make([]byte, 32))
	verdict := "/api/worker/runs/" + child.String() + "/cross-check-verdict"
	for _, body := range []string{
		`{"claim_generation":1,"verdict":"approve","reason_class":"approve","items":[]}`,
		`{"claim_generation":1,"outcome":"completed","verdict":""}`,
		`{"claim_generation":1,"outcome":"block","findings":[]}`,
		`{"claim_generation":1,"outcome":"completed","findings":[{"id":"bad id","severity":"major"}]}`,
		`{"claim_generation":2,"outcome":"completed","findings":[]}`,
	} {
		rec := call(http.MethodPost, verdict, body)
		if rec.Code != 400 && rec.Code != 409 {
			t.Fatalf("invalid code verdict accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, id := range []string{"", strings.Repeat("x", 65), "white space", "new\nline", "ansi\x1b[31m", "control\x00", "bidi\u202e"} {
		body, err := json.Marshal(map[string]any{"claim_generation": 1, "outcome": "completed", "findings": []map[string]any{{"id": id, "severity": "major"}}})
		if err != nil {
			t.Fatal(err)
		}
		refused := call(http.MethodPost, verdict, string(body))
		if refused.Code != 400 && refused.Code != 409 {
			t.Fatalf("invalid stored identifier accepted: %q status=%d", id, refused.Code)
		}
	}
	duplicate := call(http.MethodPost, verdict, `{"claim_generation":1,"outcome":"completed","findings":[{"id":"same","severity":"major"},{"id":"same","severity":"minor"}]}`)
	if duplicate.Code != 400 && duplicate.Code != 409 {
		t.Fatal("duplicate stored IDs accepted")
	}
	accepted := call(http.MethodPost, verdict, `{"claim_generation":1,"outcome":"completed","findings":[]}`)
	if accepted.Code != 200 {
		t.Fatalf("completed code route: %d %s", accepted.Code, accepted.Body.String())
	}
	var status string
	var settled bool
	if err := pool.QueryRow(t.Context(), "SELECT status,auto_approve FROM runs WHERE id=$1", other).Scan(&status, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "running" || settled {
		t.Fatal("code route approved or parked lead")
	}
	checked, err := h.q.GetRunByID(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	var noUsage apitypes.RunDTO
	h.overlayPlanCrossCheckSummary(t.Context(), owner, checked, &noUsage)
	if noUsage.CodeCrossCheckSummary == nil || noUsage.CodeCrossCheckSummary.Usage != nil {
		t.Fatal("missing child usage invented a cost")
	}
	cliMustExec(t, pool, `INSERT INTO run_usage(run_id,model,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens,cost_usd,harness,cost_status) VALUES($1,'code-cost',10,20,30,40,1.25,'codex','metered')`, child)
	var withUsage apitypes.RunDTO
	h.overlayPlanCrossCheckSummary(t.Context(), owner, checked, &withUsage)
	if withUsage.CodeCrossCheckSummary == nil || withUsage.CodeCrossCheckSummary.Usage == nil ||
		withUsage.CodeCrossCheckSummary.Usage.CostUSD != 1.25 || withUsage.CodeCrossCheckSummary.Usage.CostStatus != "metered" ||
		withUsage.CodeCrossCheckSummary.Usage.InputTokens != 10 {
		t.Fatalf("recorded checker usage missing: %+v", withUsage.CodeCrossCheckSummary)
	}
	var nonOwnerUsage apitypes.RunDTO
	h.overlayPlanCrossCheckSummary(t.Context(), uuid.New(), checked, &nonOwnerUsage)
	if nonOwnerUsage.CodeCrossCheckSummary != nil {
		t.Fatal("checker usage exposed to non-owner")
	}
	run, err := h.q.GetRunByID(t.Context(), lead)
	if err != nil {
		t.Fatal(err)
	}
	var detail apitypes.RunDTO
	h.overlayPlanCrossCheckSummary(t.Context(), owner, run, &detail)
	if detail.CodeCrossCheckSummary == nil || detail.CodeCrossCheckSummary.HeadCommit != nil ||
		detail.CodeCrossCheckSummary.ReasonClass == nil || *detail.CodeCrossCheckSummary.ReasonClass != "snapshot_failed" {
		t.Fatalf("owner pre-snapshot projection: %+v", detail.CodeCrossCheckSummary)
	}
	var otherViewer apitypes.RunDTO
	h.overlayPlanCrossCheckSummary(t.Context(), uuid.New(), run, &otherViewer)
	if otherViewer.CodeCrossCheckSummary != nil {
		t.Fatal("code findings exposed to non-owner")
	}
}
