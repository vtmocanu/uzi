package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// These tests deliberately use cliVaultLiveDB's full Routes router and persisted
// vault rows. A non-401 alone would also pass with an unwired vault returning 500.
const cliVaultPassword = "a-dedicated-vault-passphrase"

func cliVaultCode(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("response = %d, want %d; body: %s", rec.Code, want, rec.Body.String())
	}
}

func cliVaultStatus(t *testing.T, router http.Handler, token string, want bool) {
	t.Helper()
	rec := bearerReq(router, http.MethodGet, "/api/vault/status", token)
	cliVaultCode(t, rec, http.StatusOK)
	var body struct {
		Unlocked *bool `json:"unlocked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Unlocked == nil || *body.Unlocked != want {
		t.Fatalf("status = %s, want unlocked=%t", rec.Body.String(), want)
	}
}

// cliVaultLiveDB is cliLiveDB with a database-backed vault. Only this suite wires
// one: other cliLiveDB suites save secrets without unlocking a vault.
func cliVaultLiveDB(t *testing.T, authLimiters ...*mw.Limiter) (*Handler, http.Handler, *pgxpool.Pool) {
	t.Helper()
	h, router, pool := cliLiveDB(t, authLimiters...)
	h.vault = vault.New(h.box, h.q)
	return h, router, pool
}

func cliVaultSeed(t *testing.T, h *Handler, id uuid.UUID) {
	t.Helper()
	if err := h.vault.Unlock(context.Background(), id, cliVaultPassword); err != nil {
		t.Fatalf("create vault: %v", err)
	}
	h.vault.Lock(id)
}

func TestCLIVaultUnlockStatusLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	cliVaultSeed(t, h, owner)
	cliVaultStatus(t, router, token, false)
	cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/unlock", token,
		`{"password":"`+cliVaultPassword+`"}`), http.StatusNoContent)
	cliVaultStatus(t, router, token, true)
	// Browser lock still works and the token observes the same cache.
	cliVaultCode(t, cookieReq(t, router, http.MethodPost, "/api/vault/lock",
		cliMintJWT(t, pool, owner), ""), http.StatusNoContent)
	cliVaultStatus(t, router, token, false)
}

func TestCLIVaultOIDCPassphraseLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	cliMustExec(t, pool, "UPDATE users SET password_hash = NULL WHERE id = $1", owner)
	jwt := cliMintJWT(t, pool, owner)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	cliVaultCode(t, cookieReq(t, router, http.MethodPost, "/api/vault/passphrase", jwt,
		`{"passphrase":"`+cliVaultPassword+`"}`), http.StatusNoContent)
	if _, err := h.q.GetUserVault(context.Background(), owner); err != nil {
		t.Fatalf("passphrase did not persist vault: %v", err)
	}
	cliVaultStatus(t, router, token, true)
	cliVaultCode(t, cookieReq(t, router, http.MethodPost, "/api/vault/lock", jwt, ""), http.StatusNoContent)
	cliVaultStatus(t, router, token, false)
	cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/unlock", token,
		`{"password":"`+cliVaultPassword+`"}`), http.StatusNoContent)
	cliVaultStatus(t, router, token, true)
}

func TestCLIVaultWrongPasswordNoVaultLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t)
	owner, absent := cliSeedUser(t, pool, false), cliSeedUser(t, pool, false)
	cliVaultSeed(t, h, owner)
	ownerToken := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	absentToken := cliMintToken(t, pool, absent, clitoken.ScopeUser)
	wrong := bearerReqBody(router, http.MethodPost, "/api/vault/unlock", ownerToken, `{"password":"wrong"}`)
	missing := bearerReqBody(router, http.MethodPost, "/api/vault/unlock", absentToken,
		`{"password":"`+cliVaultPassword+`"}`)
	cliVaultCode(t, wrong, http.StatusForbidden)
	cliVaultCode(t, missing, http.StatusForbidden)
	if wrong.Body.String() != missing.Body.String() || wrong.Header().Get("Content-Type") != missing.Header().Get("Content-Type") {
		t.Fatalf("wrong password and missing vault differ: %s / %s", wrong.Body.String(), missing.Body.String())
	}
	if _, err := h.q.GetUserVault(context.Background(), absent); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unlock must not create a vault; lookup error = %v", err)
	}
	cliVaultStatus(t, router, ownerToken, false)
	cliVaultStatus(t, router, absentToken, false)
	cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/unlock", ownerToken,
		`{"password":"`+cliVaultPassword+`"}`), http.StatusNoContent)
}

func TestCLIVaultOwnerIsolationLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t)
	owner, admin := cliSeedUser(t, pool, false), cliSeedUser(t, pool, true)
	cliVaultSeed(t, h, owner)
	ownerToken := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	// A valid admin_ro credential retains IsAdmin. Vault handlers must still
	// use its owner's ID rather than expanding authority to another user's vault.
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/unlock?user_id="+owner.String(), uza,
		`{"password":"`+cliVaultPassword+`"}`), http.StatusForbidden)
	cliVaultStatus(t, router, ownerToken, false)
	cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/unlock", ownerToken,
		`{"password":"`+cliVaultPassword+`"}`), http.StatusNoContent)
	cliVaultStatus(t, router, ownerToken, true)
	cliVaultStatus(t, router, uza, false)
	if _, err := h.q.GetUserVault(context.Background(), admin); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign unlock created admin vault: %v", err)
	}
	// Positive control for the valid owner uza: it can unlock its own vault.
	cliVaultSeed(t, h, admin)
	cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/unlock", uza,
		`{"password":"`+cliVaultPassword+`"}`), http.StatusNoContent)
	cliVaultStatus(t, router, uza, true)
	h.vault.Lock(owner)
	cliVaultStatus(t, router, ownerToken, false)
	cliVaultStatus(t, router, uza, true)
}

func TestCLIVaultSharedUserLimitLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t, mw.NewLimiter(2, time.Hour, nil))
	owner, other := cliSeedUser(t, pool, false), cliSeedUser(t, pool, false)
	cliVaultSeed(t, h, owner)
	cliVaultSeed(t, h, other)
	first := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	second := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	body := `{"password":"` + cliVaultPassword + `"}`
	cliVaultCode(t, cookieReq(t, router, http.MethodPost, "/api/vault/unlock",
		cliMintJWT(t, pool, owner), body), http.StatusNoContent)
	cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/unlock", first, body), http.StatusNoContent)
	h.vault.Lock(owner)
	rec := bearerReqBody(router, http.MethodPost, "/api/vault/unlock", second, body)
	cliVaultCode(t, rec, http.StatusTooManyRequests)
	retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || retry != 3600 {
		t.Fatalf("Retry-After = %q, want 3600", rec.Header().Get("Retry-After"))
	}
	cliVaultStatus(t, router, first, false)
	// httptest requests all use the same remote address. Another user must
	// nevertheless have an independent bucket.
	cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/unlock",
		cliMintToken(t, pool, other, clitoken.ScopeUser), body), http.StatusNoContent)
	cliVaultStatus(t, router, cliMintToken(t, pool, other, clitoken.ScopeUser), true)
}

func TestCLIVaultAuthenticationCSRFLiveDB(t *testing.T) {
	// A one-request budget also proves rejected authentication/CSRF attempts
	// never consume the authenticated owner's unlock allowance.
	h, router, pool := cliVaultLiveDB(t, mw.NewLimiter(1, time.Hour, nil))
	owner := cliSeedUser(t, pool, false)
	cliVaultSeed(t, h, owner)
	jwt := cliMintJWT(t, pool, owner)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	bogus, _, _, err := clitoken.Generate(clitoken.ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"password":"` + cliVaultPassword + `"}`
	for _, endpoint := range []struct{ method, path string }{
		{http.MethodGet, "/api/vault/status"},
		{http.MethodPost, "/api/vault/unlock"},
	} {
		for _, credential := range []string{"", bogus} {
			cliVaultCode(t, bearerReqBody(router, endpoint.method, endpoint.path, credential, body), http.StatusUnauthorized)
		}
		// Even with valid cookie and CSRF, a bogus Bearer cannot fall back.
		req := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only request cookie; response security attributes do not apply.
		req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
		req.Header.Set("Authorization", "Bearer "+bogus)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		cliVaultCode(t, rec, http.StatusUnauthorized)
	}
	for _, csrf := range []string{"", "invalid"} {
		req := httptest.NewRequest(http.MethodPost, "/api/vault/unlock", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only request cookie; response security attributes do not apply.
		req.Header.Set(auth.CSRFHeaderName, csrf)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		cliVaultCode(t, rec, http.StatusForbidden)
	}
	cliVaultStatus(t, router, token, false)
	cliVaultCode(t, cookieReq(t, router, http.MethodPost, "/api/vault/unlock", jwt, body), http.StatusNoContent)
	cliVaultStatus(t, router, token, true)
}

func TestCLIVaultCookieOnlyWritesLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t)
	owner := cliSeedUser(t, pool, true)
	cliMustExec(t, pool, "UPDATE users SET password_hash = NULL WHERE id = $1", owner)
	jwt := cliMintJWT(t, pool, owner)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	uza := cliMintToken(t, pool, owner, clitoken.ScopeAdminRO)
	body := `{"passphrase":"` + cliVaultPassword + `"}`
	for _, token := range []string{uzc, uza} {
		cliVaultCode(t, bearerReqBody(router, http.MethodPost, "/api/vault/passphrase", token, body), http.StatusUnauthorized)
	}
	if _, err := h.q.GetUserVault(context.Background(), owner); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Bearer passphrase created vault: %v", err)
	}
	cliVaultCode(t, cookieReq(t, router, http.MethodPost, "/api/vault/passphrase", jwt, body), http.StatusNoContent)
	for _, token := range []string{uzc, uza} {
		cliVaultCode(t, bearerReq(router, http.MethodPost, "/api/vault/lock", token), http.StatusUnauthorized)
		cliVaultStatus(t, router, token, true)
	}
	cliVaultCode(t, cookieReq(t, router, http.MethodPost, "/api/vault/lock", jwt, ""), http.StatusNoContent)
	cliVaultStatus(t, router, uzc, false)
}
