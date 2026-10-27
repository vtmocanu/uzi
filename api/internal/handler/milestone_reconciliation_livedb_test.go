package handler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestMilestoneReconciliationHTTPReclaimLiveDB exercises the owner and worker
// routers against Postgres. The legacy completion contract keeps this test
// focused on transport; workersvc LiveDB tests cover interlocked contracts.
func TestMilestoneReconciliationHTTPReclaimLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), PlanMaxRevisions: 3, WorkerHeartbeatStale: time.Minute})
	svc.SetVault(vault.NewPostgres(box, pool))
	svc.SetTxBeginner(pool)
	svc.SetRetentionLockPool(pool)
	svc.SetBackground(func(func()) {})
	svc.SetForgeBaseURLAllowed(func(url string) bool { return url == "https://forge.e2e" })

	owner, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	issueIID := int64(binary.BigEndian.Uint64(runID[:8]) >> 1)
	const checkpoint = "2222222222222222222222222222222222222222"
	const sourceHead = "1111111111111111111111111111111111111111"
	const oldList = `[{"id":"M1","title":"Old one"},{"id":"M2","title":"Old two"},{"id":"M3","title":"Old three"},{"id":"M4","title":"Old four"}]`
	const newList = `[{"id":"M1","title":"Replacement one"},{"id":"M2","title":"Replacement two"}]`
	seal := func(value string) []byte {
		t.Helper()
		ciphertext, err := box.Seal([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return ciphertext
	}
	const toyPAT = "toy-forge-credential"
	svc.SetListRefTipsFn(func(_ context.Context, opts pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
		wantRef := fmt.Sprintf("refs/uzi-checkpoints/agent/issue-%d", issueIID)
		if opts.PAT != toyPAT || len(refs) != 1 || refs[0] != wantRef {
			return nil, fmt.Errorf("unexpected checkpoint lookup")
		}
		return map[string]string{wantRef: checkpoint}, nil
	})
	exec := func(sql string, args ...any) {
		t.Helper()
		cliMustExec(t, pool, sql, args...)
	}
	exec(`INSERT INTO users(id,email,password_hash,vault_mode) VALUES($1,$2,'x','unprotected')`, owner, fmt.Sprintf("reconciliation-http-%s@e2e", owner))
	t.Cleanup(func() {
		// Delete only this fixture's rows, in FK order.
		exec("DELETE FROM runs WHERE id=$1", runID)
		exec("DELETE FROM users WHERE id=$1", owner)
	})
	exec(`INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://forge.e2e','bot',1,$3)`, connID, owner, seal(toyPAT))
	exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES($1,$2,1,$3,'https://forge.e2e/g/r','main',true)`, repoID, connID, "g/reconciliation-http-"+repoID.String())
	exec(`INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with)
 VALUES($1,$2,'anthropic_token','toy model credential',true,$3,'master')`, uuid.New(), owner, seal("toy-model-credential"))
	workerToken, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at,capabilities,protocol_capabilities)
 VALUES($1,$2,$3,$4,'online',now(),$5,$6)`, workerID, owner, "reconciliation-http-"+workerID.String(), hash,
		[]string{capability.Docker}, []string{capability.CredentialSwitchV1, capability.CompletionInterlockV1, capability.GateRevisionV1, capability.MilestoneReconciliationClaimV1})
	exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,
 claim_generation,auto_approve,plan_source,session_id,milestones_frozen,milestones_completed,
 milestones_in_progress,milestones_agents,scope_ceiling,stop_kind,stop_reason,checkpoint_tip,checkpoint_tip_at)
 VALUES($1,$2,$3,'issue',$4,'test','test','running',$5,1,false,'agent','toy-session',$6,
 '["M1","M2","M3","M4"]','["M4"]','[{"milestone_id":"M4","agent":"old-agent"}]',4,'scope_capped','old scope',$7,now())`,
		runID, owner, repoID, issueIID, workerID, []byte(oldList), checkpoint)

	h := &Handler{pool: pool, q: q, box: box, wsvc: svc, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	ownerRouter := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	workerRouter := h.WorkerRoutes(lim)
	ownerToken := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	call := func(router http.Handler, token, method, path, body string, want int) map[string]any {
		t.Helper()
		rec := bearerReqBody(router, method, path, token, body)
		if rec.Code != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return out
	}
	ownerPath := "/api/runs/" + runID.String()
	workerPath := "/api/worker/runs/" + runID.String()
	gate := fmt.Sprintf(`{"status":"awaiting_approval","claim_generation":1,"presentation_id":%q,
 "plan_md":"replacement plan","milestones":%s,"required_capabilities":["docker"],
 "gate_checkpoint_witness":{"source_head":%q,"published_checkpoint":%q}}`, uuid.NewString(), newList, sourceHead, checkpoint)
	ack := call(workerRouter, workerToken, http.MethodPost, workerPath+"/state", gate, http.StatusOK)
	if ack["gate_revision"] != float64(1) {
		t.Fatalf("gate ACK lacks allocated revision: %v", ack)
	}
	detail := call(ownerRouter, ownerToken, http.MethodGet, ownerPath, "", http.StatusOK)["run"].(map[string]any)
	digest, _ := detail["reset_state_digest"].(string)
	if digest == "" {
		t.Fatal("owner detail lacks reset_state_digest")
	}
	refused := call(ownerRouter, ownerToken, http.MethodPost, ownerPath+"/inputs",
		`{"kind":"approve_plan","expected_gate_revision":1}`, http.StatusConflict)
	if refused["reason"] != "milestone_reconciliation_required" {
		t.Fatalf("ordinary approval refusal: %v", refused)
	}
	before, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.MilestonesCompleted) == "[]" || before.Status != "awaiting_approval" {
		t.Fatal("ordinary approval changed old progress or gate")
	}
	reset := fmt.Sprintf(`{"kind":"approve_plan","expected_gate_revision":1,"expected_reset_state_digest":%q,
 "reconciliation":{"mode":"reset","reason":"replace completed milestones","expected_completion_revision":null}}`, digest)
	call(ownerRouter, ownerToken, http.MethodPost, ownerPath+"/inputs", reset, http.StatusAccepted)
	assertReset := func(status string, generation int64, released bool) {
		t.Helper()
		run, err := q.GetRunByID(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != status || run.ClaimGeneration != generation || run.ClaimReleasedAt.Valid != released {
			t.Fatalf("reset ownership: status=%s generation=%d released=%v", run.Status, run.ClaimGeneration, run.ClaimReleasedAt.Valid)
		}
		for _, raw := range [][]byte{run.MilestonesCompleted, run.MilestonesInProgress, run.MilestonesAgents} {
			var items []any
			if err := json.Unmarshal(raw, &items); err != nil || items == nil || len(items) != 0 {
				t.Fatalf("old progress survived: %s (decode %v)", raw, err)
			}
		}
		var frozen, expected any
		if err := json.Unmarshal(run.MilestonesFrozen, &frozen); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(newList), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(frozen, expected) || run.ScopeCeiling.Valid || run.StopKind.Valid || run.StopReason.Valid {
			t.Fatal("replacement definitions or cleared scope differ")
		}
		if !run.CheckpointTip.Valid || run.CheckpointTip.String != checkpoint || run.ContractRevision.Valid {
			t.Fatal("legacy contract or checkpoint changed")
		}
	}
	assertReset("queued", 1, true)
	assertReplay := func() {
		t.Helper()
		call(ownerRouter, ownerToken, http.MethodPost, ownerPath+"/inputs", reset, http.StatusAccepted)
		var approvals, audits int
		if err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'),
 (SELECT count(*) FROM run_milestone_reconciliations WHERE run_id=$1)`, runID).Scan(&approvals, &audits); err != nil {
			t.Fatal(err)
		}
		if approvals != 1 || audits != 1 {
			t.Fatalf("replay duplicated effects: approvals=%d audits=%d", approvals, audits)
		}
	}
	assertReplay()
	assertReset("queued", 1, true)
	assertFenced := func() {
		t.Helper()
		for _, stamp := range []string{"", `,"claim_generation":1`} {
			out := call(workerRouter, workerToken, http.MethodPost, workerPath+"/state",
				`{"status":"running","milestones_completed":["M1","M2"],"milestones_in_progress":["M2"]`+stamp+"}", http.StatusConflict)
			if out["disposition"] != "stale_claim" {
				t.Fatalf("state generation fence: %v", out)
			}
		}
		for _, stamp := range []string{"", `,"claim_generation":0`, `,"claim_generation":1`, `,"claim_generation":99`} {
			call(workerRouter, workerToken, http.MethodPost, workerPath+"/memory",
				`{"title":"old","body":"old memory"`+stamp+"}", http.StatusConflict)
		}
		var memories int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_memory WHERE run_id=$1", runID).Scan(&memories); err != nil || memories != 0 {
			t.Fatalf("stale HTTP memory effects: %d %v", memories, err)
		}
		for _, query := range []string{"", "?claim_generation=1"} {
			out := call(workerRouter, workerToken, http.MethodGet, workerPath+"/inputs"+query, "", http.StatusConflict)
			if out["reason"] != "stale_claim" {
				t.Fatalf("poll generation fence: %v", out)
			}
		}
	}
	assertFenced()
	assertReset("queued", 1, true)
	claim := call(workerRouter, workerToken, http.MethodPost, "/api/worker/runs/claim", "", http.StatusOK)
	if claim["run_id"] != runID.String() || claim["claim_generation"] != float64(2) ||
		claim["plan_approved"] != true || claim["resume_phase"] != "implementing" {
		t.Fatalf("claim did not resume approved replacement: run=%v generation=%v approved=%v phase=%v",
			claim["run_id"], claim["claim_generation"], claim["plan_approved"], claim["resume_phase"])
	}
	var expected any
	if err := json.Unmarshal([]byte(newList), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(claim["execution_milestones"], expected) {
		t.Fatalf("claim execution definitions = %v", claim["execution_milestones"])
	}
	assertFenced()
	assertReset("claimed", 2, false)
	assertReplay()
	assertReset("claimed", 2, false)
	call(workerRouter, workerToken, http.MethodPost, workerPath+"/memory",
		`{"title":"current","body":"current memory","claim_generation":2}`, http.StatusCreated)
}
