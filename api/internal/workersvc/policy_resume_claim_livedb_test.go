package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// TestPolicyResumeClaimPayloadLiveDB drives the REAL claim assembly for a run released from a
// compact policy hold. The run is a Claude-harness row that carries the policy columns directly:
// the policy facts are harness-independent claim inputs, and a Codex claim needs a Codex account
// binding that is unrelated to what is asserted here.
//
//   - fresh_from_saved_work on a run whose plan is not approved yet: the saved session is cleared,
//     so the claim must not restore the awaiting_approval gate (that needs a session), the plan
//     stays unapproved, and the claim carries no preserving facts.
//   - preserve on the same shape (control): the session survives, so the claim restores the gate
//     AND carries policy_preserve_only plus the required head.
func TestPolicyResumeClaimPayloadLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	for _, lane := range []string{"fresh_from_saved_work", "fresh_from_saved_work_headless", "same_conversation"} {
		t.Run(lane, func(t *testing.T) {
			ctx := context.Background()
			if err := store.Migrate(ctx, dsn); err != nil {
				t.Fatal(err)
			}
			pool, err := store.OpenPool(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			box := newBox(t)
			q := store.New(pool)
			svc := New(q, box, testParams())
			svc.SetVault(vault.NewPostgres(box, pool))
			svc.SetTxBeginner(pool)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, sql, args...); err != nil {
					t.Fatalf("exec %q: %v", sql, err)
				}
			}
			userID, connID, repoID, wkrID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			sealedPAT, err := box.Seal([]byte("bot-pat-3607-abcdef1234567890"))
			if err != nil {
				t.Fatal(err)
			}
			sealedAnthropic, err := box.Seal([]byte("anthropic-3607-token-abcdef1234567890"))
			if err != nil {
				t.Fatal(err)
			}
			exec(`INSERT INTO users (id, email, password_hash, vault_mode) VALUES ($1, $2, 'x', 'unprotected')`, userID, fmt.Sprintf("p3607-%s@e2e", userID))
			exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, sealedPAT)
			exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			      VALUES ($1, $2, 1, 'g/p3607', 'https://forge.e2e/g/p3607', 'main', true)`, repoID, connID)
			exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
			      VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`, uuid.New(), userID, "anthropic-"+uuid.NewString(), sealedAnthropic)
			exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-3607', $3, 'offline')`, wkrID, userID, wkrID[:])
			if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: wkrID, ProtocolCapabilities: []string{capability.ProviderPolicyHoldV1}}); err != nil {
				t.Fatal(err)
			}
			wkr, err := q.GetWorkerByID(ctx, wkrID)
			if err != nil {
				t.Fatal(err)
			}
			head := hexHead("e")
			fresh := lane != "same_conversation"
			continuationMode := lane
			var requiredHead any = head
			if lane == "fresh_from_saved_work_headless" {
				continuationMode = "fresh_from_saved_work"
				requiredHead = nil
			}
			outcome := uuid.New()
			// An unapproved agent plan at the gate with a saved session, then held by a policy refusal.
			exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
			        claim_generation, auto_approve, plan_source, plan_md, session_id, hold_reason, status_since,
			        policy_outcome_id, policy_continuation_mode, policy_applying_worker_id, policy_applying_generation, policy_source_generation,
			        policy_preserve_required, policy_required_head)
			      VALUES ($1, $2, $3, 'issue', 3607, 't', 'd', 'paused', $4, 1, false, 'agent', '# Plan awaiting approval', 'saved-session', 'codex_policy_hold', now(),
			        $5, $6, $4, 1, 1, true, $7)`, runID, userID, repoID, wkrID, outcome, continuationMode, requiredHead)
			// Older pending inputs and an unresolved episode do not select the new marker.
			exec(`INSERT INTO run_user_inputs (run_id, kind, body, created_at) VALUES ($1, 'follow_up', 'older guidance', now() - interval '1 hour')`, runID)
			before, err := q.GetRunClaimContext(ctx, runID)
			if err != nil || before.PolicyResumeInputsPending {
				t.Fatalf("unresolved episode: pending=%v, error=%v", before.PolicyResumeInputsPending, err)
			}
			mode := PolicyResumePreserve
			if fresh {
				mode = PolicyResumeFreshFromSavedWork
			}
			if _, err := svc.ResumePolicyHold(ctx, userID, runID, PolicyResumeRequest{Mode: mode, ExpectedEpisodeID: outcome, Guidance: "stay narrow"}); err != nil {
				t.Fatalf("resume: %v", err)
			}
			payload, err := svc.Claim(ctx, wkr, nil)
			if err != nil || payload == nil || payload.RunID != runID.String() {
				t.Fatalf("claim = %+v, %v", payload, err)
			}
			wire, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var claim map[string]any
			if err := json.Unmarshal(wire, &claim); err != nil {
				t.Fatal(err)
			}
			if claim["policy_resume_inputs_pending"] != true {
				t.Fatalf("Resume guidance must select the input barrier even without a head: %s", wire)
			}
			// Delivery retires the marker, even though the resolved episode remains on the run.
			// Leave the older input pending to prove the resolution-time bound is real.
			exec(`UPDATE run_user_inputs SET consumed_at=now(), applied_at=now()
			      WHERE run_id=$1 AND kind='follow_up' AND created_at >= (SELECT policy_resolved_at FROM runs WHERE id=$1)`, runID)
			after, err := q.GetRunClaimContext(ctx, runID)
			if err != nil || after.PolicyResumeInputsPending {
				t.Fatalf("delivered guidance: pending=%v, error=%v", after.PolicyResumeInputsPending, err)
			}
			var autoApprove bool
			var planSource string
			if err := pool.QueryRow(ctx, "SELECT auto_approve, plan_source FROM runs WHERE id=$1", runID).Scan(&autoApprove, &planSource); err != nil {
				t.Fatal(err)
			}
			if autoApprove || planSource != "agent" {
				t.Fatalf("the plan became approved: auto_approve=%v source=%s", autoApprove, planSource)
			}
			if fresh {
				if phase, ok := claim["resume_phase"]; ok {
					t.Fatalf("resume_phase = %v on a fresh_from_saved_work claim, want absent (no session to restore the gate from)", phase)
				}
				if claim["session_id"] != nil || claim["policy_preserve_only"] != nil || claim["policy_expected_head"] != nil {
					t.Fatalf("fresh claim carries preserving facts: session=%v preserve=%v head=%v", claim["session_id"], claim["policy_preserve_only"], claim["policy_expected_head"])
				}
				return
			}
			if claim["resume_phase"] != "awaiting_approval" || claim["session_id"] != "saved-session" {
				t.Fatalf("preserve control must restore the gate with the saved session: phase=%v session=%v", claim["resume_phase"], claim["session_id"])
			}
			if claim["policy_preserve_only"] != true || claim["policy_expected_head"] != head {
				t.Fatalf("preserving claim facts: preserve=%v head=%v, want true/%s", claim["policy_preserve_only"], claim["policy_expected_head"], head)
			}
		})
	}
}
