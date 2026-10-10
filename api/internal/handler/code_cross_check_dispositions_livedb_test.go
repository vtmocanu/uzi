package handler

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestCodeCrossCheckDispositionsRoutesStateLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 21971, true)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	token := "dispositions-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'dispositions',$3,'online')", worker, owner, hash[:])
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='claude',code_cross_check_required=true,auto_approve=false,claim_generation=1 WHERE id=$1", lead, worker)
	child := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,status,claim_generation,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES($1,$2,$3,$4,'cross_check',$5,'codex','running',1,true,1800,'checker','candidate')`, child, owner, repo, worker, lead)
	cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,base_commit,head_commit,candidate_digest,verdict,outcome,code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,deadline_at)
 VALUES($1,$2,'codex','code',1,1,$3,$4,$5,'failed','pending','{}','','',sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),now()+interval '30 minutes')`, lead, child, strings.Repeat("a", 40), strings.Repeat("b", 40), make([]byte, 32))
	path := "/api/worker/runs/" + lead.String() + "/cross-checks/code/dispositions"
	call := func(method, path, body, auth string, streamed bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if streamed {
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}
			req.Body = io.NopCloser(strings.NewReader(body))
		}
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	batch := `{"claim_generation":1,"dispositions":[{"id":"F-1","disposition":"addressed","reason":"` + strings.Repeat("é", 512) + `"}]}`
	if rec := call("POST", path, batch, "", false); rec.Code != 401 {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	if rec := call("POST", path, batch, token, false); rec.Code != 409 {
		t.Fatalf("pending finalized: %d %s", rec.Code, rec.Body.String())
	}
	verdict := `{"claim_generation":1,"outcome":"completed","findings":[{"id":"F-1","severity":"major","title":"first"},{"id":"F_2","severity":"minor","title":"second"}]}`
	if rec := call("POST", "/api/worker/runs/"+child.String()+"/cross-check-verdict", verdict, token, false); rec.Code != 200 {
		t.Fatalf("verdict route: %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{
		`{"claim_generation":1,"dispositions":[{"id":"F-1","disposition":"addressed","reason":"fixed","extra":true}]}`,
		`{"claim_generation":1,"dispositions":[],"approval":true}`,
		`{"claim_generation":1,"dispositions":null}`,
		`{"dispositions":[]}`,
		batch + ` {}`,
	} {
		if rec := call("POST", path, body, token, true); rec.Code != 400 {
			t.Fatalf("bad fields accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, body := range []string{
		strings.Replace(batch, "F-1", " F-1", 1),
		strings.Replace(batch, "F-1", "f-1", 1),
		strings.Replace(batch, "F-1", "unknown", 1),
		strings.Replace(batch, strings.Repeat("é", 512), strings.Repeat("é", 512)+"x", 1),
		`{"claim_generation":1,"dispositions":[{"id":"F-1","disposition":"addressed","reason":"fixed"},{"id":"F-1","disposition":"declined","reason":"no"}]}`,
		strings.Replace(batch, `"claim_generation":1`, `"claim_generation":2`, 1),
	} {
		if rec := call("POST", path, body, token, true); rec.Code != 409 {
			t.Fatalf("invalid batch accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, id := range []string{"", strings.Repeat("x", 65), "white space", "new\nline", "ansi\x1b[31m", "control\x00", "bidi\u202e", "unknown"} {
		body, err := json.Marshal(map[string]any{"claim_generation": 1, "dispositions": []map[string]string{{"id": id, "disposition": "declined", "reason": "verified"}}})
		if err != nil {
			t.Fatal(err)
		}
		if rec := call("POST", path, string(body), token, true); rec.Code != 409 {
			t.Fatalf("invalid ID %q: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if rec := call("POST", path, batch+strings.Repeat(" ", 128<<10), token, true); rec.Code != 413 {
		t.Fatalf("streamed body cap: %d %s", rec.Code, rec.Body.String())
	}
	tooMany := make([]map[string]string, 21)
	for i := range tooMany {
		tooMany[i] = map[string]string{"id": "F-1", "disposition": "addressed", "reason": "fixed"}
	}
	oversized, _ := json.Marshal(map[string]any{"claim_generation": 1, "dispositions": tooMany})
	if rec := call("POST", path, string(oversized), token, true); rec.Code != 400 {
		t.Fatal("count cap ignored")
	}
	cc, err := h.q.GetCodeCrossCheck(t.Context(), lead)
	if err != nil || cc.FinalizedAt.Valid || cc.Dispositions != nil {
		t.Fatal("refused requests wrote state")
	}
	first := call("POST", path, batch, token, true)
	if first.Code != 200 {
		t.Fatalf("batch route: %d %s", first.Code, first.Body.String())
	}
	var got apitypes.CodeCrossCheck
	if err := json.Unmarshal(first.Body.Bytes(), &got); err != nil || got.FinalizedAt == nil || got.Outcome != "completed" {
		t.Fatalf("finalized response: %+v %v", got, err)
	}
	var ds []struct{ ID, Disposition, Reason string }
	if err := json.Unmarshal(got.Dispositions, &ds); err != nil || len(ds) != 2 || ds[0].ID != "F-1" || ds[0].Disposition != "addressed" || ds[1].ID != "F_2" || ds[1].Disposition != "not_reported" || ds[1].Reason != "" {
		t.Fatalf("persisted missing disposition: %s %v", got.Dispositions, err)
	}
	retry := call("POST", path, batch, token, true)
	if retry.Code != 200 || retry.Body.String() != first.Body.String() {
		t.Fatal("lost ACK retry changed durable evidence")
	}
	if rec := call("POST", path, strings.Replace(batch, strings.Repeat("é", 512), "different", 1), token, false); rec.Code != 409 {
		t.Fatal("conflicting retry accepted")
	}
	statusPath := "/api/worker/runs/" + lead.String() + "/cross-checks/code/latest?claim_generation=1"
	status := call("GET", statusPath, "", token, false)
	if status.Code != 200 || status.Body.String() != first.Body.String() {
		t.Fatalf("status omitted persisted dispositions: %d %s", status.Code, status.Body.String())
	}
	cc, err = h.q.GetCodeCrossCheck(t.Context(), lead)
	if err != nil || !cc.FinalizedAt.Valid || len(cc.Dispositions) == 0 {
		t.Fatalf("finalized evidence unavailable: %v", err)
	}
	var state string
	var approval bool
	if err := pool.QueryRow(t.Context(), "SELECT status,auto_approve FROM runs WHERE id=$1", lead).Scan(&state, &approval); err != nil || state != "running" || approval {
		t.Fatal("advice approved or vetoed lead")
	}
	// An interrupted no-snapshot failure must retain the paired-NULL allowlist
	// reason in the worker DTO, while the owner still sees the persisted record.
	noSHA := uuid.New()
	cliMustExec(t, pool, "INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind) VALUES($1,$2,$3,8,'Do X','desc','running','issue')", noSHA, owner, repo)
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='claude',code_cross_check_required=true,claim_generation=1 WHERE id=$1", noSHA, worker)
	if rec := call("POST", "/api/worker/runs/"+noSHA.String()+"/cross-checks", `{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed"}`, token, false); rec.Code != 200 {
		t.Fatalf("no snapshot failure: %d %s", rec.Code, rec.Body.String())
	}
	cliMustExec(t, pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", noSHA)
	noSnapshot := call("GET", "/api/worker/runs/"+noSHA.String()+"/cross-checks/code/latest?claim_generation=2", "", token, false)
	var noSnapshotDTO apitypes.CodeCrossCheck
	if noSnapshot.Code != 200 || json.Unmarshal(noSnapshot.Body.Bytes(), &noSnapshotDTO) != nil ||
		noSnapshotDTO.HeadCommit != nil || noSnapshotDTO.BaseCommit != nil || noSnapshotDTO.InterruptedAt == nil ||
		noSnapshotDTO.ReasonClass == nil || *noSnapshotDTO.ReasonClass != "snapshot_failed" ||
		string(noSnapshotDTO.Findings) != "[]" || string(noSnapshotDTO.Dispositions) != "null" {
		t.Fatalf("paired NULL reason lost: %d %s", noSnapshot.Code, noSnapshot.Body.String())
	}
	cliMustExec(t, pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", lead)
	for _, body := range []string{batch, strings.Replace(batch, `"claim_generation":1`, `"claim_generation":2`, 1)} {
		if rec := call("POST", path, body, token, false); rec.Code != 409 {
			t.Fatal("retry accepted after generation changed")
		}
	}
	interrupted := call("GET", strings.Replace(statusPath, "generation=1", "generation=2", 1), "", token, false)
	if interrupted.Code != 200 || json.Unmarshal(interrupted.Body.Bytes(), &got) != nil || got.FinalizedAt == nil || got.InterruptedAt == nil || got.ReasonClass == nil || *got.ReasonClass != "superseded" || string(got.Findings) != "[]" || string(got.Dispositions) != "null" {
		t.Fatalf("interrupted projection: %d %s", interrupted.Code, interrupted.Body.String())
	}
	run, err := h.q.GetRunByID(t.Context(), lead)
	if err != nil {
		t.Fatal(err)
	}
	var ownerDetail apitypes.RunDTO
	h.overlayPlanCrossCheckSummary(t.Context(), owner, run, &ownerDetail)
	if summary := ownerDetail.CodeCrossCheckSummary; summary == nil || summary.InterruptedAt == nil ||
		summary.FinalizedAt == nil || len(summary.Findings) == 0 || string(summary.Findings) == "[]" ||
		string(summary.Dispositions) != string(cc.Dispositions) {
		t.Fatalf("owner lost interrupted evidence: %+v", summary)
	}
}
