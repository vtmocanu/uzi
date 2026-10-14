package handler

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2870: every code cross-check row the worker can read serializes findings as an array, never
// null, whichever store path left the column NULL. The worker decoder rejects null, and the lead then
// stops waiting for its checker.
func TestCodeCrossCheckFindingsNeverNullLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)

	worker := uuid.New()
	token := "findings-worker-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,max_cross_check_slots,protocol_capabilities) VALUES($1,$2,'findings',$3,'online',1,ARRAY['cross_check_code_v1','cross_check_lane_v1','codex_harness_v1'])", worker, owner, hash[:])
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	conn := rmSeedConn(t, pool, owner)
	project := int64(28700)
	newLead := func() (uuid.UUID, uuid.UUID) {
		project++
		repo := rmSeedRepo(t, pool, conn, project, true)
		lead := rmSeedRun(t, pool, owner, repo, "running")
		cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='claude',code_cross_check_required=true,claim_generation=1 WHERE id=$1", lead, worker)
		return lead, repo
	}
	// The pending row exactly as InsertCodeCrossCheck leaves it: findings omitted, so NULL.
	pendingRow := func(lead, repo uuid.UUID, childStatus, deadline string) {
		child := uuid.New()
		cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,status,claim_generation,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES($1,$2,$3,$4,'cross_check',$5,'codex',$6,1,true,1800,'checker','candidate')`, child, owner, repo, worker, lead, childStatus)
		cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,base_commit,head_commit,candidate_digest,verdict,outcome,code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,deadline_at)
 VALUES($1,$2,'codex','code',1,1,$3,$4,$5,'failed','pending','{}','','',sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),now()+$6::interval)`, lead, child, strings.Repeat("a", 40), strings.Repeat("b", 40), make([]byte, 32), deadline)
	}
	assertArray := func(t *testing.T, name string, rec *httptest.ResponseRecorder, outcome, reason string) {
		t.Helper()
		var body struct {
			Outcome     string          `json:"outcome"`
			ReasonClass *string         `json:"reason_class"`
			Findings    json.RawMessage `json:"findings"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if string(body.Findings) != "[]" || body.Outcome != outcome || (reason == "") != (body.ReasonClass == nil) || (body.ReasonClass != nil && *body.ReasonClass != reason) {
			t.Fatalf("%s: want outcome=%s reason=%q findings=[]; got %s", name, outcome, reason, rec.Body.String())
		}
	}
	status := func(lead uuid.UUID) *httptest.ResponseRecorder {
		return call(http.MethodGet, "/api/worker/runs/"+lead.String()+"/cross-checks/code/latest?claim_generation=1", "")
	}
	storedNull := func(t *testing.T, name string, lead uuid.UUID) {
		t.Helper()
		var isNull bool
		if err := pool.QueryRow(t.Context(), "SELECT findings IS NULL FROM cross_checks WHERE lead_run_id=$1 AND stage='code'", lead).Scan(&isNull); err != nil || !isNull {
			t.Fatalf("%s: precondition: stored findings must stay NULL (normalization is response-side), null=%t err=%v", name, isNull, err)
		}
	}

	t.Run("pending submit", func(t *testing.T) {
		// A usable opposite-harness credential admits a real child instead of a failed row.
		sealed := mustSealT(t.Context(), t, h.box, []byte("pending-submit-fixture"))
		secret := uuid.New()
		cliMustExec(t, pool, `INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with)
 VALUES($1,$2,'openai_api_key','pending-submit',true,$3,'master')`, secret, owner, sealed)
		if _, err := h.q.InsertCodexCredentialState(t.Context(), store.InsertCodexCredentialStateParams{UserSecretID: secret, UserID: owner, Status: "static"}); err != nil {
			t.Fatal(err)
		}
		lead, _ := newLead()
		body := `{"stage":"code","claim_generation":1,"base_commit":"` + strings.Repeat("a", 40) + `","head_commit":"` + strings.Repeat("b", 40) + `"}`
		submitted := call(http.MethodPost, "/api/worker/runs/"+lead.String()+"/cross-checks", body)
		assertArray(t, "pending submit", submitted, "pending", "")
		storedNull(t, "pending submit", lead)
		latest := status(lead)
		assertArray(t, "pending submit latest", latest, "pending", "")
		if submitted.Body.String() != latest.Body.String() {
			t.Fatalf("pending submit and latest disagree: submit=%s latest=%s", submitted.Body.String(), latest.Body.String())
		}
	})
	t.Run("pending", func(t *testing.T) {
		lead, repo := newLead()
		pendingRow(lead, repo, "running", "30 minutes")
		assertArray(t, "pending status", status(lead), "pending", "")
		storedNull(t, "pending", lead)
	})
	t.Run("pre-snapshot failure", func(t *testing.T) {
		lead, _ := newLead()
		rec := call(http.MethodPost, "/api/worker/runs/"+lead.String()+"/cross-checks", `{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed"}`)
		assertArray(t, "failure submit", rec, "failed", "snapshot_failed")
		assertArray(t, "failure status", status(lead), "failed", "snapshot_failed")
		storedNull(t, "pre-snapshot failure", lead)
	})
	t.Run("expired", func(t *testing.T) {
		lead, repo := newLead()
		pendingRow(lead, repo, "running", "-1 minute")
		assertArray(t, "expired status", status(lead), "failed", "timed_out")
		storedNull(t, "expired", lead)
	})
	t.Run("dead checker", func(t *testing.T) {
		lead, repo := newLead()
		pendingRow(lead, repo, "failed", "30 minutes")
		assertArray(t, "dead-child status", status(lead), "failed", "checker_unavailable")
		storedNull(t, "dead checker", lead)
	})
}
