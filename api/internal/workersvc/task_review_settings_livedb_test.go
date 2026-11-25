package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
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

	base := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}
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

	// The model assertions below run with a Runtime default worker effort, so no inherited effort level
	// is withheld (that is asserted at the end).
	env.exec(`UPDATE users SET default_codex_effort = 'runtime', default_effort = 'runtime' WHERE id = $1`, userID)

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

	// The review effort (PRD #1570 M2): an unset review effort inherits the stored Codex worker
	// effort, an explicit one overrides it, `runtime` sends none, a run_override effort on the review
	// run changes nothing, and a corrupt value sends the Codex built-in.
	setEffort := func(review, worker string) {
		t.Helper()
		env.exec(`UPDATE users SET codex_task_review_effort = NULLIF($2,''), default_codex_effort = NULLIF($3,''),
			task_review_effort = 'max', default_effort = 'max' WHERE id = $1`, userID, review, worker)
	}
	gen := int64(10)
	effort := func(caps []string) (string, int) {
		t.Helper()
		gen++
		p := assemble(caps, gen)
		return derefOr(p.TaskReviewEffort, "<omitted>"), notes(gen)
	}
	set("", "")
	setEffort("", "low")
	if got, n := effort(capable); got != "low" || n != 0 {
		t.Errorf("unset review + worker low, capable: effort=%q notes=%d, want low/0 (the Claude columns are never read for Codex)", got, n)
	}
	if got, n := effort(base); got != "<omitted>" || n != 1 {
		t.Errorf("unset review + worker low, incapable: effort=%q notes=%d, want withheld with one notice", got, n)
	}
	env.exec(`UPDATE runs SET effort = 'xhigh', effort_source = 'run_override' WHERE id = $1`, reviewID)
	if got, _ := effort(capable); got != "low" {
		t.Errorf("a run_override effort changed the review effort to %q, want the worker's low", got)
	}
	setEffort("high", "low")
	if got, n := effort(capable); got != "high" || n != 0 {
		t.Errorf("explicit high: effort=%q notes=%d", got, n)
	}
	setEffort("runtime", "low")
	if got, n := effort(capable); got != "<omitted>" || n != 0 {
		t.Errorf("explicit runtime with a worker level: effort=%q notes=%d, want omitted", got, n)
	}
	if got, n := effort(base); got != "<omitted>" || n != 0 {
		t.Errorf("explicit runtime, incapable: effort=%q notes=%d, want no notice", got, n)
	}
	setEffort("", "runtime")
	if got, _ := effort(capable); got != "<omitted>" {
		t.Errorf("inherited Runtime default worker effort: %q, want omitted", got)
	}
	setEffort("HI gh", "low")
	if got, _ := effort(capable); got != agenttmpl.UziDefaultCodexEffort {
		t.Errorf("corrupt review effort = %q, want the built-in %q", got, agenttmpl.UziDefaultCodexEffort)
	}
	setEffort("", "")
	if got, n := effort(base); got != "<omitted>" || n != 1 {
		t.Errorf("inherited built-in, incapable: effort=%q notes=%d, want withheld with one notice", got, n)
	}
	setEffort("", "low")
	pred, err := svc.PredictLead(env.ctx, userID, HarnessCodex, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if pred.Effort == nil || *pred.Effort != "low" || !pred.ReviewEffortInherited || pred.EffortSource != "user_default" {
		t.Errorf("predicted codex review effort = %+v", pred)
	}
}
