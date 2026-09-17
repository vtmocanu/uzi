package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// judge_harness_repair_livedb_test.go is the PRD #1429 M3 regression suite for the D4
// harness-first repair of assembleJudgeClaim (judge.go): a Codex judge run must never open,
// record, or carry an Anthropic credential, whatever else is true of the user's account, and
// a Claude judge run's claim must stay byte-identical. Mirrors
// harness_claim_repair_livedb_test.go's ordinary-lane suite, and reuses the codexTestEnv
// fixtures. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. Every name ends LiveDB so the store-IT sweep selects it.

// seedCodexJudgeRun inserts a kind='judge' run reviewing targetRunID, owned by workerID in
// status 'claimed', harness='codex' (the M1 stopgap this milestone replaces). A judge run
// carries no repo_id (repo-less by shape), mirroring workersvc.CreateJudgeRun's INSERT.
func (e codexTestEnv) seedCodexJudgeRun(t *testing.T, userID, workerID, targetRunID uuid.UUID) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, worker_id, trigger_source, harness)
	        VALUES ($1, $2, 'judge', $3, 'Judge of run', '', 'claimed', $4, 'judge', 'codex')`,
		runID, userID, targetRunID, workerID)
	return runID
}

// TestAssembleJudgeClaimCodexOnlyFullAssemblySucceedsLiveDB (D4, success criterion 7): a
// Codex-only user's JUDGE run assembles a FULL claim through assembleClaim (which forks to
// assembleJudgeClaim for kind=judge): it carries Secrets.Codex and an EMPTY
// AnthropicOAuthToken (the harness-first branch never calls judgeChoice/openWithAutoRetry/
// recordRunCredential for a Codex judge), and writes no run_credential_epochs row.
func TestAssembleJudgeClaimCodexOnlyFullAssemblySucceedsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)

	access, refresh := codexToken("access"), codexToken("refresh")
	aliasID := env.seedLinkedSubscription(t, userID, "codex-sub-"+uuid.NewString(), access, refresh)

	targetRunID := env.seedCodexRun(t, userID, workerID, repoID) // an ordinary reviewed run; its own harness is irrelevant here
	judgeRunID := env.seedCodexJudgeRun(t, userID, workerID, targetRunID)

	svc := &Service{q: env.q, box: env.box}
	if err := svc.FreezeCodexBinding(env.ctx, userID, judgeRunID, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding on the judge run: %v", err)
	}

	run := mustRun(t, env, judgeRunID)
	if run.Harness != harnessCodex {
		t.Fatalf("fixture judge run harness = %q, want codex", run.Harness)
	}
	wkr := store.Worker{ID: workerID, UserID: userID}

	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim on a Codex-only user's judge run: %v", err)
	}
	if payload.Secrets.Codex == nil {
		t.Fatal("a Codex-only user's judge claim must carry Secrets.Codex")
	}
	if payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("a Codex judge claim must carry NO anthropic_oauth_token, got %q", payload.Secrets.AnthropicOAuthToken)
	}
	if n := env.countRunCredentialEpochs(t, judgeRunID); n != 0 {
		t.Fatalf("run_credential_epochs rows = %d, want 0 (a Codex judge claim must write no Anthropic epoch)", n)
	}
}

// TestAssembleJudgeClaimCodexBothCredentialsNoAnthropicEpochLiveDB (D4): a Codex judge run
// for a user who ALSO holds a default Anthropic token still carries NO anthropic_oauth_token
// and writes NO run_credential_epochs row — the harness-first branch is unconditional, not
// merely "no Anthropic token present".
func TestAssembleJudgeClaimCodexBothCredentialsNoAnthropicEpochLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	seedDefaultAnthropicToken(t, env, userID) // the user ALSO holds a usable Anthropic token

	access, refresh := codexToken("access"), codexToken("refresh")
	aliasID := env.seedLinkedSubscription(t, userID, "codex-sub-"+uuid.NewString(), access, refresh)

	targetRunID := env.seedCodexRun(t, userID, workerID, repoID)
	judgeRunID := env.seedCodexJudgeRun(t, userID, workerID, targetRunID)

	svc := &Service{q: env.q, box: env.box}
	if err := svc.FreezeCodexBinding(env.ctx, userID, judgeRunID, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding on the judge run: %v", err)
	}

	run := mustRun(t, env, judgeRunID)
	wkr := store.Worker{ID: workerID, UserID: userID}
	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim on a both-credential Codex judge run: %v", err)
	}
	if payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("a both-credential Codex judge claim must carry NO anthropic_oauth_token, got %q", payload.Secrets.AnthropicOAuthToken)
	}
	if payload.Secrets.Codex == nil {
		t.Fatal("a both-credential Codex judge claim must still carry Secrets.Codex")
	}
	if n := env.countRunCredentialEpochs(t, judgeRunID); n != 0 {
		t.Fatalf("run_credential_epochs rows = %d, want 0 (a Codex judge claim must never attribute the Anthropic token, even when one exists)", n)
	}
}

// TestAssembleJudgeClaimClaudeRunUnchangedLiveDB is the control: an ordinary Claude judge
// run's claim stays byte-identical — it carries the Anthropic token and records exactly one
// run_credential_epochs row, exactly as before the D4 harness-first repair.
func TestAssembleJudgeClaimClaudeRunUnchangedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	plainToken := seedDefaultAnthropicToken(t, env, userID)

	targetRunID := env.seedCodexRun(t, userID, workerID, repoID)
	judgeRunID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, worker_id, trigger_source, harness)
	          VALUES ($1, $2, 'judge', $3, 'Judge of run', '', 'claimed', $4, 'judge', 'claude')`,
		judgeRunID, userID, targetRunID, workerID)

	// New(...), not the bare &Service{} struct literal the other two tests in this file use:
	// the Claude lane's judgeChoice hits BindModeAuto (users.judge_anthropic_bind_mode
	// defaults to 'auto') and so calls autoChoice, which reads s.now() — nil on a bare
	// struct literal, so this MUST go through the full constructor.
	svc := New(env.q, env.box, testParams())
	run := mustRun(t, env, judgeRunID)
	if run.Harness != harnessClaude {
		t.Fatalf("fixture judge run harness = %q, want claude", run.Harness)
	}

	wkr := store.Worker{ID: workerID, UserID: userID}
	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim on a Claude judge run: %v", err)
	}
	if payload.Secrets.AnthropicOAuthToken != plainToken {
		t.Fatalf("anthropic_oauth_token = %q, want the spent token %q (a Claude judge claim must be unchanged)", payload.Secrets.AnthropicOAuthToken, plainToken)
	}
	if payload.Secrets.Codex != nil {
		t.Fatalf("a Claude judge claim must carry no Codex secrets, got %+v", payload.Secrets.Codex)
	}
	if n := env.countRunCredentialEpochs(t, judgeRunID); n != 1 {
		t.Fatalf("run_credential_epochs rows = %d, want exactly 1 (unchanged Claude attribution)", n)
	}
}
