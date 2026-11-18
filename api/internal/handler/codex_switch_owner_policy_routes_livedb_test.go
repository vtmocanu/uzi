package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

func TestWorkerCodexOwnerPolicyRouteLiveDB(t *testing.T) {
	for _, fence := range []string{"pending_fresh", "unresolved_outcome"} {
		t.Run(fence, func(t *testing.T) {
			e, w := setupStoppedGuardsRouteWithSeed(t, true)
			ctx := context.Background()
			var current, target uuid.UUID
			if err := e.pool.QueryRow(ctx, `SELECT codex_secret_id,
 codex_switch_target_secret_id FROM runs WHERE id=$1`, e.run).Scan(&current, &target); err != nil {
				t.Fatal(err)
			}
			if current == uuid.Nil || target == uuid.Nil || current == target || target == e.secret {
				t.Fatal("fixture requires distinct current A and Codex target B")
			}
			oldOp := e.op
			withdrawStoppedRoute(t, e, w)
			fresh := fence == "pending_fresh"
			outcome := uuid.New()
			// A resolved fresh episode isolates PolicyPendingFresh; a non-fresh
			// unresolved episode isolates the outcome disjunct of UnresolvedPolicyEpisode.
			stoppedRouteExec(t, e, `UPDATE runs SET
 status='running', hold_reason=NULL,
 policy_outcome_id=$2, policy_preserve_required=true,
 policy_continuation_mode='same_conversation',
 policy_applying_worker_id=worker_id,
 policy_applying_generation=claim_generation,
 policy_source_generation=claim_generation,
 policy_pending_fresh=$3,
 policy_resolved_at=CASE WHEN $3::boolean THEN now() ELSE NULL END
 WHERE id=$1`, e.run, outcome, fresh)
			var isolated bool
			if err := e.pool.QueryRow(ctx, `SELECT
 status='running' AND hold_reason IS NULL
 AND worker_id=$2 AND claim_generation=$3
 AND codex_secret_id=$4 AND codex_claim_epoch=41 AND codex_cap_hash IS NOT NULL
 AND credential_switch_requested_at IS NULL
 AND credential_switch_generation IS NULL AND credential_switch_op_id IS NULL
 AND codex_switch_target_secret_id IS NULL
 AND policy_outcome_id=$5 AND policy_preserve_required
 AND policy_continuation_mode='same_conversation'
 AND policy_applying_worker_id=worker_id
 AND policy_applying_generation=claim_generation
 AND policy_source_generation=claim_generation
 AND policy_pending_fresh=$6 AND (policy_resolved_at IS NOT NULL)=$6
 AND policy_fresh_consumed_generation IS NULL
 AND policy_fresh_confirmed_generation IS NULL
 AND policy_required_head IS NULL
 AND policy_recovery_verified_generation IS NULL
 AND policy_recovery_verified_head IS NULL
 AND policy_hold_cause IS NULL
 FROM runs WHERE id=$1`, e.run, e.worker, w.ClaimGeneration, current, outcome, fresh).Scan(&isolated); err != nil {
				t.Fatal(err)
			}
			if !isolated {
				t.Fatal("stored fixture does not isolate the intended policy fence on the current claim")
			}

			lim := mw.NewLimiter(100000, time.Minute, nil)
			router := e.handler.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
			token := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
			body := fmt.Sprintf(`{"mode":"pinned","secret_id":%q}`, target)
			before := stoppedRouteSnapshot(t, e)
			rec := bearerReqBody(router, http.MethodPost,
				"/api/runs/"+e.run.String()+"/credential", token, body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("policy-fenced owner credential request status=%d, want 409", rec.Code)
			}
			var refusal struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &refusal) != nil || !strings.Contains(refusal.Error, "(hold: policy hold)") {
				t.Fatal("owner refusal did not identify the policy hold")
			}
			after := stoppedRouteSnapshot(t, e)
			sections := []string{"run", "base", "switch", "messages", "inputs", "custody", "presentations"}
			if len(before) != len(sections) || len(after) != len(sections) {
				t.Fatal("snapshot must contain all seven sections")
			}
			for _, section := range sections {
				if !reflect.DeepEqual(before[section], after[section]) {
					t.Fatalf("rejected owner request changed snapshot section %s", section)
				}
			}

			// Resolve while retaining the complete preserving episode tuple.
			// This is a narrow availability companion, not a full policy contract.
			stoppedRouteExec(t, e, `UPDATE runs SET policy_pending_fresh=false,
 policy_resolved_at=now() WHERE id=$1`, e.run)
			before = stoppedRouteSnapshot(t, e)
			rec = bearerReqBody(router, http.MethodPost,
				"/api/runs/"+e.run.String()+"/credential", token, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("resolved preserving owner credential request status=%d, want 200", rec.Code)
			}
			var pending bool
			if err := e.pool.QueryRow(ctx, `SELECT
 credential_switch_requested_at IS NOT NULL
 AND credential_switch_generation=$2
 AND credential_switch_op_id IS NOT NULL AND credential_switch_op_id<>$3
 AND codex_switch_target_secret_id=$4
 AND codex_switch_target_generation=$2
 AND codex_switch_target_op_id=credential_switch_op_id
 AND codex_secret_id=$5 AND codex_claim_epoch=41
 FROM runs WHERE id=$1`, e.run, w.ClaimGeneration, oldOp, target, current).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if !pending {
				t.Fatal("resolved owner request did not store a fresh pending operation targeting B with binding A at epoch 41")
			}
			after = stoppedRouteSnapshot(t, e)
			var runBefore, runAfter map[string]json.RawMessage
			if err := json.Unmarshal(before["run"], &runBefore); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(after["run"], &runAfter); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{
				"worker_id", "claim_generation", "codex_secret_id", "codex_claim_epoch", "codex_cap_hash",
				"policy_outcome_id", "policy_preserve_required", "policy_continuation_mode",
				"policy_applying_worker_id", "policy_applying_generation", "policy_source_generation",
				"policy_pending_fresh", "policy_resolved_at",
			} {
				if !reflect.DeepEqual(runBefore[field], runAfter[field]) {
					t.Fatalf("resolved owner request changed authority or policy field %s", field)
				}
			}
		})
	}
}
