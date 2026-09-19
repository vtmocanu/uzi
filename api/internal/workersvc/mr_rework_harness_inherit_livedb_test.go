package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// mr_rework_harness_inherit_livedb_test.go is the PRD #1429 M2 regression for D4's derived-run
// inheritance rule applied to MR-rework: a rework of a Codex source run inherits harness='codex'
// (and freezes its own Codex binding) as an EXPLICIT createRunResolved selection, and if that
// harness is no longer usable at creation time the create fails with ErrNoCredentialForHarness
// and leaves no row — never a silent fallback to Claude. It reuses the codexTestEnv fixtures.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. Every name ends LiveDB so the store-IT sweep selects it.

// atomicMRReworkSvc mirrors atomicSvc (create_run_atomic_livedb_test.go): a Service over the live
// queries WITH the transaction beginner wired, so a derived Codex rework can freeze its binding
// atomically with the INSERT.
func atomicMRReworkSvc(env codexTestEnv) *Service {
	svc := New(env.q, env.box, testParams())
	svc.txBeginner = env.pool
	return svc
}

// countMRReworkRunsForSource counts mr_rework runs whose target_run_id is sourceRunID, so a test
// can prove a refused create left no derived row.
func (e codexTestEnv) countMRReworkRunsForSource(t *testing.T, sourceRunID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM runs WHERE kind = 'mr_rework' AND target_run_id = $1`,
		sourceRunID).Scan(&n); err != nil {
		t.Fatalf("count mr_rework runs for source: %v", err)
	}
	return n
}

// TestCreateAutoMRReworkRunInheritsCodexHarnessLiveDB (D4): a derived MR-rework of a Codex
// source run inherits harness='codex' as an explicit selection and freezes its OWN Codex
// binding in the create transaction — it does not merely copy the source's frozen columns.
func TestCreateAutoMRReworkRunInheritsCodexHarnessLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)
	// BOTH harnesses usable, no default_harness set: D11's implicit tiebreak (rule 4) would pick
	// CLAUDE. Seeding this proves the rework's harness comes from EXPLICIT source-run inheritance,
	// not merely "Codex happens to also be what implicit resolution would choose".
	seedAnthropicToken(t, env, userID)

	// The source run: seed then freeze onto the currently-usable default Codex credential,
	// exactly as newSubscriptionFixture does, keeping repoID in hand for the rework create.
	sourceRunID := env.seedCodexRun(t, userID, workerID, repoID)
	bareSvc := &Service{q: env.q, box: env.box}
	if err := bareSvc.FreezeCodexBinding(env.ctx, userID, sourceRunID, alias, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding (source): %v", err)
	}
	source := mustRun(t, env, sourceRunID)
	if source.Harness != harnessCodex {
		t.Fatalf("source run harness = %q, want codex", source.Harness)
	}

	svc := atomicMRReworkSvc(env)
	newRun, err := svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, "agent/issue-9", 90, sourceRunID, "Rework MR review", "desc", sampleReviewSnapshot())
	if err != nil {
		t.Fatalf("CreateAutoMRReworkRun: %v", err)
	}
	// Re-read: the INSERT's own RETURNING predates the in-tx freeze (freeze runs AFTER the insert,
	// same transaction), so the committed post-freeze state must be read back, exactly like
	// TestCreateRunAtomicCodexFreezesInTxLiveDB does.
	got := mustRun(t, env, newRun.ID)
	if got.Harness != string(HarnessCodex) {
		t.Fatalf("derived rework harness = %q, want codex (inherited from the source run)", got.Harness)
	}
	if !got.CodexSecretID.Valid {
		t.Fatal("derived rework must freeze its OWN codex_secret_id, got none")
	}
	if !got.CodexMaterialRevision.Valid {
		t.Fatal("derived rework must freeze codex_material_revision (the atomic in-tx freeze)")
	}
}

// TestCreateAutoMRReworkRunSourceHarnessUnusableRefusesLiveDB (D4): when the Codex source run's
// harness is NO LONGER usable at rework-creation time (its only Codex credential was removed),
// creation fails with ErrNoCredentialForHarness and creates NO run — it does not silently fall
// back to Claude.
func TestCreateAutoMRReworkRunSourceHarnessUnusableRefusesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)

	sourceRunID := env.seedCodexRun(t, userID, workerID, repoID)
	bareSvc := &Service{q: env.q, box: env.box}
	if err := bareSvc.FreezeCodexBinding(env.ctx, userID, sourceRunID, alias, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding (source): %v", err)
	}
	source := mustRun(t, env, sourceRunID)
	if source.Harness != harnessCodex {
		t.Fatalf("source run harness = %q, want codex", source.Harness)
	}

	// The user's only Codex credential is removed AFTER the source run froze it: Codex is no
	// longer usable for this user, but the source run's own harness stays 'codex' (M1's
	// deletion-proof coherence). No Anthropic token either, so the explicit inherited harness has
	// nothing to fall back to even if it tried.
	env.exec(`DELETE FROM user_secrets WHERE id = $1`, alias)

	svc := atomicMRReworkSvc(env)
	_, err := svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, "agent/issue-9", 90, sourceRunID, "Rework MR review", "desc", sampleReviewSnapshot())
	if !errors.Is(err, ErrNoCredentialForHarness) {
		t.Fatalf("CreateAutoMRReworkRun err = %v, want ErrNoCredentialForHarness (inherited codex harness now unusable, no fallback)", err)
	}
	if n := env.countMRReworkRunsForSource(t, sourceRunID); n != 0 {
		t.Fatalf("mr_rework runs for source = %d, want 0 (a refused create must leave no row)", n)
	}
}
