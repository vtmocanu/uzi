package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// Issue #3719: the real-router differential-auth proof for the two Codex pool routes,
// PUT /api/me/codex-account-mode and PATCH /api/me/secrets/codex_auth/{id}/auto-eligible. Both
// are mounted under RequireUser, so a uzc_ CLI Bearer reaches them exactly as a browser session
// does, while an anonymous request is refused and a cookie request without CSRF is a 403. Each
// case also reads the stored row back: a 2xx proves nothing about what was written, and a 404
// must prove nothing was.

func seedPoolSecret(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, kind, label string, disabled bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	disabledSQL := "NULL"
	if disabled {
		disabledSQL = "now()"
	}
	cliMustExec(t, pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with, disabled_at)
		 VALUES ($1, $2, $3, $4, false, $5, 'master', `+disabledSQL+`)`,
		id, owner, kind, label+"-"+id.String()[:8], []byte("ct"))
	return id
}

func storedAutoEligible(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(context.Background(), `SELECT auto_eligible FROM user_secrets WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read auto_eligible: %v", err)
	}
	return v
}

func storedCodexMode(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(), `SELECT codex_account_mode FROM users WHERE id = $1`, user).Scan(&v); err != nil {
		t.Fatalf("read codex_account_mode: %v", err)
	}
	return v
}

// cookieNoCSRFReq is the browser path WITHOUT the CSRF header.
func cookieNoCSRFReq(router http.Handler, method, path, jwt, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestCodexAccountModeRouteDifferentialAuthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	otherBefore := storedCodexMode(t, pool, other)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	jwt := cliMintJWT(t, pool, user)
	const path = "/api/me/codex-account-mode"
	const auto = `{"codex_account_mode":"auto"}`
	const dflt = `{"codex_account_mode":"default"}`

	if m := storedCodexMode(t, pool, user); m != "default" {
		t.Fatalf("a new user starts in %q, want default", m)
	}

	// Anonymous: 401, nothing written.
	if rec := bearerReqBody(router, http.MethodPut, path, "", auto); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401: %s", rec.Code, rec.Body)
	}
	if m := storedCodexMode(t, pool, user); m != "default" {
		t.Fatalf("anonymous write changed the mode to %q", m)
	}

	// Cookie without CSRF: 403, nothing written.
	if rec := cookieNoCSRFReq(router, http.MethodPut, path, jwt, auto); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie without CSRF = %d, want 403: %s", rec.Code, rec.Body)
	}
	if m := storedCodexMode(t, pool, user); m != "default" {
		t.Fatalf("CSRF-less write changed the mode to %q", m)
	}

	// uzc_ Bearer: 200, the body names the mode, the row changed.
	rec := bearerReqBody(router, http.MethodPut, path, uzc, auto)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Mode string `json:"codex_account_mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Mode != "auto" {
		t.Fatalf("Bearer body = %s (err %v), want codex_account_mode auto", rec.Body, err)
	}
	if m := storedCodexMode(t, pool, user); m != "auto" {
		t.Fatalf("stored mode after Bearer = %q, want auto", m)
	}

	// Cookie + CSRF: 200, flips back.
	if rec := cookieReq(t, router, http.MethodPut, path, jwt, dflt); rec.Code != http.StatusOK {
		t.Fatalf("cookie+CSRF = %d, want 200: %s", rec.Code, rec.Body)
	}
	if m := storedCodexMode(t, pool, user); m != "default" {
		t.Fatalf("stored mode after cookie = %q, want default", m)
	}

	// Bad values and a missing field are 400 and write nothing.
	for _, bad := range []string{`{"codex_account_mode":"pool"}`, `{"codex_account_mode":""}`, `{}`, `{"codex_account_mode":1}`, `not json`} {
		if rec := bearerReqBody(router, http.MethodPut, path, uzc, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", bad, rec.Code)
		}
	}
	if m := storedCodexMode(t, pool, user); m != "default" {
		t.Fatalf("a rejected body changed the mode to %q", m)
	}
	// Identity is the session user: the other user's row never moved.
	if m := storedCodexMode(t, pool, other); m != otherBefore {
		t.Fatalf("another user's mode changed from %q to %q", otherBefore, m)
	}
}

func TestCodexAuthAutoEligibleRouteDifferentialAuthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	jwt := cliMintJWT(t, pool, user)

	mine := seedPoolSecret(t, pool, user, "codex_auth", "mine", false)
	foreign := seedPoolSecret(t, pool, other, "codex_auth", "foreign", false)
	openaiKey := seedPoolSecret(t, pool, user, "openai_api_key", "key", false)
	anthropic := seedPoolSecret(t, pool, user, "anthropic_token", "claude", false)
	disabled := seedPoolSecret(t, pool, user, "codex_auth", "off", true)

	codexPath := func(id uuid.UUID) string { return "/api/me/secrets/codex_auth/" + id.String() + "/auto-eligible" }
	anthropicPath := func(id uuid.UUID) string {
		return "/api/me/secrets/anthropic_token/" + id.String() + "/auto-eligible"
	}
	const on, off = `{"auto_eligible":true}`, `{"auto_eligible":false}`

	// Anonymous: 401. Cookie without CSRF: 403. Neither writes.
	if rec := bearerReqBody(router, http.MethodPatch, codexPath(mine), "", on); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", rec.Code)
	}
	if rec := cookieNoCSRFReq(router, http.MethodPatch, codexPath(mine), jwt, on); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie without CSRF = %d, want 403", rec.Code)
	}
	if storedAutoEligible(t, pool, mine) {
		t.Fatal("a refused request pooled the alias")
	}

	// uzc_ Bearer pools it.
	rec := bearerReqBody(router, http.MethodPatch, codexPath(mine), uzc, on)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Secret struct {
			ID           string `json:"id"`
			Kind         string `json:"kind"`
			AutoEligible bool   `json:"auto_eligible"`
		} `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Secret.ID != mine.String() || body.Secret.Kind != "codex_auth" || !body.Secret.AutoEligible {
		t.Fatalf("Bearer body = %s (err %v)", rec.Body, err)
	}
	if !storedAutoEligible(t, pool, mine) {
		t.Fatal("Bearer did not pool the alias")
	}

	// Cookie + CSRF un-pools it.
	if rec := cookieReq(t, router, http.MethodPatch, codexPath(mine), jwt, off); rec.Code != http.StatusOK {
		t.Fatalf("cookie+CSRF = %d, want 200: %s", rec.Code, rec.Body)
	}
	if storedAutoEligible(t, pool, mine) {
		t.Fatal("cookie+CSRF did not un-pool the alias")
	}

	// A body without the field is a 400, never a silent opt-out.
	bearerReqBody(router, http.MethodPatch, codexPath(mine), uzc, on)
	if rec := bearerReqBody(router, http.MethodPatch, codexPath(mine), uzc, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", rec.Code)
	}
	if !storedAutoEligible(t, pool, mine) {
		t.Fatal("a 400 changed the flag")
	}

	// 404 and no write: another user's alias, and ids of the wrong kind on the Codex route.
	unknown := uuid.New()
	for name, id := range map[string]uuid.UUID{"foreign alias": foreign, "openai_api_key": openaiKey, "anthropic_token": anthropic, "unknown id": unknown} {
		if rec := bearerReqBody(router, http.MethodPatch, codexPath(id), uzc, on); rec.Code != http.StatusNotFound {
			t.Errorf("%s on the Codex route = %d, want 404: %s", name, rec.Code, rec.Body)
		}
		if id != unknown && storedAutoEligible(t, pool, id) {
			t.Errorf("%s was pooled by a refused request", name)
		}
	}

	// A codex_auth id on the Anthropic route is a 404 too (not a 500), and stays unpooled.
	codexOnly := seedPoolSecret(t, pool, user, "codex_auth", "codex-only", false)
	if rec := bearerReqBody(router, http.MethodPatch, anthropicPath(codexOnly), uzc, on); rec.Code != http.StatusNotFound {
		t.Errorf("codex_auth id on the Anthropic route = %d, want 404: %s", rec.Code, rec.Body)
	}
	if storedAutoEligible(t, pool, codexOnly) {
		t.Error("the Anthropic route pooled a codex_auth alias")
	}
	// And the Anthropic route still works for its own kind.
	if rec := bearerReqBody(router, http.MethodPatch, anthropicPath(anthropic), uzc, on); rec.Code != http.StatusOK || !storedAutoEligible(t, pool, anthropic) {
		t.Errorf("Anthropic route on an anthropic_token = %d (stored %v), want 200 and pooled", rec.Code, storedAutoEligible(t, pool, anthropic))
	}

	// A disabled alias cannot be pooled (409), but un-pooling one is allowed.
	if rec := bearerReqBody(router, http.MethodPatch, codexPath(disabled), uzc, on); rec.Code != http.StatusConflict {
		t.Errorf("pooling a disabled alias = %d, want 409: %s", rec.Code, rec.Body)
	}
	if storedAutoEligible(t, pool, disabled) {
		t.Error("a disabled alias was pooled")
	}
	cliMustExec(t, pool, `UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, disabled)
	if rec := bearerReqBody(router, http.MethodPatch, codexPath(disabled), uzc, off); rec.Code != http.StatusOK || storedAutoEligible(t, pool, disabled) {
		t.Errorf("un-pooling a disabled alias = %d (stored %v), want 200 and cleared", rec.Code, storedAutoEligible(t, pool, disabled))
	}
}
