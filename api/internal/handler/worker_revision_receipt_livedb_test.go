package handler

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Exercise WorkerRunState's reconciled response, including the first revision
// receipt (no predecessors) and a replacement chain, through the authenticated route.
func TestWorkerReconciledRevisionReceiptLiveDB(t *testing.T) {
	for _, mode := range []string{"no receipt", "nil", "empty", "nonempty"} {
		t.Run(mode, func(t *testing.T) {
			h, router, pool := cliLiveDB(t)
			h.wsvc.SetTxBeginner(pool)
			owner := cliSeedUser(t, pool, false)
			repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 995, true)
			run := rmSeedRun(t, pool, owner, repo, "running")
			worker := uuid.New()
			token := "receipt-worker-" + uuid.NewString()
			hash := sha256.Sum256([]byte(token))
			cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'receipt',$3,'online')", worker, owner, hash[:])
			cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='codex',auto_approve=true,plan_source='agent',plan_cross_check_required=true,claim_generation=1 WHERE id=$1", run, worker)
			call := func(body map[string]any) map[string]json.RawMessage {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+"/state", strings.NewReader(string(raw)))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				var response map[string]json.RawMessage
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
					t.Fatalf("state response: %d %s", rec.Code, rec.Body.String())
				}
				if string(response["plan_cross_check_settled"]) != "true" {
					t.Fatalf("missing reconciliation proof: %s", rec.Body.String())
				}
				return response
			}
			source := map[string]any{"status": "awaiting_approval", "claim_generation": 1, "plan_md": "# Source", "milestones": []any{}, "presentation_id": uuid.NewString(), "plan_cross_check_gate_reason": "codex_lead_unsupported"}
			response := call(source)
			if mode == "no receipt" {
				for _, key := range []string{"revision_input_id", "superseded_revision_input_ids"} {
					if _, exists := response[key]; exists {
						t.Fatalf("unexpected %s: %s", key, response[key])
					}
				}
				return
			}
			input := func() int64 {
				t.Helper()
				var id int64
				err := pool.QueryRow(t.Context(), "INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision,consumed_at,consumed_worker_id,consumed_claim_generation) VALUES($1,'revise_plan','feedback','bound',1,now(),$2,1) RETURNING id", run, worker).Scan(&id)
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			var predecessors []int64
			if mode == "empty" {
				predecessors = []int64{}
			}
			if mode == "nonempty" {
				predecessors = []int64{input()}
			}
			active := input()
			source["plan_md"], source["presentation_id"] = "# Revised", uuid.NewString()
			source["expected_gate_revision"], source["revision_input_id"] = 1, active
			// An omitted field decodes to a nil result slice; [] remains nonnil.
			if mode != "nil" {
				source["superseded_revision_input_ids"] = predecessors
			}
			for attempt := 0; attempt < 2; attempt++ {
				response = call(source) // Also exercise lost-ACK replay.
				var id int64
				if json.Unmarshal(response["revision_input_id"], &id) != nil || id != active {
					t.Fatalf("missing active receipt: %s", response)
				}
				want, _ := json.Marshal(append([]int64{}, predecessors...))
				if string(response["superseded_revision_input_ids"]) != string(want) {
					t.Fatalf("predecessor receipt = %s, want %s", response["superseded_revision_input_ids"], want)
				}
			}
		})
	}
}
