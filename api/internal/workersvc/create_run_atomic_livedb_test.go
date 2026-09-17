package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// create_run_atomic_livedb_test.go drives the M5B atomic create seam (createRunAtomic, PRD
// #1429 M1 / D1) DIRECTLY against a real Postgres with a manual-issue-shaped CreateRun closure.
// It proves the four D1 invariants the pure/store resolver tests structurally cannot: a Codex
// resolution freezes the binding in the SAME transaction that commits the run; a freeze failure
// rolls the whole transaction back so no run row survives; a Claude resolution commits with no
// Codex binding on the cheap path; and a nil txBeginner refuses Codex fail-closed. It reuses the
// codexTestEnv harness. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run
// via ./e2e/run-store-it.sh. Every name ends LiveDB so the store-IT sweep selects it.

// atomicSvc builds a Service over the live queries WITH the transaction beginner wired (the
// live pool satisfies TxBeginner), so createRunAtomic opens a real transaction. The
// fail-closed nil-txBeginner path is exercised by its own test with a beginner-less Service.
func atomicSvc(env codexTestEnv) *Service {
	return &Service{q: env.q, box: env.box, txBeginner: env.pool}
}

// manualIssueInsert returns a createRunAtomic insert closure shaped like a manual issue-run
// CreateRun (D1: the closure is INSERT-shape-agnostic). It stamps runs.harness from the
// resolved harness, exactly as the M2 handler will — so the committed row's harness is the D11
// result createRunAtomic resolved inside the transaction, not a caller guess.
func manualIssueInsert(env codexTestEnv, userID, repoID uuid.UUID) func(*store.Queries, resolvedHarness) (store.Run, error) {
	return func(q *store.Queries, resolved resolvedHarness) (store.Run, error) {
		return q.CreateRun(env.ctx, store.CreateRunParams{
			UserID:           userID,
			RepoID:           repoID,
			IssueIid:         pgtype.Int8{Int64: 1, Valid: true},
			IssueTitle:       "atomic-seam",
			IssueDescription: "manual issue-shaped run for the createRunAtomic seam",
			PlanSource:       planSourceAgent,
			TriggerSource:    "manual",
			Harness:          string(resolved.Harness),
		})
	}
}

// countRunsForUser reports how many runs a user owns — 0 after a rolled-back create proves the
// whole transaction (run INSERT + any freeze) committed nothing.
func (e codexTestEnv) countRunsForUser(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM runs WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

// TestCreateRunAtomicCodexFreezesInTxLiveDB (D1, case a): a Codex-only user resolves codex and
// the committed run row carries harness='codex' AND a matching codex_secret_id / auth mode /
// material revision — proving the binding freeze happened in the same transaction as the INSERT.
func TestCreateRunAtomicCodexFreezesInTxLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)

	svc := atomicSvc(env)
	run, res, err := svc.createRunAtomic(env.ctx, userID, nil, manualIssueInsert(env, userID, repoID))
	if err != nil {
		t.Fatalf("createRunAtomic: %v", err)
	}
	if res.Harness != HarnessCodex {
		t.Fatalf("resolved harness = %q, want codex", res.Harness)
	}
	got := mustRun(t, env, run.ID)
	if got.Harness != string(HarnessCodex) {
		t.Fatalf("committed harness = %q, want codex", got.Harness)
	}
	if !got.CodexSecretID.Valid || uuid.UUID(got.CodexSecretID.Bytes) != alias {
		t.Fatalf("committed codex_secret_id = %v, want the default subscription alias %s (freeze must run in-tx)", got.CodexSecretID, alias)
	}
	if got.CodexAuthMode.String != codexAuthModeSubscription {
		t.Fatalf("committed codex_auth_mode = %q, want subscription", got.CodexAuthMode.String)
	}
	if !got.CodexMaterialRevision.Valid {
		t.Fatal("committed codex_material_revision must be set (the freeze pinned it in the create tx)")
	}
}

// TestCreateRunAtomicFreezeFailureRollsBackLiveDB (D1, case b): a FORCED freeze failure rolls
// the whole transaction back, so NO runs row exists — the run INSERT that already ran inside the
// transaction is undone with the freeze.
func TestCreateRunAtomicFreezeFailureRollsBackLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)

	svc := atomicSvc(env)
	boom := errors.New("forced freeze failure")
	svc.codexFreezeFn = func(_ context.Context, _ codexFreezeStore, _, _, _ uuid.UUID, _ string) error {
		return boom
	}
	_, _, err := svc.createRunAtomic(env.ctx, userID, nil, manualIssueInsert(env, userID, repoID))
	if !errors.Is(err, boom) {
		t.Fatalf("createRunAtomic err = %v, want the forced freeze failure", err)
	}
	if n := env.countRunsForUser(t, userID); n != 0 {
		t.Fatalf("runs after rolled-back create = %d, want 0 (a freeze failure must leave no run row)", n)
	}
}

// TestCreateRunAtomicClaudeCommitsLiveDB (D1, case c): a Claude user commits harness='claude'
// with no Codex binding — no freeze ran.
func TestCreateRunAtomicClaudeCommitsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	seedAnthropicToken(t, env, userID)

	svc := atomicSvc(env)
	run, res, err := svc.createRunAtomic(env.ctx, userID, nil, manualIssueInsert(env, userID, repoID))
	if err != nil {
		t.Fatalf("createRunAtomic: %v", err)
	}
	if res.Harness != HarnessClaude {
		t.Fatalf("resolved harness = %q, want claude", res.Harness)
	}
	got := mustRun(t, env, run.ID)
	if got.Harness != string(HarnessClaude) {
		t.Fatalf("committed harness = %q, want claude", got.Harness)
	}
	if got.CodexSecretID.Valid || got.CodexMaterialRevision.Valid {
		t.Fatalf("a Claude run must carry no Codex binding: secret=%v material_rev=%v", got.CodexSecretID, got.CodexMaterialRevision)
	}
}

// TestCreateRunAtomicNilTxRefusesCodexLiveDB (D1, case d): with NO tx beginner wired a Codex
// resolution is refused fail-closed (errCodexCreateRequiresTx) and creates no run — it never
// falls back to a non-atomic insert.
func TestCreateRunAtomicNilTxRefusesCodexLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)

	// A Service WITHOUT a txBeginner.
	svc := &Service{q: env.q, box: env.box}
	_, _, err := svc.createRunAtomic(env.ctx, userID, nil, manualIssueInsert(env, userID, repoID))
	if !errors.Is(err, errCodexCreateRequiresTx) {
		t.Fatalf("createRunAtomic err = %v, want errCodexCreateRequiresTx (Codex must fail closed with no tx)", err)
	}
	if n := env.countRunsForUser(t, userID); n != 0 {
		t.Fatalf("runs after fail-closed refusal = %d, want 0", n)
	}
}

// TestCreateRunAtomicExplicitCodexUnavailableRefusesLiveDB pins the explicit-unavailable path
// through the atomic seam (D2/D4): an explicit Codex request from a Claude-only user is refused
// no_credential_for_harness and never falls back to Claude — resolution fails before the INSERT,
// so no run is created.
func TestCreateRunAtomicExplicitCodexUnavailableRefusesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	seedAnthropicToken(t, env, userID) // Claude usable, Codex not

	svc := atomicSvc(env)
	codex := HarnessCodex
	_, _, err := svc.createRunAtomic(env.ctx, userID, &codex, manualIssueInsert(env, userID, repoID))
	if !errors.Is(err, errNoCredentialForHarness) {
		t.Fatalf("createRunAtomic err = %v, want errNoCredentialForHarness (explicit codex never falls back)", err)
	}
	if n := env.countRunsForUser(t, userID); n != 0 {
		t.Fatalf("runs after explicit-unavailable refusal = %d, want 0", n)
	}
}
