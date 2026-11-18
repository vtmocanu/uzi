package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// A policy episode that starts after the owner request is stored but before the
// worker's (next) preflight must be refused by preflight itself, never READY,
// and apply, which repeats validation, must refuse too. The shared setup helper
// obtains one earlier READY (a rolled-back, stateless decision); the episode is
// installed after it and the real preflight route is called again.
func TestWorkerCodexPolicyEpisodeBeforePreflightRouteLiveDB(t *testing.T) {
	for _, fence := range []string{"pending_fresh", "unresolved_outcome"} {
		t.Run(fence, func(t *testing.T) {
			e, w := setupStoppedGuardsRouteWithSeed(t, true)
			ctx := context.Background()
			var target uuid.UUID
			if err := e.pool.QueryRow(ctx, `SELECT codex_switch_target_secret_id FROM runs WHERE id=$1`,
				e.run).Scan(&target); err != nil {
				t.Fatal(err)
			}
			if target == uuid.Nil {
				t.Fatal("fixture requires a pending target B")
			}
			fresh := fence == "pending_fresh"
			stoppedRouteExec(t, e, `UPDATE runs SET
 status='running', hold_reason=NULL,
 policy_outcome_id=$2, policy_preserve_required=true,
 policy_continuation_mode='same_conversation',
 policy_applying_worker_id=worker_id,
 policy_applying_generation=claim_generation,
 policy_source_generation=claim_generation,
 policy_pending_fresh=$3,
 policy_resolved_at=CASE WHEN $3::boolean THEN now() ELSE NULL END
 WHERE id=$1`, e.run, uuid.New(), fresh)

			before := stoppedRouteSnapshot(t, e)
			pre := fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q,"target_secret_id":%q}`,
				w.ClaimGeneration, w.RegisterNonce, e.op, target)
			rec := e.do(e.worker, http.MethodPost,
				"/api/worker/runs/"+e.run.String()+"/credential-switch/codex/preflight", pre)
			if rec.Code != http.StatusOK {
				t.Fatalf("policy-fenced preflight status=%d, want 200", rec.Code)
			}
			var wire map[string]json.RawMessage
			if json.Unmarshal(rec.Body.Bytes(), &wire) != nil {
				t.Fatal("preflight response is not a JSON object")
			}
			var outcome, reason string
			// Check the decision first so a bypass fails on READY, not on shape.
			if json.Unmarshal(wire["outcome"], &outcome) != nil || outcome != "not_applied" {
				t.Fatalf("policy-fenced preflight outcome=%.64q, want not_applied", outcome)
			}
			if json.Unmarshal(wire["reason"], &reason) != nil || reason != "policy_refusal" {
				t.Fatal("policy-fenced preflight did not return policy_refusal")
			}
			if _, ok := wire["expected_epoch"]; ok {
				t.Fatal("refused preflight must not carry READY authority")
			}
			var gen int64
			var nonce, op string
			if json.Unmarshal(wire["claim_generation"], &gen) != nil || gen != w.ClaimGeneration ||
				json.Unmarshal(wire["register_nonce"], &nonce) != nil || nonce != w.RegisterNonce ||
				json.Unmarshal(wire["op_id"], &op) != nil || op != e.op.String() {
				t.Fatal("refused preflight must echo the current claim, nonce and operation")
			}
			if len(wire) != 5 {
				t.Fatal("refused preflight must contain exactly outcome, reason, claim_generation, register_nonce and op_id")
			}

			rec = e.do(e.worker, http.MethodPost, e.applyPath(), fmt.Sprintf(
				`{"claim_generation":%d,"register_nonce":%q,"op_id":%q}`, w.ClaimGeneration, w.RegisterNonce, e.op))
			if rec.Code != http.StatusOK {
				t.Fatalf("apply after refused preflight status=%d, want 200", rec.Code)
			}
			var applyWire map[string]json.RawMessage
			if json.Unmarshal(rec.Body.Bytes(), &applyWire) != nil ||
				json.Unmarshal(applyWire["outcome"], &outcome) != nil || outcome != "not_applied" ||
				json.Unmarshal(applyWire["reason"], &reason) != nil || reason != "policy_refusal" {
				t.Fatal("apply after refused preflight did not return not_applied/policy_refusal")
			}

			after := stoppedRouteSnapshot(t, e)
			sections := []string{"run", "base", "switch", "messages", "inputs", "custody", "presentations"}
			if len(before) != len(sections) || len(after) != len(sections) {
				t.Fatal("snapshot must contain all seven sections")
			}
			for _, section := range sections {
				b, bok := before[section]
				a, aok := after[section]
				if !bok || !aok || !reflect.DeepEqual(b, a) {
					t.Fatalf("refused preflight or apply changed snapshot section %s", section)
				}
			}
		})
	}
}

func TestWorkerCodexApplyPolicyAfterPreflightRouteLiveDB(t *testing.T) {
	for _, fence := range []string{"pending_fresh", "unresolved_outcome"} {
		t.Run(fence, func(t *testing.T) {
			e, w := setupStoppedGuardsRouteWithSeed(t, true)
			ctx := context.Background()
			var current, target uuid.UUID
			var oldHash []byte
			if err := e.pool.QueryRow(ctx, `SELECT codex_secret_id,
 codex_switch_target_secret_id,codex_cap_hash FROM runs WHERE id=$1`,
				e.run).Scan(&current, &target, &oldHash); err != nil {
				t.Fatal(err)
			}
			if current == uuid.Nil || target == uuid.Nil || current == target || target == e.secret || len(oldHash) == 0 {
				t.Fatal("fixture requires distinct current A and pending B with a capability hash")
			}
			// The helper already obtained READY at epoch 41. Install the fence
			// afterwards, leaving that same pending operation X intact.
			fresh := fence == "pending_fresh"
			outcome := uuid.New()
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
 AND harness='codex' AND kind='issue' AND review_target_run_id IS NULL
 AND claim_released_at IS NULL AND worker_id=$2 AND claim_generation=$3
 AND codex_secret_id=$4 AND codex_claim_epoch=41 AND codex_cap_hash=$9
 AND credential_switch_requested_at IS NOT NULL
 AND credential_switch_generation=$3 AND credential_switch_op_id=$7
 AND codex_switch_target_secret_id=$8
 AND codex_switch_target_generation=$3 AND codex_switch_target_op_id=$7
 AND codex_switch_target_auth_mode='api_key'
 AND codex_switch_target_material_revision IS NOT NULL
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
 AND EXISTS (SELECT 1 FROM workers w WHERE w.id=r.worker_id
 AND w.user_id=r.user_id AND w.snapshot_register_nonce=$10
 AND w.credential_switch_apply_nonce=$10)
 AND (SELECT count(*) FROM run_credential_epochs b
 WHERE b.run_id=r.id AND b.claim_generation=r.claim_generation
 AND b.secret_id=$4)=1
 AND NOT EXISTS (SELECT 1 FROM run_credential_switch_epochs s WHERE s.run_id=r.id)
 FROM runs r WHERE id=$1`, e.run, e.worker, w.ClaimGeneration, current,
				outcome, fresh, e.op, target, oldHash, w.RegisterNonce).Scan(&isolated); err != nil {
				t.Fatal(err)
			}
			if !isolated {
				t.Fatal("fixture does not isolate the policy fence with pending X and current A at epoch 41")
			}

			before := stoppedRouteSnapshot(t, e)
			body := fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q}`,
				w.ClaimGeneration, w.RegisterNonce, e.op)
			rec := e.do(e.worker, http.MethodPost, e.applyPath(), body)
			if rec.Code != http.StatusOK {
				t.Fatalf("policy-fenced apply status=%d, want 200", rec.Code)
			}
			var wire map[string]json.RawMessage
			if json.Unmarshal(rec.Body.Bytes(), &wire) != nil {
				t.Fatal("apply response is not a JSON object")
			}
			var actualOutcome string
			if json.Unmarshal(wire["outcome"], &actualOutcome) != nil {
				t.Fatal("apply response lacks a string outcome")
			}
			// Keep this first: a bypass mutant must fail on the actual decision,
			// before any extra auth fields or changed snapshots are examined.
			if actualOutcome != "not_applied" {
				t.Fatalf("policy-fenced apply outcome=%.64q, want not_applied", actualOutcome)
			}
			var reason string
			if json.Unmarshal(wire["reason"], &reason) != nil || reason != "policy_refusal" {
				t.Fatal("policy-fenced apply did not return policy_refusal")
			}
			if len(wire) != 2 {
				t.Fatal("refusal must contain exactly outcome and reason")
			}
			after := stoppedRouteSnapshot(t, e)
			sections := []string{"run", "base", "switch", "messages", "inputs", "custody", "presentations"}
			if len(before) != len(sections) || len(after) != len(sections) {
				t.Fatal("snapshot must contain all seven sections")
			}
			for _, section := range sections {
				b, bok := before[section]
				a, aok := after[section]
				if !bok || !aok || !reflect.DeepEqual(b, a) {
					t.Fatalf("refused apply changed snapshot section %s", section)
				}
			}

			// Resolve only the two blocking facts, retaining the preserving
			// episode tuple. This is a narrow availability companion.
			stoppedRouteExec(t, e, `UPDATE runs SET policy_pending_fresh=false,
 policy_resolved_at=now() WHERE id=$1`, e.run)
			rec = e.do(e.worker, http.MethodPost, e.applyPath(), body)
			if rec.Code != http.StatusOK {
				t.Fatalf("resolved preserving apply status=%d, want 200", rec.Code)
			}
			if json.Unmarshal(rec.Body.Bytes(), &wire) != nil {
				t.Fatal("resolved apply response is not a JSON object")
			}
			if json.Unmarshal(wire["outcome"], &actualOutcome) != nil || actualOutcome != "applied" {
				t.Fatal("resolved preserving apply did not return applied")
			}
			var epoch, ordinal int64
			if json.Unmarshal(wire["capability_epoch"], &epoch) != nil || epoch != 42 ||
				json.Unmarshal(wire["ordinal"], &ordinal) != nil || ordinal != 1 {
				t.Fatal("resolved apply must return epoch 42 and ordinal 1")
			}
			var valid bool
			if err := e.pool.QueryRow(ctx, `SELECT
 codex_secret_id=$2 AND codex_auth_mode='api_key'
 AND codex_claim_epoch=42 AND codex_cap_hash IS NOT NULL
 AND octet_length(codex_cap_hash)=32 AND codex_cap_hash<>$3
 AND claim_generation=$4 AND worker_id=$5 AND claim_released_at IS NULL
 AND codex_material_revision IS NOT NULL
 AND credential_switch_requested_at IS NULL
 AND credential_switch_generation IS NULL AND credential_switch_op_id IS NULL
 AND codex_switch_target_secret_id IS NULL
 AND policy_outcome_id=$7 AND policy_preserve_required
 AND policy_continuation_mode='same_conversation'
 AND policy_applying_worker_id=$5 AND policy_applying_generation=$4
 AND policy_source_generation=$4 AND NOT policy_pending_fresh
 AND policy_resolved_at IS NOT NULL
 AND (SELECT count(*) FROM run_credential_switch_epochs s WHERE s.run_id=r.id)=1
 AND EXISTS (SELECT 1 FROM run_credential_switch_epochs s
 WHERE s.run_id=r.id AND s.op_id=$6 AND s.ordinal=1
 AND s.secret_id=r.codex_secret_id AND s.worker_id=r.worker_id
 AND s.claim_generation=r.claim_generation
 AND s.codex_claim_epoch=r.codex_claim_epoch AND s.codex_auth_mode=r.codex_auth_mode
 AND s.label IS NOT DISTINCT FROM r.codex_secret_label
 AND s.codex_material_revision=r.codex_material_revision
 AND s.codex_account_key IS NOT DISTINCT FROM r.codex_account_key)
 FROM runs r WHERE id=$1`, e.run, target, oldHash, w.ClaimGeneration,
				e.worker, e.op, outcome).Scan(&valid); err != nil {
				t.Fatal(err)
			}
			if !valid {
				t.Fatal("resolved X did not journal ordinal 1 and bind B at epoch 42 with current claim authority and a rotated capability hash")
			}
		})
	}
}
