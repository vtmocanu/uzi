package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// harness_claim_repair_livedb_test.go is the PRD #1429 M2 regression suite for the D4
// harness-first repair of assembleClaim (claim_assembly.go): a Codex run must never open,
// record, or carry an Anthropic credential, whatever else is true of the user's account, and
// a Claude run's claim must stay byte-identical. It reuses the codexTestEnv fixtures from
// codexauthz_livedb_test.go / codexcred_livedb_test.go. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres; run via ./e2e/run-store-it.sh. Every name ends LiveDB so the
// store-IT sweep selects it.

// countRunCredentialEpochs is the raw row count for run_credential_epochs, used to prove a
// Codex claim writes NO Anthropic attribution row (mirrors the pattern in
// credential_switch_message_livedb_test.go).
func (e codexTestEnv) countRunCredentialEpochs(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_credential_epochs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count run_credential_epochs: %v", err)
	}
	return n
}

// countHarnessModelFallbackNotes counts the D6 nonsecret status note (harness_model_fallback)
// on a run's feed.
func (e codexTestEnv) countHarnessModelFallbackNotes(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM run_messages WHERE run_id = $1 AND kind = $2`,
		runID, harnessModelFallbackKind).Scan(&n); err != nil {
		t.Fatalf("count harness_model_fallback notes: %v", err)
	}
	return n
}

// resealBotPAT reseals a real, master-box-decryptable bot PAT for userID's forge connection.
// seedCodexInfra stores a placeholder ciphertext the master box cannot open, so assembleClaim
// (which opens it before the harness branch) needs a real one to reach that branch at all.
func resealBotPAT(t *testing.T, env codexTestEnv, userID uuid.UUID) {
	t.Helper()
	sealed, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, sealed, userID)
}

// seedDefaultAnthropicToken inserts a default anthropic_token secret, returning its plaintext.
func seedDefaultAnthropicToken(t *testing.T, env codexTestEnv, userID uuid.UUID) string {
	t.Helper()
	plain := codexToken("anthropic")
	sealed, err := env.box.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES (gen_random_uuid(), $1, 'anthropic_token', $2, true, $3, 'master')`,
		userID, "anthropic-default", sealed)
	return plain
}

// TestAssembleClaimCodexOnlyFullAssemblySucceedsLiveDB (D4, success criterion 3): a Codex-only
// user's run assembles a FULL claim through assembleClaim: it carries Secrets.Codex and an
// EMPTY AnthropicOAuthToken (the harness-first branch never opens an Anthropic credential for
// a Codex run), and writes no run_credential_epochs row.
func TestAssembleClaimCodexOnlyFullAssemblySucceedsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env) // seeds a fully-bound, claimed subscription Codex run
	resealBotPAT(t, env, f.userID)

	svc := New(env.q, env.box, testParams())
	run := mustRun(t, env, f.runID)
	if run.Harness != harnessCodex {
		t.Fatalf("fixture run harness = %q, want codex", run.Harness)
	}

	payload, err := svc.assembleClaim(env.ctx, f.wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim on a Codex-only user's run: %v", err)
	}
	if payload.Secrets.Codex == nil {
		t.Fatal("a Codex-only user's claim must carry Secrets.Codex")
	}
	if payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("a Codex claim must carry NO anthropic_oauth_token, got %q", payload.Secrets.AnthropicOAuthToken)
	}
	if n := env.countRunCredentialEpochs(t, f.runID); n != 0 {
		t.Fatalf("run_credential_epochs rows = %d, want 0 (a Codex claim must write no Anthropic epoch)", n)
	}
}

// TestAssembleClaimCodexBothCredentialsNoAnthropicEpochLiveDB (D4): a Codex run for a user who
// ALSO holds a default Anthropic token still carries NO anthropic_oauth_token and writes NO
// run_credential_epochs row — the harness-first branch is unconditional, not merely "no
// Anthropic token present".
func TestAssembleClaimCodexBothCredentialsNoAnthropicEpochLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	seedDefaultAnthropicToken(t, env, f.userID) // the user ALSO holds a usable Anthropic token

	svc := New(env.q, env.box, testParams())
	run := mustRun(t, env, f.runID)

	payload, err := svc.assembleClaim(env.ctx, f.wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim on a both-credential Codex run: %v", err)
	}
	if payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("a both-credential Codex claim must carry NO anthropic_oauth_token, got %q", payload.Secrets.AnthropicOAuthToken)
	}
	if payload.Secrets.Codex == nil {
		t.Fatal("a both-credential Codex claim must still carry Secrets.Codex")
	}
	if n := env.countRunCredentialEpochs(t, f.runID); n != 0 {
		t.Fatalf("run_credential_epochs rows = %d, want 0 (a Codex claim must never attribute the Anthropic token, even when one exists)", n)
	}
}

// TestAssembleClaimClaudeRunUnchangedLiveDB is the control: an ordinary Claude run's claim
// stays byte-identical — it carries the Anthropic token and records exactly one
// run_credential_epochs row, exactly as before the D4 harness-first repair.
func TestAssembleClaimClaudeRunUnchangedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	resealBotPAT(t, env, userID)
	plainToken := seedDefaultAnthropicToken(t, env, userID)

	runID := env.seedCodexRun(t, userID, workerID, repoID) // harness defaults to 'claude'

	svc := New(env.q, env.box, testParams())
	run := mustRun(t, env, runID)
	if run.Harness != harnessClaude {
		t.Fatalf("fixture run harness = %q, want claude", run.Harness)
	}
	wkr := store.Worker{ID: workerID, UserID: userID}

	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim on a Claude run: %v", err)
	}
	if payload.Secrets.AnthropicOAuthToken != plainToken {
		t.Fatalf("anthropic_oauth_token = %q, want the spent token %q (a Claude claim must be unchanged)", payload.Secrets.AnthropicOAuthToken, plainToken)
	}
	if payload.Secrets.Codex != nil {
		t.Fatalf("a Claude claim must carry no Codex secrets, got %+v", payload.Secrets.Codex)
	}
	if n := env.countRunCredentialEpochs(t, runID); n != 1 {
		t.Fatalf("run_credential_epochs rows = %d, want exactly 1 (unchanged Claude attribution)", n)
	}
}

// TestAssembleClaimD6ModelFallbackDropsClaudeAliasLiveDB (D6): a Codex run whose stored model is
// a Claude alias assembles a claim whose DefaultModel is NOT that alias (it is dropped so the
// worker falls back to its own harness default) and records a visible, nonsecret
// harness_model_fallback run message.
func TestAssembleClaimD6ModelFallbackDropsClaudeAliasLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)

	const claudeAlias = "sonnet"
	env.exec(`UPDATE runs SET model = $2 WHERE id = $1`, f.runID, claudeAlias)

	svc := New(env.q, env.box, testParams())
	run := mustRun(t, env, f.runID)
	if run.Model.String != claudeAlias || !run.Model.Valid {
		t.Fatalf("fixture run model = %+v, want the stored claude alias %q", run.Model, claudeAlias)
	}

	payload, err := svc.assembleClaim(env.ctx, f.wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim on a Codex run with an incompatible stored model: %v", err)
	}
	if payload.Config.DefaultModel != nil && *payload.Config.DefaultModel == claudeAlias {
		t.Fatalf("payload.Config.DefaultModel = %q, want the claude alias DROPPED (nil, or the harness default), never forwarded to Codex", *payload.Config.DefaultModel)
	}
	if n := env.countHarnessModelFallbackNotes(t, f.runID); n != 1 {
		t.Fatalf("harness_model_fallback run messages = %d, want exactly 1", n)
	}
}
