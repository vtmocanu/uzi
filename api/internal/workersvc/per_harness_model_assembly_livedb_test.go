package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1551 M4 assembleClaim-level coverage: the claim payload's DefaultModel is resolved from the
// per-harness LANE matching the frozen run harness (not the legacy shared default_model), a custom
// Codex root is shipped unchanged to a capable worker and REQUEUED (never failed) for an incapable
// one, a Codex task-REVIEW run ships no default_model at all, and a Claude run reads the Claude lane
// (the old shared-slot behaviour — a Codex model in the legacy default_model reaching a Claude run —
// no longer happens). Runs against a real throwaway Postgres — skipped unless UZI_TEST_DATABASE_URL
// is set (run via ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID.

// runStatusOf reads a run's status.
func runStatusOf(t *testing.T, env codexTestEnv, runID uuid.UUID) string {
	t.Helper()
	var s string
	if err := env.pool.QueryRow(env.ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&s); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return s
}

// TestAssembleCodexClaimCustomLaneLiveDB proves the D4 lane read AND the D6 post-claim re-check on a
// real, fully-bound subscription Codex run: with the owner's Codex lane set to a CUSTOM id (and the
// Claude lane set to a Claude id that must be ignored), a codex_custom_model_v1 worker's claim carries
// the custom id UNCHANGED, while a codex_harness_v1-only worker's claim is REQUEUED (never failed,
// never Astra).
//
// MUTATION (D6 assembly re-check): delete the `errCustomModelCapabilityMissing` guard in
// claim_assembly.go — the incapable-worker sub-case then assembles a payload shipping the custom id to
// a worker that cannot run it, and that sub-case fails.
func TestAssembleCodexClaimCustomLaneLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	// Codex lane custom; Claude lane a real Claude id that a Codex claim MUST ignore.
	env.exec(`UPDATE users SET default_codex_model = $2, default_claude_model = $3 WHERE id = $1`,
		f.userID, customCodexModel, "claude-opus-4-8")

	svc := New(env.q, env.box, testParams())
	run := mustRun(t, env, f.runID)

	t.Run("capable worker: custom id shipped unchanged", func(t *testing.T) {
		capable := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: []string{capability.CodexHarnessV1, capability.CodexCustomModelV1}}
		payload, err := svc.assembleClaim(env.ctx, capable, run)
		if err != nil {
			t.Fatalf("assembleClaim (capable worker): %v", err)
		}
		if payload.Config.DefaultModel == nil {
			t.Fatal("codex claim DefaultModel is nil; want the custom Codex lane value")
		}
		if got := *payload.Config.DefaultModel; got != customCodexModel {
			t.Fatalf("codex claim DefaultModel = %q, want the custom Codex lane %q (unchanged, and NOT the Claude lane)", got, customCodexModel)
		}
	})

	t.Run("incapable worker: requeue, never fail, never Astra", func(t *testing.T) {
		incapable := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: []string{capability.CodexHarnessV1}}
		payload, err := svc.assembleClaim(env.ctx, incapable, run)
		if !errors.Is(err, errCustomModelCapabilityMissing) {
			t.Fatalf("assembleClaim (incapable worker): err = %v, want errCustomModelCapabilityMissing", err)
		}
		if payload != nil {
			t.Fatalf("a gated custom-model claim must return no payload; got %+v", payload)
		}
		// finishRunClaim REQUEUES the claimed run to queued through the fenced exact-claim
		// transaction (PRD #1590 M1), never fails it. The capable sub-case minted a capability,
		// so the finish compares against the current row, which this incapable claim left as is.
		svc.SetTxBeginner(env.pool)
		current := mustRun(t, env, f.runID)
		if got, rerr := svc.finishRunClaim(env.ctx, current, payload, err, claimRecoveryIdentity{workerID: f.workerID}); rerr != nil || got != nil {
			t.Fatalf("finishRunClaim = (%v, %v), want idle", got != nil, rerr)
		}
		if s := runStatusOf(t, env, f.runID); s != "queued" {
			t.Fatalf("after the gated claim the run status = %q, want queued (requeued, not failed)", s)
		}
		var owner pgtype.UUID
		if err := env.pool.QueryRow(env.ctx, `SELECT worker_id FROM runs WHERE id = $1`, f.runID).Scan(&owner); err != nil {
			t.Fatalf("read requeued run affinity: %v", err)
		}
		if !owner.Valid || uuid.UUID(owner.Bytes) != f.workerID {
			t.Fatalf("custom-capability requeue worker_id = %+v, want prior worker %s for bounded resume affinity", owner, f.workerID)
		}
	})
}

// TestAssembleCodexReviewClaimShipsNoModelLiveDB is the assembly-side REQUIRED review regression: a
// fully-bound Codex task-REVIEW run (kind='task', review_target_run_id set) whose owner's Codex lane
// is CUSTOM assembles WITHOUT a requeue and ships NO default_model — a Codex task review uses the
// agent's built-in gpt-6-sol, so assembly reads no Codex lane and the custom-model re-check is skipped.
// It is claimable/assemblable by a codex_harness_v1-only worker.
//
// MUTATION (review exemption): change claim_assembly.go's review guard so a review run reads the Codex
// lane (e.g. key it on kind, or drop it) — the codex_harness_v1-only worker then trips
// errCustomModelCapabilityMissing on the custom lane and this test fails.
func TestAssembleCodexReviewClaimShipsNoModelLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	resealBotPAT(t, env, userID)
	env.exec(`UPDATE users SET default_codex_model = $2 WHERE id = $1`, userID, customCodexModel)

	// A completed target, then a claimed Codex task-review run pointing at it; freeze its Codex binding.
	targetID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, issue_title, issue_description, status, harness)
	          VALUES ($1, $2, $3, 'task', $4, 'main', 't', 'd', 'completed', 'codex')`,
		targetID, userID, repoID, "uzi/task/"+targetID.String())

	reviewID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at, issue_title, issue_description, status, worker_id, harness)
	          VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), 't', 'd', 'claimed', $6, 'codex')`,
		reviewID, userID, repoID, "uzi/task/"+reviewID.String(), targetID, workerID)

	aliasID := env.seedLinkedSubscription(t, userID, "codex-review-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	svc := New(env.q, env.box, testParams())
	if err := svc.FreezeCodexBinding(env.ctx, userID, reviewID, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding on the review run: %v", err)
	}

	run := mustRun(t, env, reviewID)
	if !run.ReviewTargetRunID.Valid {
		t.Fatal("fixture review run must carry review_target_run_id")
	}
	if run.Harness != string(HarnessCodex) {
		t.Fatalf("fixture review run harness = %q, want codex", run.Harness)
	}
	// A codex_harness_v1-ONLY worker assembles it (no codex_custom_model_v1) and it ships no model.
	harnessOnly := store.Worker{ID: workerID, UserID: userID, ProtocolCapabilities: []string{capability.CodexHarnessV1}}
	payload, err := svc.assembleClaim(env.ctx, harnessOnly, run)
	if err != nil {
		t.Fatalf("assembleClaim on a Codex review run must succeed for a codex_harness_v1-only worker (review exempt): %v", err)
	}
	if payload.Config.DefaultModel != nil {
		t.Fatalf("a Codex task-review claim must ship NO default_model (built-in gpt-6-sol); got %q", *payload.Config.DefaultModel)
	}
}

// TestAssembleClaudeClaimReadsClaudeLaneNotLegacyLiveDB is the D3 claim-level regression: a Claude run
// reads the CLAUDE lane, so the old shared-slot behaviour — a Codex model saved in the legacy
// users.default_model reaching a Claude run — no longer happens. With the legacy default_model set to a
// curated Codex id and the Claude lane empty, the Claude claim ships NO model; setting the Claude lane
// then ships exactly that value.
//
// MUTATION (lane read): revert chat.go / claim_assembly.go to read GetUserDefaultModel — the first
// sub-case then ships the Codex legacy value on a Claude claim and fails.
func TestAssembleClaudeClaimReadsClaudeLaneNotLegacyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	resealBotPAT(t, env, userID)

	// A default Anthropic token so the Claude claim's credential ladder resolves.
	seedDefaultAnthropicToken(t, env, userID)

	// Legacy default_model holds a curated Codex id (the pre-#1551 shared slot); Claude lane empty.
	env.exec(`UPDATE users SET default_model = $2, default_claude_model = NULL WHERE id = $1`, userID, curatedCodexModel)

	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	          VALUES ($1, $2, $3, 'issue', 4242, 't', 'd', 'claimed', $4)`, runID, userID, repoID, workerID)

	svc := New(env.q, env.box, testParams())
	wkr := store.Worker{ID: workerID, UserID: userID}

	run := mustRun(t, env, runID)
	if run.Harness != harnessClaude {
		t.Fatalf("fixture run harness = %q, want claude", run.Harness)
	}
	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim (Claude run): %v", err)
	}
	if payload.Config.DefaultModel != nil {
		t.Fatalf("a Claude claim must NOT inherit the legacy default_model (%q); DefaultModel = %q, want nil (empty Claude lane)", curatedCodexModel, *payload.Config.DefaultModel)
	}

	// Now set the Claude lane: the claim ships exactly it.
	env.exec(`UPDATE users SET default_claude_model = $2 WHERE id = $1`, userID, "claude-sonnet-4-6")
	payload2, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim (Claude run, lane set): %v", err)
	}
	if payload2.Config.DefaultModel == nil || *payload2.Config.DefaultModel != "claude-sonnet-4-6" {
		t.Fatalf("Claude claim DefaultModel = %v, want the Claude lane %q", payload2.Config.DefaultModel, "claude-sonnet-4-6")
	}
}
