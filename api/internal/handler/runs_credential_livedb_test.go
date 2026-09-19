package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1247 M2: the create-time credential override on POST /api/runs. These tests exercise
// the handler → workersvc.ResolveCredentialOverride → createRun seam end to end against a
// REAL Postgres, which the fake-store tier cannot: the override columns' persistence, the
// kind-scoped owner-scoped secret lookup, and the typed-error → HTTP-status mapping only hold
// once they round-trip through the schema. They reuse the M5 start-run guard fixture
// (newStartRunGuardFixture) — a real forge + real guard + real workersvc — so the happy path
// actually reaches the insert with a clean (protClean) repo.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix. A package that prints `ok` with
// PASS=0 is INVALID, not green.

// seedOwnedAnthropicToken inserts one anthropic_token user_secret owned by userID and returns
// its id. The ciphertext is a placeholder: the create path only writes the override COLUMNS
// (the secret is opened at claim time, not here), and the validator's kind-scoped lookup reads
// metadata only, so no decryptable material is needed to prove persistence.
func seedOwnedAnthropicToken(ctx context.Context, t *testing.T, f startRunGuardFixture, userID uuid.UUID, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
		 VALUES ($1, $2, 'anthropic_token', $3, $4, 'master')`,
		id, userID, label, []byte("x"))
	return id
}

// createRunWithOverride posts POST /api/runs with an explicit body (so a test can carry a
// credential_override the guard fixture's bare createRun does not). It drives the same handler
// entry point with the owner in context, exactly as f.createRun does.
func (f startRunGuardFixture) createRunWithOverride(t *testing.T, repoID uuid.UUID, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/repos/x/runs", bytes.NewReader(raw))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), f.owner), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.CreateRun(w, r)
	return w
}

// TestCreateRunCredentialOverridePersistsLiveDB (M2, D1) proves a run created through the
// create path with credential_override{mode:"pinned", secret_id:<own anthropic id>} persists
// the two override columns on the created run row — the create-time choice actually flows
// through the handler + StartRunForUser + createRun into the INSERT.
func TestCreateRunCredentialOverridePersistsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repoID := f.seedEnabledRepoWithIssue(ctx, t, 5201, 7, protClean)
	secretID := seedOwnedAnthropicToken(ctx, t, f, f.owner.ID, "pin-target-"+uuid.NewString())

	w := f.createRunWithOverride(t, repoID, map[string]any{
		"issue_iid": 7,
		"credential_override": map[string]any{
			"mode":      "pinned",
			"secret_id": secretID.String(),
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}

	// Read the created run's override columns back from Postgres — the DTO the handler
	// returns is not the evidence; the persisted row is.
	var mode string
	var secret uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT credential_override_mode, credential_override_secret_id FROM runs WHERE repo_id = $1`,
		repoID).Scan(&mode, &secret); err != nil {
		t.Fatalf("read override columns: %v", err)
	}
	if mode != "pinned" {
		t.Errorf("credential_override_mode = %q, want pinned", mode)
	}
	if secret != secretID {
		t.Errorf("credential_override_secret_id = %s, want the pinned target %s", secret, secretID)
	}
}

// TestCreateRunCredentialOverrideAbsentIsNullLiveDB (M2, back-compat) proves a create with NO
// credential_override key leaves both columns NULL — byte-identical to a pre-#1247 create, the
// web-board/chat path.
func TestCreateRunCredentialOverrideAbsentIsNullLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repoID := f.seedEnabledRepoWithIssue(ctx, t, 5202, 7, protClean)

	w := f.createRunWithOverride(t, repoID, map[string]any{"issue_iid": 7})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	var modeNull, secretNull bool
	if err := f.pool.QueryRow(ctx,
		`SELECT credential_override_mode IS NULL, credential_override_secret_id IS NULL FROM runs WHERE repo_id = $1`,
		repoID).Scan(&modeNull, &secretNull); err != nil {
		t.Fatalf("read override columns: %v", err)
	}
	if !modeNull || !secretNull {
		t.Errorf("a create with no override must leave both columns NULL, got mode_null=%v secret_null=%v", modeNull, secretNull)
	}
}

// TestCreateRunCredentialOverrideInheritClearsLiveDB (M2, D5) proves an explicit
// {mode:"inherit"} resolves to nil (clear both columns) — the token vocabulary is consistent
// with set-token even though for a create it is the same as omitting.
func TestCreateRunCredentialOverrideInheritClearsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repoID := f.seedEnabledRepoWithIssue(ctx, t, 5203, 7, protClean)

	w := f.createRunWithOverride(t, repoID, map[string]any{
		"issue_iid":           7,
		"credential_override": map[string]any{"mode": "inherit"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	var modeNull, secretNull bool
	if err := f.pool.QueryRow(ctx,
		`SELECT credential_override_mode IS NULL, credential_override_secret_id IS NULL FROM runs WHERE repo_id = $1`,
		repoID).Scan(&modeNull, &secretNull); err != nil {
		t.Fatalf("read override columns: %v", err)
	}
	if !modeNull || !secretNull {
		t.Errorf("inherit must clear both columns, got mode_null=%v secret_null=%v", modeNull, secretNull)
	}
}

// TestCreateRunCredentialOverrideRefusalsLiveDB (M2, D6/D9) covers the refusal classes the
// handler maps from the one validator's typed errors, plus the handler-level malformed-uuid
// 400. Each refusal must create NO run row. The 409 lane-not-switchable class is NOT reachable
// here — createRun only ever creates issue-kind runs — so it is covered at the validator level
// in M1 (credential_override_test.go); this asserts create-path reachable classes only.
func TestCreateRunCredentialOverrideRefusalsLiveDB(t *testing.T) {
	ctx := context.Background()

	// 404: a secret owned by ANOTHER user (foreign id) → ErrCredentialOverrideSecretNotFound.
	t.Run("foreign secret id is 404", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5210, 7, protClean)
		otherUser := uuid.New()
		mustExecT(ctx, t, f.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			otherUser, "cred-foreign-"+uuid.NewString()+"@e2e")
		foreign := seedOwnedAnthropicToken(ctx, t, f, otherUser, "foreign-"+uuid.NewString())
		w := f.createRunWithOverride(t, repoID, map[string]any{
			"issue_iid":           7,
			"credential_override": map[string]any{"mode": "pinned", "secret_id": foreign.String()},
		})
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
		}
		assertNoRun(ctx, t, f, repoID)
	})

	// 404: a secret owned by the caller but of a NON-anthropic kind (wrong kind) → same 404,
	// the kind-scoped predicate doing its job.
	t.Run("wrong-kind own secret is 404", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5211, 7, protClean)
		wrongKind := uuid.New()
		mustExecT(ctx, t, f.pool,
			`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
			 VALUES ($1, $2, 'openai_api_key', $3, $4, 'master')`,
			wrongKind, f.owner.ID, "wrong-kind-"+uuid.NewString(), []byte("x"))
		w := f.createRunWithOverride(t, repoID, map[string]any{
			"issue_iid":           7,
			"credential_override": map[string]any{"mode": "pinned", "secret_id": wrongKind.String()},
		})
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
		}
		assertNoRun(ctx, t, f, repoID)
	})

	// 422: the owner's default_harness is codex AND that Codex default is actually USABLE (a
	// linked/static credential), so D11 genuinely resolves Codex inside the create transaction
	// → ErrCredentialOverrideHarnessUnsupported, regardless of mode (the harness check precedes
	// the per-mode secret lookup). PRD #1429 M2 (D5): merely setting the raw default_harness
	// column is no longer sufficient — the effective harness now comes from a real D11
	// resolution, so an UNUSABLE Codex default falls through to Claude and ACCEPTS the
	// override instead (covered by the Claude-fallback case below).
	t.Run("codex default harness is 422", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5212, 7, protClean)
		mustExecT(ctx, t, f.pool, `UPDATE users SET default_harness = 'codex' WHERE id = $1`, f.owner.ID)
		codexSecretID := uuid.New()
		mustExecT(ctx, t, f.pool,
			`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
			 VALUES ($1, $2, 'openai_api_key', $3, true, $4, 'master')`,
			codexSecretID, f.owner.ID, "codex-key-"+uuid.NewString(), []byte("x"))
		if _, err := f.h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
			UserSecretID: codexSecretID,
			UserID:       f.owner.ID,
			Status:       "static",
		}); err != nil {
			t.Fatalf("insert codex credential state: %v", err)
		}
		w := f.createRunWithOverride(t, repoID, map[string]any{
			"issue_iid":           7,
			"credential_override": map[string]any{"mode": "auto"},
		})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
		}
		assertNoRun(ctx, t, f, repoID)
	})

	// 201: the SAME raw default_harness=codex, but WITHOUT a usable Codex credential, falls
	// through D11 to Claude — the Anthropic override is then accepted, not refused (D5's
	// "unusable Codex default falls through to Claude and accepts a valid Anthropic override").
	t.Run("unusable codex default falls through to claude and accepts override", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5215, 7, protClean)
		mustExecT(ctx, t, f.pool, `UPDATE users SET default_harness = 'codex' WHERE id = $1`, f.owner.ID)
		w := f.createRunWithOverride(t, repoID, map[string]any{
			"issue_iid":           7,
			"credential_override": map[string]any{"mode": "auto"},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
		}
	})

	// 400: a pinned mode with no secret_id → ErrCredentialOverridePinnedNeedsSecret.
	t.Run("pinned without secret is 400", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5213, 7, protClean)
		w := f.createRunWithOverride(t, repoID, map[string]any{
			"issue_iid":           7,
			"credential_override": map[string]any{"mode": "pinned"},
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		assertNoRun(ctx, t, f, repoID)
	})

	// 400: a mode outside the closed set → ErrCredentialOverrideInvalidMode.
	t.Run("unknown mode is 400", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5214, 7, protClean)
		w := f.createRunWithOverride(t, repoID, map[string]any{
			"issue_iid":           7,
			"credential_override": map[string]any{"mode": "bogus"},
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		assertNoRun(ctx, t, f, repoID)
	})

	// 400: a malformed secret_id (not a uuid) is rejected by the handler BEFORE the validator.
	t.Run("malformed secret id is 400", func(t *testing.T) {
		f := newStartRunGuardFixture(ctx, t)
		repoID := f.seedEnabledRepoWithIssue(ctx, t, 5215, 7, protClean)
		w := f.createRunWithOverride(t, repoID, map[string]any{
			"issue_iid":           7,
			"credential_override": map[string]any{"mode": "pinned", "secret_id": "not-a-uuid"},
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		assertNoRun(ctx, t, f, repoID)
	})
}

// assertNoRun asserts a refused create left no run row for the repo.
func assertNoRun(ctx context.Context, t *testing.T, f startRunGuardFixture, repoID uuid.UUID) {
	t.Helper()
	if n := f.runCount(ctx, t, repoID); n != 0 {
		t.Errorf("a refused override must not create a run, found %d run rows", n)
	}
}
