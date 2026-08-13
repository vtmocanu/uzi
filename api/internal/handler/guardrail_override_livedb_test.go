package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for PRD #66 M8 (D8): the admin per-repo guardrail override. It
// reuses M4's enableGuardFixture (a real GitLab httptest server behind the real
// driver + a real privcheck.Service), so the override is exercised end to end: the
// admin route stores it on the repos row, GetRepoForUser projects the new column, and
// the M4 enable gate reads guardrail_override_reason IS NOT NULL to pass Overridden to
// GuardRepo, which downgrades the waivable findings post-evaluation.
//
// The CRITICAL invariant here is that the override NEVER waives protection_unreadable
// (D3/R8): an overridden repo whose branch-protection read errors is still refused.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// mkAdmin inserts an admin user and returns it (a real FK target for
// guardrail_override_by, and the actor RequireAdmin lets through).
func (f enableGuardFixture) mkAdmin(ctx context.Context, t *testing.T) store.User {
	t.Helper()
	u := store.User{ID: uuid.New(), Email: fmt.Sprintf("gov-admin-%s@e2e", uuid.NewString()[:8]), IsAdmin: true}
	mustExecT(ctx, t, f.pool,
		`INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`, u.ID, u.Email)
	return u
}

// setOverride POSTs the admin override for repoID as actor, returning the recorder.
func (f enableGuardFixture) setOverride(t *testing.T, actor store.User, repoID uuid.UUID, reason string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"reason": reason})
	r := httptest.NewRequest(http.MethodPost, "/admin/repos/x/guardrail-override", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), actor), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.SetRepoGuardrailOverride(w, r)
	return w
}

// clearOverride DELETEs the admin override for repoID as actor.
func (f enableGuardFixture) clearOverride(t *testing.T, actor store.User, repoID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/admin/repos/x/guardrail-override", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), actor), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.ClearRepoGuardrailOverride(w, r)
	return w
}

// overrideReason reads guardrail_override_reason (NULL → "", ok=false).
func (f enableGuardFixture) overrideReason(ctx context.Context, t *testing.T, repoID uuid.UUID) (string, bool) {
	t.Helper()
	var reason *string
	if err := f.pool.QueryRow(ctx, `SELECT guardrail_override_reason FROM repos WHERE id = $1`, repoID).Scan(&reason); err != nil {
		t.Fatalf("read override reason: %v", err)
	}
	if reason == nil {
		return "", false
	}
	return *reason, true
}

// A can-push repo is refused at the enable gate; after an admin sets the override it
// enables (200) — the waivable write_role_can_push finding is downgraded. The set
// response also carries the override metadata on the DTO (M8 exposes it for M9).
func TestGuardrailOverrideEnablesCanPushRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	admin := f.mkAdmin(ctx, t)
	repoID := f.seedRepo(ctx, t, 201, false, protCanPush)

	// Refused before the override.
	if w := f.setEnabled(t, repoID, true); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pre-override enable status = %d, want 422 (body %s)", w.Code, w.Body.String())
	}

	// Admin sets the override.
	w := f.setOverride(t, admin, repoID, "forge fix scheduled; accepting the risk until then")
	if w.Code != http.StatusOK {
		t.Fatalf("set override status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var setResp struct {
		Repo struct {
			GuardrailOverride *struct {
				Reason string `json:"reason"`
				By     string `json:"by"`
				At     string `json:"at"`
			} `json:"guardrail_override"`
		} `json:"repo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &setResp); err != nil {
		t.Fatalf("decode set-override body: %v (body %s)", err, w.Body.String())
	}
	if setResp.Repo.GuardrailOverride == nil {
		t.Fatalf("set-override DTO must expose guardrail_override metadata")
	}
	if setResp.Repo.GuardrailOverride.By != admin.ID.String() {
		t.Errorf("override.by = %q, want the admin actor id %q", setResp.Repo.GuardrailOverride.By, admin.ID.String())
	}
	if setResp.Repo.GuardrailOverride.Reason == "" || setResp.Repo.GuardrailOverride.At == "" {
		t.Errorf("override metadata must carry reason and at, got %+v", setResp.Repo.GuardrailOverride)
	}

	// Now the same repo enables.
	if w := f.setEnabled(t, repoID, true); w.Code != http.StatusOK {
		t.Fatalf("post-override enable status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !f.repoEnabled(ctx, t, repoID) {
		t.Errorf("an overridden can-push repo must be enabled after a 200")
	}
}

// THE CRITICAL TEST (D3/R8): an overridden repo whose branch-protection read ERRORS is
// STILL refused. The override downgrades only the "bot is too strong" codes, never
// protection_unreadable — a hostile or blipping forge must not pass by erroring.
func TestGuardrailOverrideNeverWaivesUnreadableLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	admin := f.mkAdmin(ctx, t)
	repoID := f.seedRepo(ctx, t, 202, false, protError)

	if w := f.setOverride(t, admin, repoID, "accepting the risk"); w.Code != http.StatusOK {
		t.Fatalf("set override status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	// Even with the override active, the enable is refused because the read errored.
	w := f.setEnabled(t, repoID, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 — the override must NOT waive protection_unreadable (body %s)", w.Code, w.Body.String())
	}
	if f.repoEnabled(ctx, t, repoID) {
		t.Errorf("an overridden repo whose protection read errors must NOT be enabled (D3/R8)")
	}
}

// Revoke re-arms the guardrail immediately: after an override enables a can-push repo,
// clearing it and re-attempting the enable is refused again.
func TestGuardrailOverrideRevokeReblocksLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	admin := f.mkAdmin(ctx, t)
	repoID := f.seedRepo(ctx, t, 203, false, protCanPush)

	if w := f.setOverride(t, admin, repoID, "temporary"); w.Code != http.StatusOK {
		t.Fatalf("set override: %d (%s)", w.Code, w.Body.String())
	}
	if _, ok := f.overrideReason(ctx, t, repoID); !ok {
		t.Fatal("override reason must be set after POST")
	}
	if w := f.setEnabled(t, repoID, true); w.Code != http.StatusOK {
		t.Fatalf("enable with override: %d (%s)", w.Code, w.Body.String())
	}
	// Disable (never gated), revoke, then re-enable must be refused.
	if w := f.setEnabled(t, repoID, false); w.Code != http.StatusOK {
		t.Fatalf("disable: %d (%s)", w.Code, w.Body.String())
	}
	if w := f.clearOverride(t, admin, repoID); w.Code != http.StatusOK {
		t.Fatalf("clear override: %d (%s)", w.Code, w.Body.String())
	}
	if _, ok := f.overrideReason(ctx, t, repoID); ok {
		t.Fatal("override reason must be NULL after DELETE (revoke NULLs all three)")
	}
	if w := f.setEnabled(t, repoID, true); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("re-enable after revoke status = %d, want 422 (revoke must re-arm the block) (body %s)", w.Code, w.Body.String())
	}
}

// A member (non-admin) hitting the override routes gets 403 via RequireAdmin — there
// is no member self-allow path (R6). Both POST and DELETE are checked.
func TestGuardrailOverrideMemberForbiddenLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 204, false, protCanPush)
	member := f.owner // the fixture owner is a plain member (IsAdmin=false)

	req := func(method string) int {
		var body *bytes.Reader
		if method == http.MethodPost {
			b, _ := json.Marshal(map[string]string{"reason": "let me in"})
			body = bytes.NewReader(b)
		} else {
			body = bytes.NewReader(nil)
		}
		r := httptest.NewRequest(method, "/admin/repos/x/guardrail-override", body)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", repoID.String())
		r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), member), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		// Exercise the RequireAdmin gate the WRITE group mounts these under.
		var h http.Handler
		if method == http.MethodPost {
			h = mw.RequireAdmin(http.HandlerFunc(f.h.SetRepoGuardrailOverride))
		} else {
			h = mw.RequireAdmin(http.HandlerFunc(f.h.ClearRepoGuardrailOverride))
		}
		h.ServeHTTP(w, r)
		return w.Code
	}

	if code := req(http.MethodPost); code != http.StatusForbidden {
		t.Errorf("member POST override status = %d, want 403", code)
	}
	if code := req(http.MethodDelete); code != http.StatusForbidden {
		t.Errorf("member DELETE override status = %d, want 403", code)
	}
	// And the override was never written.
	if _, ok := f.overrideReason(ctx, t, repoID); ok {
		t.Error("a forbidden member request must not write the override")
	}
}

// POST with an empty/whitespace reason is a 400 (the override must carry a reason).
func TestGuardrailOverrideEmptyReasonBadRequestLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	admin := f.mkAdmin(ctx, t)
	repoID := f.seedRepo(ctx, t, 205, false, protCanPush)

	if w := f.setOverride(t, admin, repoID, "   "); w.Code != http.StatusBadRequest {
		t.Fatalf("empty-reason POST status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if _, ok := f.overrideReason(ctx, t, repoID); ok {
		t.Error("a 400 empty-reason POST must not write the override")
	}
}

// An unknown repo id is a 404 on both POST and DELETE (unscoped :one RETURNING → no rows).
func TestGuardrailOverrideUnknownRepoIs404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	admin := f.mkAdmin(ctx, t)

	if w := f.setOverride(t, admin, uuid.New(), "x"); w.Code != http.StatusNotFound {
		t.Fatalf("POST unknown repo status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	if w := f.clearOverride(t, admin, uuid.New()); w.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown repo status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}
