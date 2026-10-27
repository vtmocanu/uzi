package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/auth"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func vaultToggleRouter(h *Handler, burst int) http.Handler {
	r := chi.NewRouter()
	limiter := mw.NewLimiter(burst, time.Hour, nil)
	r.Route("/api", func(r chi.Router) {
		h.mountVaultRoutes(r, limiter)
		h.mountAuthRoutes(r, limiter, limiter)
	})
	return r
}

// Use production JWT and cookie/CSRF generation, then the actual mounted routes.
func vaultRouteRequest(t *testing.T, router http.Handler, h *Handler, user store.User, method, path, body string, signed, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if signed {
		token, err := auth.IssueToken(h.cfg.JWTSecret, user.ID.String(), user.TokenVersion, h.cfg.AuthTokenTTL)
		if err != nil {
			t.Fatal(err)
		}
		cookies := httptest.NewRecorder()
		if err := auth.SetAuthCookies(cookies, token, auth.CookieOptions{Secure: false, TTL: h.cfg.AuthTokenTTL}); err != nil {
			t.Fatal(err)
		}
		for _, cookie := range cookies.Result().Cookies() {
			req.AddCookie(cookie)
			if csrf && cookie.Name == auth.CSRFCookieName {
				req.Header.Set(auth.CSRFHeaderName, cookie.Value)
			}
		}
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func assertVaultRouteSnapshot(t *testing.T, router http.Handler, h *Handler, user store.User, want vault.Status) {
	t.Helper()
	rec := vaultRouteRequest(t, router, h, user, http.MethodGet, "/api/vault/status", "", true, false)
	var status vault.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || rec.Code != http.StatusOK || status != want {
		t.Fatalf("status=%d snapshot=%+v want=%+v err=%v", rec.Code, status, want, err)
	}
	rec = vaultRouteRequest(t, router, h, user, http.MethodGet, "/api/auth/me", "", true, false)
	var session struct{ Vault vault.Status }
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil || rec.Code != http.StatusOK || session.Vault != want {
		t.Fatalf("auth/me=%d snapshot=%+v want=%+v err=%v", rec.Code, session.Vault, want, err)
	}
}

func TestVaultToggleRoutesLifecycleLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	enrollmentPolicy(t, pool, "optional")
	ctx := context.Background()
	q := store.New(pool)
	h := &Handler{pool: pool, q: q, cfg: oidcLiveCfg(), box: oidcLiveBox(t), settings: settings.New(q, time.Hour)}
	h.vault = vault.NewPostgres(h.box, pool)
	poker := &fakeCodexPoker{}
	h.SetCodexUsagePoker(poker)
	email := uuid.NewString() + "@example.com"
	const password = "route-toggle-password" //nolint:gosec // G101: synthetic password for the isolated route fixture.
	register, _ := json.Marshal(map[string]string{"email": email, "password": password})
	rec := httptest.NewRecorder()
	h.Register(rec, httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(string(register))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register=%d", rec.Code)
	}
	user, err := q.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	router := vaultToggleRouter(h, 30)
	off := vault.Status{Mode: store.VaultModeUnprotected, Policy: "optional"}
	assertVaultRouteSnapshot(t, router, h, user, off)
	sealed, err := h.vault.Seal(ctx, user.ID, store.KindOpenAIAPIKey, []byte("private-fixture-material"), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with) VALUES ($1,$2,'default',$3,$4)", user.ID, store.KindOpenAIAPIKey, sealed.Ciphertext, sealed.SealedWith)
	var before []byte
	if err := pool.QueryRow(ctx, "SELECT ciphertext FROM user_secrets WHERE user_id=$1", user.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/vault/enable", "/api/vault/disable"} {
		for _, tc := range []struct {
			signed, csrf bool
			code         int
		}{{false, false, http.StatusUnauthorized}, {true, false, http.StatusForbidden}} {
			rec := vaultRouteRequest(t, router, h, user, http.MethodPost, path, `{"password":"route-toggle-password"}`, tc.signed, tc.csrf)
			if rec.Code != tc.code {
				t.Fatalf("%s signed=%v csrf=%v status=%d", path, tc.signed, tc.csrf, rec.Code)
			}
		}
	}
	rec = vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/enable", `{"password":"wrong-password"}`, true, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong enable=%d", rec.Code)
	}
	after := reloadUser(t, pool, user)
	var unchanged []byte
	if err := pool.QueryRow(ctx, "SELECT ciphertext FROM user_secrets WHERE user_id=$1", user.ID).Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	if after.VaultMode != user.VaultMode || after.VaultEpoch != user.VaultEpoch || !reflect.DeepEqual(before, unchanged) || poker.count(user.ID) != 0 {
		t.Fatal("wrong enable password changed durable state or poked")
	}
	assertVaultRouteSnapshot(t, router, h, user, off)

	// An Off unlock must not create a wrapper or change the durable epoch.
	rec = vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/unlock", `{"password":"route-toggle-password"}`, true, true)
	if rec.Code != http.StatusNoContent || reloadUser(t, pool, user).VaultEpoch != user.VaultEpoch {
		t.Fatalf("off unlock=%d", rec.Code)
	}
	assertVaultRouteSnapshot(t, router, h, user, off)
	rec = vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/enable", `{"password":"route-toggle-password"}`, true, true)
	var action vaultToggleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &action); err != nil || rec.Code != http.StatusOK || action.Mode != store.VaultModeProtected || !strings.Contains(action.RotationAdvice, "backups") {
		t.Fatalf("enable=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
	for _, forbidden := range []string{password, "private-fixture-material", "ciphertext", "wrapped_dek", "kek_salt", user.PasswordHash.String} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("enable leaked %q", forbidden)
		}
	}
	on := vault.Status{Mode: store.VaultModeProtected, Policy: "optional", Exists: true, Unlocked: true}
	assertVaultRouteSnapshot(t, router, h, user, on)
	if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/enable", `{"password":"route-toggle-password"}`, true, true); rec.Code != http.StatusConflict {
		t.Fatalf("existing wrapper enable=%d", rec.Code)
	}
	h.vault.Lock(user.ID)
	on.Unlocked = false
	assertVaultRouteSnapshot(t, router, h, user, on)
	if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/disable", "", true, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "unlock") {
		t.Fatalf("locked disable=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/unlock", `{"password":"route-toggle-password"}`, true, true); rec.Code != http.StatusNoContent {
		t.Fatalf("unlock=%d", rec.Code)
	}
	// Cache optional first, then change DB policy: status and disable use core's
	// authoritative policy, even while display settings remain cached optional.
	if policy, err := h.settings.VaultPolicy(ctx); err != nil || policy != "optional" {
		t.Fatalf("cached policy=%s err=%v", policy, err)
	}
	mustExecT(ctx, t, pool, "UPDATE app_settings SET value='required' WHERE key='vault_policy'")
	on.Unlocked, on.Policy = true, "required"
	assertVaultRouteSnapshot(t, router, h, user, on)
	epoch := reloadUser(t, pool, user).VaultEpoch
	if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/disable", "", true, true); rec.Code != http.StatusForbidden || reloadUser(t, pool, user).VaultEpoch != epoch {
		t.Fatalf("required disable=%d", rec.Code)
	}
	mustExecT(ctx, t, pool, "UPDATE app_settings SET value='optional' WHERE key='vault_policy'")
	if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/disable", "", true, true); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "rotation_advice") {
		t.Fatalf("disable=%d body=%s", rec.Code, rec.Body.String())
	}
	assertVaultRouteSnapshot(t, router, h, user, off)
	after = reloadUser(t, pool, user)
	if after.VaultEpoch != 2 || h.vault.Unlocked(user.ID) || h.vault.RequiresUnlock(ctx, user.ID) {
		t.Fatalf("disabled epoch=%d", after.VaultEpoch)
	}
	var ciphertext []byte
	var marker string
	if err := pool.QueryRow(ctx, "SELECT ciphertext,sealed_with FROM user_secrets WHERE user_id=$1", user.ID).Scan(&ciphertext, &marker); err != nil {
		t.Fatal(err)
	}
	plain, err := h.vault.OpenAtEpoch(ctx, user.ID, store.KindOpenAIAPIKey, marker, ciphertext, after.VaultEpoch)
	if err != nil || marker != store.SealedWithMasterAAD || string(plain) != "private-fixture-material" {
		t.Fatalf("disabled material marker=%s err=%v", marker, err)
	}
	mustExecT(ctx, t, pool, "UPDATE app_settings SET value='required' WHERE key='vault_policy'")
	off.Policy = "required"
	assertVaultRouteSnapshot(t, router, h, user, off)
	if h.vault.RequiresUnlock(ctx, user.ID) {
		t.Fatal("existing Off under required policy must remain runnable")
	}
}

func TestVaultToggleRoutesOIDCAndLimiterLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	enrollmentPolicy(t, pool, "optional")
	q := store.New(pool)
	h := &Handler{pool: pool, q: q, cfg: oidcLiveCfg(), box: oidcLiveBox(t), settings: settings.New(q, time.Hour)}
	h.vault = vault.NewPostgres(h.box, pool)
	ctx := context.Background()
	seed := func() store.User {
		id := uuid.New()
		mustExecT(ctx, t, pool, "INSERT INTO users (id,email,password_hash,vault_mode) VALUES ($1,$2,NULL,'unprotected')", id, id.String()+"@example.com")
		u, err := q.GetUserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	user, other := seed(), seed()
	router := vaultToggleRouter(h, 20)
	for _, passphrase := range []string{strings.Repeat("x", minPasswordLen-1), strings.Repeat("x", maxPasswordLen+1)} {
		for _, path := range []string{"/api/vault/enable", "/api/vault/passphrase"} {
			body, _ := json.Marshal(map[string]string{"password": passphrase})
			if path == "/api/vault/passphrase" {
				body, _ = json.Marshal(map[string]string{"passphrase": passphrase})
			}
			if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, path, string(body), true, true); rec.Code != http.StatusBadRequest {
				t.Fatalf("%s length %d=%d", path, len(passphrase), rec.Code)
			}
		}
	}
	if reloadUser(t, pool, user).VaultEpoch != 0 {
		t.Fatal("length rejection changed epoch")
	}
	if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/enable", `{"password":"oidc-route-passphrase"}`, true, true); rec.Code != http.StatusOK {
		t.Fatalf("OIDC enable=%d body=%s", rec.Code, rec.Body.String())
	}
	h.vault.Lock(user.ID)
	if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/passphrase", `{"passphrase":"replacement-passphrase"}`, true, true); rec.Code != http.StatusConflict {
		t.Fatalf("OIDC replacement=%d", rec.Code)
	}
	if rec := vaultRouteRequest(t, router, h, user, http.MethodPost, "/api/vault/unlock", `{"password":"oidc-route-passphrase"}`, true, true); rec.Code != http.StatusNoContent {
		t.Fatalf("OIDC original unlock=%d", rec.Code)
	}
	// A body target can never redirect the action away from the authenticated owner.
	if rec := vaultRouteRequest(t, router, h, other, http.MethodPost, "/api/vault/disable", `{"user_id":"`+user.ID.String()+`"}`, true, true); rec.Code != http.StatusOK {
		t.Fatalf("other disable=%d", rec.Code)
	}
	assertVaultRouteSnapshot(t, router, h, user, vault.Status{Mode: store.VaultModeProtected, Policy: "optional", Exists: true, Unlocked: true})

	for _, action := range []string{"enable", "disable"} {
		// Each endpoint case owns a fresh limiter; both endpoints share its per-user bucket.
		limited := vaultToggleRouter(h, 1)
		path := "/api/vault/" + action
		first := vaultRouteRequest(t, limited, h, user, http.MethodPost, path, `{"password":"oidc-route-passphrase"}`, true, true)
		if first.Code == http.StatusTooManyRequests {
			t.Fatal("first request throttled")
		}
		second := vaultRouteRequest(t, limited, h, user, http.MethodPost, path, `{"password":"oidc-route-passphrase"}`, true, true)
		if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") == "" {
			t.Fatalf("%s burst=%d", action, second.Code)
		}
		independent := vaultRouteRequest(t, limited, h, other, http.MethodPost, path, `{"password":"another-oidc-passphrase"}`, true, true)
		if independent.Code == http.StatusTooManyRequests {
			t.Fatal("different user shared limiter bucket")
		}
	}
}
