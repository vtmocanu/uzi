package workersvc

import (
	"testing"

	"github.com/google/uuid"
)

// judge_enqueue_harness_livedb_test.go is the PRD #1429 M3 regression suite for Gate 4's
// harness-aware repair (judge_enqueue.go): the judge inherits the just-completed target
// run's harness as an EXPLICIT selection, routed through the M2 createRunResolved funnel.
// Before this repair Gate 4 required an Anthropic-token presence
// (GetUserSecretCiphertext), which silently starved a Codex-only owner of any judge run at
// all — this proves the fix. Reuses the codexTestEnv fixtures. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// judgeEnqueueSvc builds a Service over the live queries with the pool wired as the tx
// beginner (a Codex judge enqueue needs it for the atomic binding freeze) and the judge
// kill-switch on.
func judgeEnqueueSvc(env codexTestEnv, userID uuid.UUID) *Service {
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	svc.SetSettings(fakeSettings{enabled: true})
	env.exec(`UPDATE users SET judge_enabled = true WHERE id = $1`, userID)
	return svc
}

// judgeForTarget reads the (at most one) judge run's id + harness for a target, or ok=false
// when none was created.
func judgeForTarget(t *testing.T, env codexTestEnv, targetRunID uuid.UUID) (id uuid.UUID, harness string, ok bool) {
	t.Helper()
	rows, err := env.pool.Query(env.ctx, `SELECT id, harness FROM runs WHERE kind = 'judge' AND target_run_id = $1`, targetRunID)
	if err != nil {
		t.Fatalf("query judge for target: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return uuid.Nil, "", false
	}
	if err := rows.Scan(&id, &harness); err != nil {
		t.Fatalf("scan judge row: %v", err)
	}
	return id, harness, true
}

// TestMaybeEnqueueJudgeCodexOnlyTargetEnqueuesCodexJudgeLiveDB (D4, success criterion 7): a
// Codex-only owner's completed target run enqueues a JUDGE RUN FROZEN ONTO CODEX — the exact
// regression the old hardcoded Anthropic-token Gate 4 caused (a Codex-only owner never got a
// judge at all, however completed/eligible the target). The judge's own Codex binding is
// frozen atomically by createRunResolved.
func TestMaybeEnqueueJudgeCodexOnlyTargetEnqueuesCodexJudgeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env) // openai_api_key default; NO anthropic token
	svc := judgeEnqueueSvc(env, userID)

	targetRunID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness)
	          VALUES ($1, $2, $3, 'issue', 9001, 't', 'd', 'completed', 'codex')`, targetRunID, userID, repoID)
	target := mustRun(t, env, targetRunID)
	if target.Harness != string(HarnessCodex) {
		t.Fatalf("fixture target harness = %q, want codex", target.Harness)
	}

	svc.maybeEnqueueJudge(env.ctx, target)

	judgeID, harness, ok := judgeForTarget(t, env, targetRunID)
	if !ok {
		t.Fatal("a Codex-only target must enqueue a judge run (was silently blocked by the old Anthropic-presence Gate 4)")
	}
	if harness != string(HarnessCodex) {
		t.Fatalf("judge harness = %q, want codex (inherited from the target, not the M1 HarnessClaude stopgap)", harness)
	}
	judgeRun := mustRun(t, env, judgeID)
	if !judgeRun.CodexSecretID.Valid {
		t.Fatal("the Codex judge run must have its own codex_secret_id frozen (createRunResolved's atomic freeze)")
	}
	if n := env.countRunCredentialEpochs(t, judgeID); n != 0 {
		t.Fatalf("run_credential_epochs rows = %d, want 0 (a Codex judge enqueue must never touch Anthropic attribution)", n)
	}
}

// TestMaybeEnqueueJudgeClaudeTargetStillRequiresAnthropicLiveDB is the control: a Claude
// target with NO Anthropic token is still silently skipped (Gate 4's Claude branch is
// byte-identical to before this repair).
func TestMaybeEnqueueJudgeClaudeTargetStillRequiresAnthropicLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t) // no anthropic token, no codex credential
	svc := judgeEnqueueSvc(env, userID)

	targetRunID := env.seedCodexRun(t, userID, workerID, repoID) // harness defaults to 'claude'
	target := mustRun(t, env, targetRunID)
	env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, targetRunID)
	target.Status = "completed"

	svc.maybeEnqueueJudge(env.ctx, target)

	if _, _, ok := judgeForTarget(t, env, targetRunID); ok {
		t.Fatal("a Claude target with no Anthropic token must still be skipped, no judge run created")
	}
}

// TestMaybeEnqueueJudgeClaudeTargetWithTokenEnqueuesLiveDB: a Claude target WITH an
// Anthropic token still enqueues, exactly as before this repair.
func TestMaybeEnqueueJudgeClaudeTargetWithTokenEnqueuesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	// Gate 4's Claude branch reads GetUserSecretCiphertext, which is keyed on is_default
	// (not mere existence — that is UserHasAnthropicToken, the resolver's own check).
	// seedDefaultAnthropicToken (not seedAnthropicToken) is the helper that sets it.
	seedDefaultAnthropicToken(t, env, userID)
	svc := judgeEnqueueSvc(env, userID)

	targetRunID := env.seedCodexRun(t, userID, workerID, repoID)
	env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, targetRunID)
	target := mustRun(t, env, targetRunID)

	svc.maybeEnqueueJudge(env.ctx, target)

	_, harness, ok := judgeForTarget(t, env, targetRunID)
	if !ok {
		t.Fatal("a Claude target with an Anthropic token must enqueue a judge")
	}
	if harness != string(HarnessClaude) {
		t.Fatalf("judge harness = %q, want claude", harness)
	}
}

// TestRerunJudgeCodexOnlyTargetEnqueuesCodexJudgeLiveDB (D4): the manual "re-run judge"
// action gets the SAME harness-aware repair as the automatic funnel above — a Codex-only
// owner's explicit re-run mints a judge FROZEN onto Codex, not the M1 HarnessClaude
// stopgap, and never touches the Anthropic-token gate.
func TestRerunJudgeCodexOnlyTargetEnqueuesCodexJudgeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env) // openai_api_key default; NO anthropic token
	svc := atomicSvc(env)
	svc.SetSettings(fakeSettings{enabled: true})

	targetRunID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness)
	          VALUES ($1, $2, $3, 'issue', 9101, 't', 'd', 'completed', 'codex')`, targetRunID, userID, repoID)

	judge, err := svc.RerunJudge(env.ctx, userID, false, targetRunID)
	if err != nil {
		t.Fatalf("RerunJudge for a Codex-only owner: %v", err)
	}
	if judge.Harness != string(HarnessCodex) {
		t.Fatalf("re-run judge harness = %q, want codex (inherited from the target)", judge.Harness)
	}
	judgeRun := mustRun(t, env, judge.ID)
	if !judgeRun.CodexSecretID.Valid {
		t.Fatal("the re-run Codex judge must have its own codex_secret_id frozen")
	}
	if n := env.countRunCredentialEpochs(t, judge.ID); n != 0 {
		t.Fatalf("run_credential_epochs rows = %d, want 0 (a Codex re-run must never touch Anthropic attribution)", n)
	}
}
