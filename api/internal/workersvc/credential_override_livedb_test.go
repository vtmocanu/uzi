package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file closes the DB-truth gap a test-adequacy pass found in PRD #1247 M1
// (prds/1247-per-run-token-selection.md): TestClaimRecordsCredentialEpochLiveDB in the
// sibling file only exercises the no-override path and claims once. Nothing before this
// file exercised the override columns, GetUserSecretMetaByIDOfKind, migration 00233/00234's
// CHECK constraints, or the epoch ON CONFLICT (run_id, claim_generation) DO UPDATE against
// a REAL Postgres. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// sealBotPAT reseals a decryptable bot PAT onto userID's forge connection.
// seedCodexInfra leaves that row with a placeholder the master box cannot decrypt, so a
// test that drives a REAL svc.Claim (which opens the forge PAT alongside the Anthropic
// credential) must reseal it first, mirroring TestClaimRecordsCredentialEpochLiveDB.
func (e codexTestEnv) sealBotPAT(t *testing.T, userID uuid.UUID) {
	t.Helper()
	sealed, err := e.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	e.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, sealed, userID)
}

// seedAnthropicSecret inserts one decryptable anthropic_token user_secret for userID and
// returns its id. Generalizes the single-token setup in credential_epoch_livedb_test.go
// to let a test seed MULTIPLE tokens for one user, which the override tests need: a
// worker-bound token and a run-pinned target distinct from it.
func (e codexTestEnv) seedAnthropicSecret(t *testing.T, userID uuid.UUID, label string, isDefault bool) uuid.UUID {
	t.Helper()
	sealed, err := e.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	id := uuid.New()
	e.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	        VALUES ($1, $2, 'anthropic_token', $3, $4, $5, 'master')`,
		id, userID, label, isDefault, sealed)
	return id
}

// seedQueuedRunWithOverride inserts a queued run carrying the given per-run credential
// override columns DIRECTLY via a store update/insert, never through the M2 create-time
// wiring (which does not exist in M1) — this is exactly what proves migration 00233/00234's
// CHECK constraints accept the write against REAL Postgres rather than a fake store.
func (e codexTestEnv) seedQueuedRunWithOverride(t *testing.T, userID, repoID uuid.UUID, mode string, secretID *uuid.UUID) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status,
	                          credential_override_mode, credential_override_secret_id)
	        VALUES ($1, $2, $3, 'issue', 101, 't', 'd', 'queued', $4, $5)`,
		runID, userID, repoID, mode, pgconv.UUIDPtr(secretID))
	return runID
}

// TestRunOverrideBeatsWorkerBindLiveDB (PRD #1247 M1, D1/D2) proves the per-run
// credential override rung resolves against REAL Postgres and outranks the claiming
// worker's own bind mode end to end: seed a user with two anthropic_token secrets (A and
// B), a worker BOUND to A, and a run whose override columns are set directly to B
// ('pinned') or to nothing ('default'); a real svc.Claim must record the OVERRIDE's
// choice, never the worker's, proving GetUserSecretMetaByIDOfKind resolved the pinned
// target under its kind-scoped predicate and the widened
// runs_anthropic_select_reason_check accepted both run_pinned and run_default.
func TestRunOverrideBeatsWorkerBindLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())

	t.Run("pinned override wins and records run_pinned", func(t *testing.T) {
		userID, workerID, repoID := env.seedCodexInfra(t)
		env.sealBotPAT(t, userID)
		tokenA := env.seedAnthropicSecret(t, userID, "bound-a-"+uuid.NewString(), true)
		tokenB := env.seedAnthropicSecret(t, userID, "pinned-b-"+uuid.NewString(), false)
		runID := env.seedQueuedRunWithOverride(t, userID, repoID, CredentialOverrideModePinned, &tokenB)

		// The worker is BOUND (pinned) to A — the ladder's worker rung, which the run
		// override must outrank.
		wkr := store.Worker{
			ID: workerID, UserID: userID, Name: "worker-pinned", Status: "online",
			AnthropicBindMode: BindModePinned,
			AnthropicSecretID: pgconv.UUID(tokenA),
		}
		payload, err := svc.Claim(env.ctx, wkr, nil)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if payload == nil || payload.RunID != runID.String() {
			t.Fatalf("Claim returned %+v, want the queued run %s", payload, runID)
		}

		run := mustRun(t, env, runID)
		if !run.AnthropicSecretID.Valid || uuid.UUID(run.AnthropicSecretID.Bytes) != tokenB {
			t.Fatalf("run.anthropic_secret_id = %+v, want the run-pinned override target %s (the worker's bind %s must have LOST)",
				run.AnthropicSecretID, tokenB, tokenA)
		}
		if run.AnthropicSelectReason.String != selectReasonRunPinned {
			t.Fatalf("run.anthropic_select_reason = %q, want %q", run.AnthropicSelectReason.String, selectReasonRunPinned)
		}
	})

	t.Run("default override wins and records run_default", func(t *testing.T) {
		userID, workerID, repoID := env.seedCodexInfra(t)
		env.sealBotPAT(t, userID)
		// A is the owner's DEFAULT; B is a distinct, non-default token the worker is
		// bound to. A run override of mode 'default' must resolve to A (the owner
		// default), NOT B (the worker binding) — proving run_default is a real,
		// distinct rung and not merely staticChoice(nil, ...) in disguise.
		tokenA := env.seedAnthropicSecret(t, userID, "owner-default-a-"+uuid.NewString(), true)
		tokenB := env.seedAnthropicSecret(t, userID, "worker-bound-b-"+uuid.NewString(), false)
		runID := env.seedQueuedRunWithOverride(t, userID, repoID, CredentialOverrideModeDefault, nil)

		wkr := store.Worker{
			ID: workerID, UserID: userID, Name: "worker-default", Status: "online",
			AnthropicBindMode: BindModePinned,
			AnthropicSecretID: pgconv.UUID(tokenB),
		}
		payload, err := svc.Claim(env.ctx, wkr, nil)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if payload == nil || payload.RunID != runID.String() {
			t.Fatalf("Claim returned %+v, want the queued run %s", payload, runID)
		}

		run := mustRun(t, env, runID)
		if !run.AnthropicSecretID.Valid || uuid.UUID(run.AnthropicSecretID.Bytes) != tokenA {
			t.Fatalf("run.anthropic_secret_id = %+v, want the owner default %s (the worker's bind %s must have LOST)",
				run.AnthropicSecretID, tokenA, tokenB)
		}
		if run.AnthropicSelectReason.String != selectReasonRunDefault {
			t.Fatalf("run.anthropic_select_reason = %q, want %q", run.AnthropicSelectReason.String, selectReasonRunDefault)
		}
	})
}

// TestRecordRunCredentialEpochIdempotentAtGenerationLiveDB (PRD #1247 M1, D7/D14) directly
// validates queries/runtime.sql's ON CONFLICT (run_id, claim_generation) DO UPDATE claim: a
// SECOND write for the SAME (run, generation) with a DIFFERENT secret/label/reason must
// update the existing epoch row in place, never duplicate it and never error. A real claim
// records the FIRST epoch at generation 1; RecordRunCredentialEpoch is then invoked directly
// a second time at that SAME generation (never ClaimRun again, which would bump the
// generation and defeat the point) with a different token.
func TestRecordRunCredentialEpochIdempotentAtGenerationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())

	userID, workerID, repoID := env.seedCodexInfra(t)
	env.sealBotPAT(t, userID)
	tokenA := env.seedAnthropicSecret(t, userID, "epoch-first-"+uuid.NewString(), true)
	tokenB := env.seedAnthropicSecret(t, userID, "epoch-second-"+uuid.NewString(), false)

	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	          VALUES ($1, $2, $3, 'issue', 202, 't', 'd', 'queued')`, runID, userID, repoID)

	wkr := store.Worker{ID: workerID, UserID: userID, Name: "worker-epoch", Status: "online"}
	payload, err := svc.Claim(env.ctx, wkr, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil || payload.RunID != runID.String() {
		t.Fatalf("Claim returned %+v, want the queued run %s", payload, runID)
	}
	genBefore, err := env.q.ListRunCredentialEpochs(env.ctx, store.ListRunCredentialEpochsParams{RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("ListRunCredentialEpochs (before): %v", err)
	}
	if len(genBefore) != 1 {
		t.Fatalf("epochs after the real claim = %d, want exactly 1: %+v", len(genBefore), genBefore)
	}
	gen := genBefore[0].ClaimGeneration
	if !genBefore[0].SecretID.Valid || uuid.UUID(genBefore[0].SecretID.Bytes) != tokenA {
		t.Fatalf("first epoch secret_id = %+v, want the claimed default token %s", genBefore[0].SecretID, tokenA)
	}

	// Re-record the SAME generation with a DIFFERENT secret/label/reason — invoking the
	// store method directly, never re-running ClaimRun (which would bump the generation
	// and could never exercise the same-generation conflict path).
	const secondLabel = "epoch-second-label"
	const secondReason = "run_pinned"
	if err := env.q.RecordRunCredentialEpoch(env.ctx, store.RecordRunCredentialEpochParams{
		RunID:           runID,
		ClaimGeneration: gen,
		SecretID:        pgconv.UUID(tokenB),
		Label:           pgconv.TextOrNull(secondLabel),
		SelectReason:    pgconv.TextOrNull(secondReason),
	}); err != nil {
		t.Fatalf("second RecordRunCredentialEpoch at the same generation: %v", err)
	}

	epochs, err := env.q.ListRunCredentialEpochs(env.ctx, store.ListRunCredentialEpochsParams{RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("ListRunCredentialEpochs (after): %v", err)
	}
	// Still EXACTLY one row for (run, generation) — the ON CONFLICT updated in place,
	// it did not insert a second row.
	if len(epochs) != 1 {
		t.Fatalf("epochs after the same-generation re-record = %d, want exactly 1 (ON CONFLICT DO UPDATE must not duplicate): %+v", len(epochs), epochs)
	}
	ep := epochs[0]
	if ep.ClaimGeneration != gen {
		t.Fatalf("epoch.claim_generation = %d, want the same generation %d", ep.ClaimGeneration, gen)
	}
	// And it reflects the SECOND write, not the first — proving DO UPDATE actually
	// overwrote the snapshot fields rather than silently no-op'ing on conflict.
	if !ep.SecretID.Valid || uuid.UUID(ep.SecretID.Bytes) != tokenB {
		t.Fatalf("epoch.secret_id = %+v after re-record, want the SECOND token %s (the first %s must have been overwritten, not kept)",
			ep.SecretID, tokenB, tokenA)
	}
	if ep.Label.String != secondLabel {
		t.Fatalf("epoch.label = %q, want the second write's %q", ep.Label.String, secondLabel)
	}
	if ep.SelectReason.String != secondReason {
		t.Fatalf("epoch.select_reason = %q, want the second write's %q", ep.SelectReason.String, secondReason)
	}
}

// TestRunOverrideRejectsWrongKindSecretLiveDB (PRD #1247 M1, D9) proves the kind-scoped
// GetUserSecretMetaByIDOfKind predicate (`AND kind = @kind`) is load-bearing against REAL
// SQL: pinning a credential_override_secret_id at a secret that EXISTS, is OWNED by the
// caller, but is of a NON-anthropic_token kind must resolve as unavailable (the kind-scoped
// query returns no row), not merely "the id doesn't exist". A positive control with a
// genuine anthropic_token secret proves the rejection is the kind predicate doing its job,
// not validateCredentialOverride refusing every id unconditionally.
func TestRunOverrideRejectsWrongKindSecretLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	userID, _, _ := env.seedCodexInfra(t)

	// A real, owned secret — but of kind openai_api_key, not anthropic_token.
	wrongKindID := uuid.New()
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
	          VALUES ($1, $2, 'openai_api_key', $3, $4, 'master')`,
		wrongKindID, userID, "wrong-kind-"+uuid.NewString(), []byte("x"))

	_, err := svc.validateCredentialOverride(env.ctx, userID, runkind.Issue, "", CredentialOverrideModePinned, &wrongKindID)
	if !errors.Is(err, ErrCredentialOverrideSecretNotFound) {
		t.Fatalf("pinning a wrong-kind (openai_api_key) secret: want ErrCredentialOverrideSecretNotFound, got %v", err)
	}

	// Positive control: the SAME user, the SAME validator, a genuine anthropic_token
	// secret — must resolve. This is what makes the rejection above meaningful: the
	// kind predicate is selective, not a blanket refusal.
	rightKindID := env.seedAnthropicSecret(t, userID, "right-kind-"+uuid.NewString(), false)
	ov, err := svc.validateCredentialOverride(env.ctx, userID, runkind.Issue, "", CredentialOverrideModePinned, &rightKindID)
	if err != nil {
		t.Fatalf("pinning a genuine anthropic_token secret: want success, got %v", err)
	}
	if ov == nil || ov.SecretID == nil || *ov.SecretID != rightKindID {
		t.Fatalf("validateCredentialOverride result = %+v, want a resolved override naming %s", ov, rightKindID)
	}
}
