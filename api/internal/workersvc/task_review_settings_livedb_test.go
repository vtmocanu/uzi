package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestTaskReviewCodexClaimLiveDB is the Codex half of the review-claim assembly proof (PRD #1570):
// a fully bound Codex task-review run ships the owner's Codex review model to a worker advertising
// task_review_settings_v1 (omitted, so the worker runs its gpt-6-sol built-in, when unset or when the stored value is no longer a
// Codex role model), withholds it with a feed notice from one that does not, never ships the Claude
// column. (Non-review claims are covered by TestNonReviewClaimNeverCarriesTaskReviewModel.) Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres; run via ./e2e/run-store-it.sh.
func TestTaskReviewCodexClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	resealBotPAT(t, env, userID)

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
	svc.SetVault(env.vlt)
	if err := svc.FreezeCodexBinding(env.ctx, userID, reviewID, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding on the review run: %v", err)
	}

	base := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}
	capable := append(append([]string{}, base...), capability.TaskReviewSettingsV1)
	assemble := func(caps []string, gen int64) *ClaimPayload {
		t.Helper()
		env.exec(`UPDATE runs SET claim_generation = $2 WHERE id = $1`, reviewID, gen)
		p, err := svc.assembleClaim(env.ctx, store.Worker{ID: workerID, UserID: userID, ProtocolCapabilities: caps}, mustRun(t, env, reviewID))
		if err != nil {
			t.Fatalf("assembleClaim: %v", err)
		}
		return p
	}
	notes := func(gen int64) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1 AND kind = 'status'
			AND payload->>'event' = 'task_review_settings_omitted' AND claim_generation = $2`, reviewID, gen).Scan(&n); err != nil {
			t.Fatalf("count notes: %v", err)
		}
		return n
	}
	set := func(claude, codex string) {
		t.Helper()
		env.exec(`UPDATE users SET task_review_model = NULLIF($2,''), codex_task_review_model = NULLIF($3,'') WHERE id = $1`, userID, claude, codex)
	}

	// Unset: the field is omitted for a capable worker (it runs its built-in); an incapable worker runs the same built-in,
	// so there is nothing withheld and no notice.
	set("", "")
	if got := derefOr(assemble(capable, 1).TaskReviewModel, "<omitted>"); got != "<omitted>" {
		t.Errorf("unset capable task_review_model = %q, want omitted (the worker's gpt-6-sol built-in applies)", got)
	}
	if got := assemble(base, 2).TaskReviewModel; got != nil || notes(2) != 0 {
		t.Errorf("unset incapable: model=%v notes=%d, want omitted with no notice", got, notes(2))
	}

	// A configured Codex model rides to a capable worker only; the Claude column is never read for Codex.
	set("opus", "gpt-6.1-sol")
	if got := derefOr(assemble(capable, 3).TaskReviewModel, "<omitted>"); got != "gpt-6.1-sol" {
		t.Errorf("configured capable task_review_model = %q, want gpt-6.1-sol", got)
	}
	if got := assemble(base, 4).TaskReviewModel; got != nil || notes(4) != 1 {
		t.Errorf("configured incapable: model=%v notes=%d, want omitted with one notice", got, notes(4))
	}

	// A stored model that left the curated set falls back to the built-in; nothing configured is withheld.
	set("", "gpt-4-retired")
	if got := derefOr(assemble(capable, 5).TaskReviewModel, "<omitted>"); got != "<omitted>" {
		t.Errorf("invalid stored capable task_review_model = %q, want omitted (the gpt-6-sol built-in applies)", got)
	}
	if got := assemble(base, 6).TaskReviewModel; got != nil || notes(6) != 0 {
		t.Errorf("invalid stored incapable: model=%v notes=%d, want omitted with no notice", got, notes(6))
	}

	// The effective view reads the same rule: configured -> user_default, retired -> builtin.
	set("", "gpt-6.1-sol")
	got, err := svc.PredictLead(env.ctx, userID, HarnessCodex, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-6.1-sol" || got.ModelSource != "user_default" || got.DefaultModel != "gpt-6-sol" {
		t.Errorf("predicted codex review = %+v", got)
	}
}
