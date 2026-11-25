package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4554: through the real worker router, a plan cross-check candidate whose milestone
// list is not exactly [{id,title}] objects is answered 400 candidate_invalid and creates no
// cross_checks row and no checker child; a valid candidate on the same lead is admitted.
func TestPlanCrossCheckInvalidMilestonesRouterLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()
	h.wsvc = workersvc.New(h.q, h.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), PlanCrossCheckMaxRevisions: 2})
	h.wsvc.SetVault(h.vault)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 45540, true)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	token := "milestone-admission-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	worker := uuid.New()
	caps := []string{"cross_check_v1", "cross_check_rounds_v1", "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "completion_interlock_v1", "codex_completion_interlock_v1", capability.CrossCheckOwnerSteeringV1}
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities,max_concurrent_runs) VALUES($1,$2,'admission',$3,'online',$4,8)`, worker, owner, hash[:], caps)
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2,harness='claude',auto_approve=true,plan_cross_check_required=true,claim_generation=1 WHERE id=$1`, lead, worker)
	sealed := mustSealT(ctx, t, h.box, []byte("milestone-admission-fixture-"+uuid.NewString()))
	secret := uuid.New()
	cliMustExec(t, pool, `UPDATE forge_connections SET token_ciphertext=$2 WHERE id=$1`, conn, sealed)
	cliMustExec(t, pool, `INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'openai_api_key','admission',true,$3,'master')`, secret, owner, sealed)
	if _, err := h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret, UserID: owner, Status: "static"}); err != nil {
		t.Fatal(err)
	}
	counts := func() (rows, children int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
			(SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check')`, lead).Scan(&rows, &children); err != nil {
			t.Fatal(err)
		}
		return rows, children
	}
	submit := func(milestones string) *httptest.ResponseRecorder {
		t.Helper()
		candidate := workersvc.PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage(milestones), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
		round, gen := int32(1), int64(1)
		body, err := json.Marshal(planCrossCheckRequest{Stage: "plan", Round: &round, ClaimGeneration: &gen, PlanCrossCheckCandidate: candidate})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+lead.String()+"/cross-checks", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	for _, invalid := range []string{`[{"id":"","title":""}]`, `[{"id":"m1","title":"t","extra":1}]`, `[{"id":"m1"}]`} {
		rec := submit(invalid)
		var result struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusBadRequest || result.Reason != "candidate_invalid" {
			t.Fatalf("%s: %d %s", invalid, rec.Code, rec.Body.String())
		}
		if rows, children := counts(); rows != 0 || children != 0 {
			t.Fatalf("%s created rows=%d children=%d", invalid, rows, children)
		}
	}
	// Control: the same lead and router admit a valid list, so the refusals above are the milestones.
	if rec := submit(`[{"id":"m1","title":"First"}]`); rec.Code != http.StatusOK {
		t.Fatalf("valid control: %d %s", rec.Code, rec.Body.String())
	}
	if rows, children := counts(); rows != 1 || children != 1 {
		t.Fatalf("valid control rows=%d children=%d", rows, children)
	}
}
