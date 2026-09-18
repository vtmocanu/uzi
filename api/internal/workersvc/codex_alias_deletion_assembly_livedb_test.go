package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimAssemblyCodexAliasDeletionFailsClosedLiveDB is the PRD #1332 M5A (D3) alias-deletion
// regression: a Codex run whose bound alias was deleted keeps harness='codex' (and its deletion-proof
// codex_material_revision sentinel) while the runs_codex_secret_fk ON DELETE SET NULL nulls only
// codex_secret_id. Claim assembly is harness-authoritative, so it MUST still enter the Codex branch and
// FAIL CLOSED (errCredentialUnavailable) — it must NEVER assemble a Claude claim or hand out the
// Anthropic token, even though a valid default Anthropic token is present.
//
// FAIL-OLD / PASS-FIXED (assembly-branch calibration): revert claim_assembly.go's guard from
// `if run.Harness == harnessCodex` back to `if run.CodexSecretID.Valid` — the nulled-alias run then
// SKIPS the Codex branch, assembleClaim succeeds returning a Claude claim carrying the Anthropic OAuth
// token, and both the error and payload assertions below fail. This is the exact silent-Claude-fallback
// the D3 fix closes.
//
// Runs against a real throwaway Postgres — skipped unless UZI_TEST_DATABASE_URL is set.
func TestClaimAssemblyCodexAliasDeletionFailsClosedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env) // a fully-bound, claimed subscription Codex run (harness='codex')

	// Seed the Claude credentials assembleClaim would spend IF it wrongly took the Claude path: reseal a
	// real bot PAT (seedCodexInfra stored a placeholder the master box cannot decrypt) and add a default
	// Anthropic token. Their presence is what makes the FAIL-OLD outcome (a Claude claim) reachable, so
	// the PASS-FIXED errCredentialUnavailable is a meaningful fail-closed rather than a missing-creds error.
	botPATSealed, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPATSealed, f.userID)
	anthropicSealed, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), f.userID, "anthropic-"+uuid.NewString(), anthropicSealed)

	svc := New(env.q, env.box, testParams())
	wkr := store.Worker{ID: f.workerID, UserID: f.userID}

	// Baseline: the fully-bound Codex run assembles WITH a Codex block, so the fixture is a genuinely
	// working Codex run and the post-deletion failure below is attributable to the deletion, not a
	// broken fixture. (A dark Codex claim still carries the Anthropic token as a fallback field; the
	// worker selects Codex by the PRESENCE of secrets.codex — so the guarantee under test is not
	// "no anthropic token" but "no payload at all when the binding is gone", asserted after deletion.)
	baseline := mustRun(t, env, f.runID)
	if baseline.Harness != harnessCodex {
		t.Fatalf("fixture run harness = %q, want codex (FreezeCodexBinding must set it)", baseline.Harness)
	}
	if !baseline.CodexSecretID.Valid {
		t.Fatalf("fixture run must be bound (codex_secret_id set) before alias deletion")
	}
	basePayload, err := svc.assembleClaim(env.ctx, wkr, baseline)
	if err != nil {
		t.Fatalf("assembleClaim on the intact Codex run: %v", err)
	}
	if basePayload.Secrets.Codex == nil {
		t.Fatal("intact Codex run must assemble a codex block")
	}

	// Delete the bound alias. The composite runs_codex_secret_fk (00202) is ON DELETE SET NULL
	// (codex_secret_id), so it nulls ONLY codex_secret_id; harness stays 'codex' and the deletion-proof
	// codex_material_revision survives (codex_credential_state cascades away). This is exactly the state
	// the alias-deletion path leaves behind.
	env.exec(`DELETE FROM user_secrets WHERE id = $1`, f.aliasID)

	run := mustRun(t, env, f.runID)
	if run.Harness != harnessCodex {
		t.Fatalf("after alias deletion harness = %q, want codex unchanged (D3: harness authoritative)", run.Harness)
	}
	if run.CodexSecretID.Valid {
		t.Fatalf("alias deletion must null codex_secret_id (the FK ON DELETE SET NULL); got valid")
	}
	if !run.CodexMaterialRevision.Valid {
		t.Fatalf("the deletion-proof codex_material_revision sentinel must survive alias deletion")
	}

	// Fail-closed: the branch is entered on harness='codex' (not codex_secret_id), the binding is now
	// unavailable, so assembleClaim returns errCredentialUnavailable with NO payload — never a Claude claim.
	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if !errors.Is(err, errCredentialUnavailable) {
		t.Fatalf("assembleClaim on a nulled-alias Codex run: err = %v, want errCredentialUnavailable (must fail closed, never assemble a Claude claim)", err)
	}
	if payload != nil {
		t.Fatalf("fail-closed assembly must return no payload; got %+v", payload)
	}
}
