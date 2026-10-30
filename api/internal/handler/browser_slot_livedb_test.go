package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/auth"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

type browserSlotLive struct {
	h       *Handler
	router  http.Handler
	user    store.User
	cookies []*http.Cookie
	csrf    string
}

func browserSlotRouter(h *Handler) http.Handler {
	lim := mw.NewLimiter(10000, time.Hour, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim).(chi.Router)
	// Production endpoints currently omit HEAD. Mount a test-only HEAD probe
	// through the same cookie-auth constructor on the real router, so both the
	// positive and negative HEAD controls must traverse authentication.
	router.With(h.requireAuth()).Head("/api/browser-slot-head-probe", h.Me)
	return router
}
func browserSlotFixture(t *testing.T, secure bool) *browserSlotLive {
	t.Helper()
	h, _, pool := cliVaultLiveDB(t)
	h.cfg.CookieSecure = secure
	h.cfg.PasswordLoginEnabled = true
	h.wsvc.SetBackground(func(fn func()) { fn() })
	id := cliVaultUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, "DELETE FROM users WHERE id=$1", id) })
	setVaultFixturePassword(t, pool, id, cliVaultPassword)
	user, err := h.q.GetUserByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	router := browserSlotRouter(h)
	body, _ := json.Marshal(map[string]string{"email": user.Email, "password": cliVaultPassword})
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	cliVaultCode(t, rec, http.StatusOK)
	e := &browserSlotLive{h: h, router: router, user: user, cookies: rec.Result().Cookies()}
	for _, c := range e.cookies {
		if c.Name == auth.CSRFCookieName {
			e.csrf = c.Value
		}
	}
	return e
}
func (e *browserSlotLive) restart() {
	e.h.vault = vault.NewPostgres(e.h.box, e.h.pool)
	e.h.wsvc.SetVault(e.h.vault)
}

type browserSlotCountingStore struct {
	*store.Queries
	reads *atomic.Int32
	fail  error
}

func (q *browserSlotCountingStore) GetVaultBrowserSlot(ctx context.Context, p store.GetVaultBrowserSlotParams) (store.VaultBrowserSlot, error) {
	q.reads.Add(1)
	if q.fail != nil {
		return store.VaultBrowserSlot{}, q.fail
	}
	return q.Queries.GetVaultBrowserSlot(ctx, p)
}

func (e *browserSlotLive) countRestart() *atomic.Int32 {
	return e.countRestartFailing(nil)
}

// countRestartFailing makes every slot read return fail (nil reads normally).
func (e *browserSlotLive) countRestartFailing(fail error) *atomic.Int32 {
	reads := new(atomic.Int32)
	h := e.h
	h.vault = vault.New(h.box, h.q, func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
		tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		qtx := store.New(tx)
		if err := fn(vault.WithStateReader(ctx, qtx), &browserSlotCountingStore{qtx, reads, fail}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	h.wsvc.SetVault(h.vault)
	h.vaultUnlockLimiter = mw.NewLimiter(1, time.Hour, nil)
	return reads
}

func (e *browserSlotLive) request(method, path, body, csrf string, edit func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for _, c := range e.cookies {
		req.AddCookie(c)
	}
	if csrf != "" {
		req.Header.Set(auth.CSRFHeaderName, csrf)
	}
	if edit != nil {
		edit(req)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}
func (e *browserSlotLive) slots(t *testing.T) int {
	t.Helper()
	var count int
	if err := e.h.pool.QueryRow(context.Background(), "SELECT count(*) FROM vault_browser_slots WHERE user_id=$1", e.user.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestBrowserSlotRestoreRoutesLiveDB(t *testing.T) {
	e := browserSlotFixture(t, true)
	if e.slots(t) != 1 {
		t.Fatal("login did not mint exactly one slot")
	}
	var originalExpiry time.Time
	for _, c := range e.cookies {
		if c.Name == vaultSlotCookieName {
			if !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteStrictMode || c.MaxAge <= 0 {
				t.Fatal("slot attributes")
			}
			originalExpiry = c.Expires
		}
	}
	if originalExpiry.IsZero() {
		t.Fatal("slot cookie missing")
	}
	other, err := e.h.freshSession(e.user)
	if err != nil {
		t.Fatal(err)
	}
	otherRec := httptest.NewRecorder()
	other.emit(otherRec)
	otherCSRF := ""
	for _, c := range otherRec.Result().Cookies() {
		if c.Name == auth.CSRFCookieName {
			otherCSRF = c.Value
		}
	}
	for _, tc := range []struct {
		name, method, csrf string
		edit               func(*http.Request)
	}{
		{"missing GET", "GET", "", nil},
		{"missing HEAD", "HEAD", "", nil},
		{"malformed HEAD", "HEAD", "bad", nil},
		{"tampered HEAD", "HEAD", e.csrf + "x", nil},
		{"other JWT HEAD", "HEAD", otherCSRF, nil},
		{"malformed", "GET", "bad", nil},
		{"tampered", "GET", e.csrf + "x", nil},
		{"other JWT", "GET", otherCSRF, nil},
		{"same site sibling", "GET", "", func(r *http.Request) {
			r.Host = "app.example.test"
			r.Header.Set("Origin", "https://sibling.example.test")
			r.Header.Set("Sec-Fetch-Site", "same-site")
		}},
		{"passive", "GET", e.csrf, func(r *http.Request) { r.Header.Set("X-Uzi-Passive", "1") }},
		{"upgrade", "GET", e.csrf, func(r *http.Request) { r.Header.Set("Upgrade", "WebSocket") }},
		{"duplicate slot", "GET", e.csrf, func(r *http.Request) { r.Header.Add("Cookie", vaultSlotCookieName+"=bad") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := e.countRestart()
			path := "/api/auth/me"
			if tc.method == "HEAD" {
				path = "/api/browser-slot-head-probe"
			}
			rec := e.request(tc.method, path, "", tc.csrf, tc.edit)
			cliVaultCode(t, rec, http.StatusOK)
			if e.h.vault.Unlocked(e.user.ID) || reads.Load() != 0 {
				t.Fatalf("ineligible request restored or read slot: reads=%d", reads.Load())
			}
			_, available := e.h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(e.user.ID))
			if available != (tc.name != "duplicate slot") {
				t.Fatal("unexpected reservation: only valid-CSRF duplicate consumes budget")
			}
		})
	}
	for _, path := range []string{"/api/auth/me", "/api/me/cli-tokens/", "/api/browser-slot-head-probe"} {
		reads := e.countRestart()
		method := "GET"
		if path == "/api/browser-slot-head-probe" {
			method = "HEAD"
		}
		cliVaultCode(t, e.request(method, path, "", e.csrf, nil), http.StatusOK)
		if !e.h.vault.Unlocked(e.user.ID) || reads.Load() != 1 {
			t.Fatalf("%s failed to restore", path)
		}
		if e.slots(t) != 1 {
			t.Fatal("restore minted or removed slot")
		}
		cliVaultCode(t, e.request("GET", "/api/auth/me", "", e.csrf, func(r *http.Request) {
			r.Header.Add("Cookie", vaultSlotCookieName+"=malformed")
		}), http.StatusOK)
		if !e.h.vault.Unlocked(e.user.ID) || reads.Load() != 1 {
			t.Fatal("malformed cookie processed or evicted an unlocked cache")
		}
	}
	// A user-scoped Bearer always dispatches independently of browser cookies.
	token, _ := cliInsertToken(t, e.h.pool, e.user.ID, "user", nil, false)
	e.restart()
	cliVaultCode(t, e.request("GET", "/api/auth/me", "", e.csrf, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }), http.StatusOK)
	if e.h.vault.Unlocked(e.user.ID) {
		t.Fatal("Bearer restored using cookies")
	}
}

func TestBrowserSlotHTTPAndRevocationLiveDB(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		e := browserSlotFixture(t, false)
		body, _ := json.Marshal(map[string]string{"email": e.user.Email, "password": cliVaultPassword})
		rec := e.request("POST", "/api/auth/login", string(body), "", func(r *http.Request) {
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("Forwarded", "proto=https;host=app.example.test")
		})
		cliVaultCode(t, rec, http.StatusOK)
		e.cookies = rec.Result().Cookies()
		if e.slots(t) != 0 {
			t.Fatal("HTTP minted slot")
		}
		for _, c := range e.cookies {
			if c.Name == vaultSlotCookieName && c.Value != "" {
				t.Fatal("HTTP emitted secret")
			}
		}
	})
	for _, action := range []struct {
		name, method, path, body string
		code                     int
	}{
		{"logout", "POST", "/api/auth/logout", "", http.StatusOK},
		{"opt out", "PUT", "/api/me/settings", `{"vault_auto_reunlock":false}`, http.StatusOK},
		{"lock", "POST", "/api/vault/lock", "", http.StatusNoContent},
	} {
		t.Run(action.name, func(t *testing.T) {
			e := browserSlotFixture(t, true)
			e.restart()
			rec := e.request(action.method, action.path, action.body, e.csrf, nil)
			cliVaultCode(t, rec, action.code)
			if e.h.vault.Unlocked(e.user.ID) {
				t.Fatal("mutation implicitly restored")
			}
			if e.slots(t) != 0 {
				t.Fatal("mutation did not revoke slot")
			}
			cleared := false
			for _, c := range rec.Result().Cookies() {
				if c.Name == vaultSlotCookieName && c.MaxAge < 0 {
					cleared = true
				}
			}
			if !cleared {
				t.Fatal("mutation did not clear cookie")
			}
			if action.name == "opt out" {
				var body struct {
					Settings struct {
						Enabled bool `json:"vault_auto_reunlock"`
					} `json:"settings"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Settings.Enabled {
					t.Fatal("preference save not confirmed")
				}
			}
		})
	}
}

func slotCleared(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == vaultSlotCookieName && c.MaxAge < 0 && c.Path == "/" {
			return true
		}
	}
	return false
}

// A grant revoked from another browser must stop reserving the owner's unlock
// budget: the first rejected restore is charged and clears the cookie, so the
// browser's following requests reserve nothing and manual unlock stays available.
func TestBrowserSlotRevokedGrantClearsCookieLiveDB(t *testing.T) {
	a := browserSlotFixture(t, true)
	body, _ := json.Marshal(map[string]string{"email": a.user.Email, "password": cliVaultPassword})
	login := a.request("POST", "/api/auth/login", string(body), "", nil)
	cliVaultCode(t, login, http.StatusOK)
	b := &browserSlotLive{h: a.h, router: a.router, user: a.user, cookies: login.Result().Cookies()}
	for _, c := range b.cookies {
		if c.Name == auth.CSRFCookieName {
			b.csrf = c.Value
		}
	}
	cliVaultCode(t, b.request("POST", "/api/vault/lock", "", b.csrf, nil), http.StatusNoContent)
	if a.slots(t) != 0 {
		t.Fatal("lock in browser B did not revoke browser A's slot")
	}
	reads := a.countRestart()
	a.h.vaultUnlockLimiter = mw.NewLimiter(2, time.Hour, nil)
	rec := a.request("GET", "/api/auth/me", "", a.csrf, nil)
	cliVaultCode(t, rec, http.StatusOK)
	if a.h.vault.Unlocked(a.user.ID) || reads.Load() != 1 {
		t.Fatalf("revoked grant restored or was not read: reads=%d", reads.Load())
	}
	if !slotCleared(rec) {
		t.Fatal("definitive rejection kept the revoked slot cookie")
	}
	// The browser honours the deletion; later requests present no slot.
	var kept []*http.Cookie
	for _, c := range a.cookies {
		if c.Name != vaultSlotCookieName {
			kept = append(kept, c)
		}
	}
	a.cookies = kept
	for range 3 {
		cliVaultCode(t, a.request("GET", "/api/auth/me", "", a.csrf, nil), http.StatusOK)
	}
	if reads.Load() != 1 {
		t.Fatal("cleared slot was read again")
	}
	if _, ok := a.h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(a.user.ID)); !ok {
		t.Fatal("cleared slot kept consuming unlock budget")
	}
	if _, ok := a.h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(a.user.ID)); ok {
		t.Fatal("rejected restore was refunded; it must stay charged")
	}
}

// Only a definitive rejection clears the cookie. A failed slot read is transient
// and keeps it; a missing row is definitive.
func TestBrowserSlotTransientRestoreKeepsCookieLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  error
		clear bool
	}{
		{"read failure", errors.New("slot read failed"), false},
		{"missing row", pgx.ErrNoRows, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := browserSlotFixture(t, true)
			reads := e.countRestartFailing(tc.fail)
			rec := e.request("GET", "/api/auth/me", "", e.csrf, nil)
			cliVaultCode(t, rec, http.StatusOK)
			if e.h.vault.Unlocked(e.user.ID) || reads.Load() != 1 {
				t.Fatalf("failed read restored or was not attempted: reads=%d", reads.Load())
			}
			if slotCleared(rec) != tc.clear {
				t.Fatalf("cleared=%t, want %t", slotCleared(rec), tc.clear)
			}
			if e.slots(t) != 1 {
				t.Fatal("rejected restore changed stored slots")
			}
		})
	}
}

type browserEntropyFailure struct{}

func (browserEntropyFailure) Read([]byte) (int, error) {
	return 0, errors.New("synthetic entropy failure")
}

func TestBrowserSlotSessionPreparationAndStaleLoginLiveDB(t *testing.T) {
	e := browserSlotFixture(t, true)
	e.restart()
	before := e.slots(t)
	e.h.browserSessionRandom = browserEntropyFailure{}
	body, _ := json.Marshal(map[string]string{"email": e.user.Email, "password": cliVaultPassword})
	rec := e.request("POST", "/api/auth/login", string(body), "", nil)
	cliVaultCode(t, rec, http.StatusInternalServerError)
	if len(rec.Header().Values("Set-Cookie")) != 0 || e.h.vault.Unlocked(e.user.ID) || e.slots(t) != before {
		t.Fatal("session preparation failure reached unlock/mint or emitted cookies")
	}
	e.h.browserSessionRandom = nil
	// Simulate logout after Login read its authorization tuple and verified the
	// password, before it enters the combined lifecycle operation.
	snapshot := vault.Snapshot(e.user)
	if _, err := e.h.vault.Logout(context.Background(), e.user.ID); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	if e.h.passwordSession(rec, req, e.user, snapshot, cliVaultPassword) {
		t.Fatal("stale login succeeded")
	}
	cliVaultCode(t, rec, http.StatusConflict)
	if len(rec.Header().Values("Set-Cookie")) != 0 || e.h.vault.Unlocked(e.user.ID) || e.slots(t) != 0 {
		t.Fatal("stale login emitted authority, cached a DEK or survived logout")
	}
}

func TestBrowserSlotSharesManualUnlockBudgetLiveDB(t *testing.T) {
	for _, successful := range []bool{true, false} {
		t.Run(fmt.Sprintf("successful_restore_%t", successful), func(t *testing.T) {
			e := browserSlotFixture(t, true)
			lim := mw.NewLimiter(1, time.Hour, nil)
			e.router = e.h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
			e.restart()
			var edit func(*http.Request)
			if !successful {
				edit = func(r *http.Request) { r.Header.Add("Cookie", vaultSlotCookieName+"=bad") }
			}
			cliVaultCode(t, e.request("GET", "/api/auth/me", "", e.csrf, edit), http.StatusOK)
			if e.h.vault.Unlocked(e.user.ID) != successful {
				t.Fatal("unexpected cache state after restore")
			}
			want := http.StatusTooManyRequests
			if successful {
				want = http.StatusNoContent
			}
			cliVaultCode(t, e.request("POST", "/api/vault/unlock", `{"password":"`+cliVaultPassword+`"}`, e.csrf, nil), want)
		})
	}
}

func TestBrowserSlotLegacyRefreshExpiryLiveDB(t *testing.T) {
	testBrowserSlotRefreshExpiry(t, true)
}
func TestBrowserSlotStableRefreshExpiryLiveDB(t *testing.T) {
	testBrowserSlotRefreshExpiry(t, false)
}
func testBrowserSlotRefreshExpiry(t *testing.T, legacy bool) {
	e := browserSlotFixture(t, true)
	// Use a legacy JWT in its refresh window. Unlock must upgrade its identity
	// without extending the slot to the rolling refresh's outgoing expiry.
	incoming := auth.Claims{UserID: e.user.ID.String(), TokenVersion: e.user.TokenVersion, RegisteredClaims: jwt.RegisteredClaims{
		Subject: e.user.ID.String(), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)), ExpiresAt: jwt.NewNumericDate(time.Now().Add(10 * time.Minute)),
	}}
	if !legacy {
		for _, c := range e.cookies {
			if c.Name == auth.AuthCookieName {
				claims, err := auth.ParseToken(e.h.cfg.JWTSecret, c.Value)
				if err != nil {
					t.Fatal(err)
				}
				incoming.BrowserSlotID = claims.BrowserSlotID
			}
		}
	}
	token, err := auth.SignClaims(e.h.cfg.JWTSecret, incoming)
	if err != nil {
		t.Fatal(err)
	}
	cookies := httptest.NewRecorder()
	if err := auth.SetAuthCookies(cookies, token, auth.CookieOptions{Secure: true, TTL: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	e.cookies = cookies.Result().Cookies()
	for _, c := range e.cookies {
		if c.Name == auth.CSRFCookieName {
			e.csrf = c.Value
		}
	}
	rec := e.request("POST", "/api/vault/unlock", `{"password":"`+cliVaultPassword+`"}`, e.csrf, nil)
	cliVaultCode(t, rec, http.StatusNoContent)
	var upgraded *auth.Claims
	var slotCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.AuthCookieName {
			upgraded, err = auth.ParseToken(e.h.cfg.JWTSecret, c.Value)
			if err != nil {
				t.Fatal(err)
			}
		}
		if c.Name == vaultSlotCookieName {
			slotCookie = c
		}
	}
	if upgraded == nil || upgraded.BrowserSlotID == "" || (!legacy && upgraded.BrowserSlotID != incoming.BrowserSlotID) {
		t.Fatal("browser identity missing or changed")
	}
	if legacy && !upgraded.ExpiresAt.Equal(incoming.ExpiresAt.Time) {
		t.Fatal("legacy identity upgrade extended incoming expiry")
	}
	if !legacy && !upgraded.ExpiresAt.After(incoming.ExpiresAt.Time) {
		t.Fatal("ordinary rolling refresh did not extend session")
	}
	if slotCookie == nil || slotCookie.Expires.After(incoming.ExpiresAt.Time) {
		t.Fatal("slot exceeded incoming expiry")
	}
	browserID, err := uuid.Parse(upgraded.BrowserSlotID)
	if err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	if err := e.h.pool.QueryRow(context.Background(), "SELECT expires_at FROM vault_browser_slots WHERE user_id=$1 AND browser_id=$2", e.user.ID, browserID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(incoming.ExpiresAt.Time) {
		t.Fatal("stored expiry changed")
	}
	if !legacy {
		// Restore uses the incoming JWT/header before middleware rolls cookies.
		e.cookies = append(cookies.Result().Cookies(), slotCookie)
		oldHeader := e.csrf
		reads := e.countRestart()
		refreshed := e.request("GET", "/api/auth/me", "", oldHeader, nil)
		cliVaultCode(t, refreshed, http.StatusOK)
		if reads.Load() != 1 || !e.h.vault.Unlocked(e.user.ID) {
			t.Fatal("incoming half-life session did not restore before refresh")
		}
		e.cookies = append(refreshed.Result().Cookies(), slotCookie)
		newHeader := ""
		for _, c := range e.cookies {
			if c.Name == auth.CSRFCookieName {
				newHeader = c.Value
			}
		}
		if newHeader == "" || newHeader == oldHeader {
			t.Fatal("refresh did not rotate CSRF")
		}
		reads = e.countRestart()
		cliVaultCode(t, e.request("GET", "/api/auth/me", "", oldHeader, nil), http.StatusOK)
		if reads.Load() != 0 || e.h.vault.Unlocked(e.user.ID) {
			t.Fatal("old header restored with refreshed JWT")
		}
		if _, ok := e.h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(e.user.ID)); !ok {
			t.Fatal("old header consumed budget")
		}
		reads = e.countRestart()
		cliVaultCode(t, e.request("GET", "/api/auth/me", "", newHeader, nil), http.StatusOK)
		if reads.Load() != 1 || !e.h.vault.Unlocked(e.user.ID) {
			t.Fatal("refreshed header/JWT did not restore old slot")
		}
		var after time.Time
		if err := e.h.pool.QueryRow(context.Background(), "SELECT expires_at FROM vault_browser_slots WHERE user_id=$1 AND browser_id=$2", e.user.ID, browserID).Scan(&after); err != nil || !after.Equal(expires) {
			t.Fatal("refresh/restore changed persisted slot expiry")
		}
	}
}

func TestBrowserSlotRejectedSessionsLiveDB(t *testing.T) {
	e := browserSlotFixture(t, true)
	original := e.cookies
	var slot *http.Cookie
	var claims *auth.Claims
	for _, c := range original {
		if c.Name == vaultSlotCookieName {
			slot = c
		}
		if c.Name == auth.AuthCookieName {
			var err error
			claims, err = auth.ParseToken(e.h.cfg.JWTSecret, c.Value)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if slot == nil || claims == nil {
		t.Fatal("missing login cookies")
	}
	foreign := cliVaultUser(t, e.h.pool, false)
	t.Cleanup(func() { cliMustExec(t, e.h.pool, "DELETE FROM users WHERE id=$1", foreign) })
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	sentinel := strings.Repeat("B", 42) + "A"
	e.countRestart()
	baseline := e.request("GET", "/api/auth/me", "", "", nil).Body.String()
	for _, name := range []string{"slot alone", "expired", "revoked", "other owner", "other browser", "unsafe missing csrf", "wrong key", "malformed key"} {
		t.Run(name, func(t *testing.T) {
			e.cookies = original
			csrf, method, path, code := e.csrf, "GET", "/api/auth/me", http.StatusOK
			copyClaims := *claims
			switch name {
			case "slot alone":
				e.cookies, code = []*http.Cookie{slot}, http.StatusUnauthorized
			case "expired", "revoked", "other owner", "other browser":
				switch name {
				case "expired":
					copyClaims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
					code = http.StatusUnauthorized
				case "revoked":
					copyClaims.TokenVersion++
					code = http.StatusUnauthorized
				case "other owner":
					copyClaims.UserID = foreign.String()
					u, err := e.h.q.GetUserByID(context.Background(), foreign)
					if err != nil {
						t.Fatal(err)
					}
					copyClaims.TokenVersion = u.TokenVersion
				case "other browser":
					copyClaims.BrowserSlotID = uuid.NewString()
				}
				token, err := auth.SignClaims(e.h.cfg.JWTSecret, copyClaims)
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				if err := auth.SetAuthCookies(rec, token, auth.CookieOptions{Secure: true, TTL: time.Hour}); err != nil {
					t.Fatal(err)
				}
				e.cookies = append(rec.Result().Cookies(), slot)
				for _, c := range e.cookies {
					if c.Name == auth.CSRFCookieName {
						csrf = c.Value
					}
				}
			case "unsafe missing csrf":
				method, path, csrf, code = "POST", "/api/me/cli-tokens/", "", http.StatusForbidden
			case "wrong key", "malformed key":
				bad := *slot //nolint:gosec // G124: copy a real Secure/HttpOnly/Strict response cookie; only the test key is corrupted.
				bad.Value = slot.Value[:40] + sentinel
				if name == "malformed key" {
					bad.Value = "malformed-" + sentinel
				}
				e.cookies = nil
				for _, c := range original {
					if c.Name != vaultSlotCookieName {
						e.cookies = append(e.cookies, c)
					}
				}
				e.cookies = append(e.cookies, &bad)
			}
			logs.Reset()
			reads := e.countRestart()
			rec := e.request(method, path, "", csrf, nil)
			cliVaultCode(t, rec, code)
			if e.h.vault.Unlocked(e.user.ID) || e.h.vault.Unlocked(foreign) {
				t.Fatal("rejected session restored vault")
			}
			if (name == "slot alone" || name == "expired" || name == "revoked" || name == "unsafe missing csrf") && reads.Load() != 0 {
				t.Fatal("unauthenticated/CSRF-rejected request read slot")
			}
			if name == "wrong key" || name == "malformed key" {
				if rec.Body.String() != baseline {
					t.Fatal("bad slot changed ordinary response")
				}
				if logs.Len() == 0 || strings.Contains(logs.String(), sentinel) || strings.Contains(rec.Body.String(), sentinel) {
					t.Fatal("missing rejection log or disclosed cookie sentinel")
				}
			}
		})
	}
}

func TestBrowserSlotRestorePromotesParkedRunLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	h, poker, _ := vaultRecoveryHandler(t, e, true)
	h.cfg.CookieSecure = true
	h.cfg.JWTSecret = []byte("browser-slot-test-signing-key")
	h.cfg.AuthTokenTTL = time.Hour
	h.settings = settings.New(h.q, time.Minute)
	user, err := h.q.GetUserByID(e.ctx, e.user)
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.freshSession(user)
	if err != nil {
		t.Fatal(err)
	}
	cookies := httptest.NewRecorder()
	session.emit(cookies)
	b := &browserSlotLive{h: h, router: browserSlotRouter(h), user: user, cookies: cookies.Result().Cookies()}
	for _, c := range b.cookies {
		if c.Name == auth.CSRFCookieName {
			b.csrf = c.Value
		}
	}
	rec := b.request("POST", "/api/vault/unlock", `{"password":"`+recoveryUnlockPassword+`"}`, b.csrf, nil)
	cliVaultCode(t, rec, http.StatusNoContent)
	for _, c := range rec.Result().Cookies() {
		if c.Name == vaultSlotCookieName {
			b.cookies = append(b.cookies, c)
		}
	}
	other, err := h.freshSession(user)
	if err != nil {
		t.Fatal(err)
	}
	otherRec := httptest.NewRecorder()
	other.emit(otherRec)
	var otherCSRF string
	for _, c := range otherRec.Result().Cookies() {
		if c.Name == auth.CSRFCookieName {
			otherCSRF = c.Value
		}
	}
	if otherCSRF == "" || otherCSRF == b.csrf {
		t.Fatal("other-JWT CSRF negative control missing")
	}
	var runs []uuid.UUID
	for i, tc := range []struct {
		name, method, csrf string
		edit               func(*http.Request)
	}{
		{"missing GET", "GET", "", nil},
		{"missing HEAD", "HEAD", "", nil},
		{"malformed GET", "GET", "bad", nil},
		{"malformed HEAD", "HEAD", "bad", nil},
		{"tampered GET", "GET", b.csrf + "x", nil},
		{"tampered HEAD", "HEAD", b.csrf + "x", nil},
		{"other JWT GET", "GET", otherCSRF, nil},
		{"other JWT HEAD", "HEAD", otherCSRF, nil},
		{"same site sibling GET", "GET", "", func(r *http.Request) {
			r.Host = "app.example.test"
			r.Header.Set("Origin", "https://sibling.example.test")
			r.Header.Set("Sec-Fetch-Site", "same-site")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Give every counterexample fresh parked work and a fresh notice, so
			// an earlier unsafe promotion cannot contaminate a later assertion.
			run := seedVaultRecoveryRow(t, e, e.user, "recovery_wait", "vault_locked", nil, 77+i)
			runs = append(runs, run)
			cliMustExec(t, e.pool, "UPDATE user_vaults SET lock_notified_at=now() WHERE user_id=$1", e.user)
			before, err := h.q.GetUserVault(e.ctx, e.user)
			if err != nil {
				t.Fatal(err)
			}
			beforeRun := vaultRecoveryRow(t, e, run)
			beforePokes := poker.count(e.user)
			reads := b.countRestart()
			path := "/api/auth/me"
			if tc.method == "HEAD" {
				path = "/api/browser-slot-head-probe"
			}
			cliVaultCode(t, b.request(tc.method, path, "", tc.csrf, tc.edit), http.StatusOK)
			after, err := h.q.GetUserVault(e.ctx, e.user)
			if err != nil {
				t.Fatal(err)
			}
			parkUnchanged := reflect.DeepEqual(beforeRun, vaultRecoveryRow(t, e, run))
			noticeUnchanged := after.LockNotifiedAt == before.LockNotifiedAt && after.LockNotifiedAt.Valid
			if !parkUnchanged || !noticeUnchanged {
				t.Fatalf("ineligible probe changed recovery or notice: parked_unchanged=%t notice_unchanged=%t", parkUnchanged, noticeUnchanged)
			}
			if h.vault.Unlocked(e.user) || reads.Load() != 0 || poker.count(e.user) != beforePokes {
				t.Fatal("ineligible probe restored, accessed a slot, or triggered vault-ready effects")
			}
			if _, available := h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(e.user)); !available {
				t.Fatal("ineligible probe reserved unlock budget")
			}
		})
	}
	reads := b.countRestart()
	beforePokes := poker.count(e.user)
	cliVaultCode(t, b.request("GET", "/api/auth/me", "", b.csrf, nil), http.StatusOK)
	after, err := h.q.GetUserVault(e.ctx, e.user)
	if err != nil || after.LockNotifiedAt.Valid {
		t.Fatal("successful restore did not clear notice")
	}
	if !h.vault.Unlocked(e.user) || reads.Load() != 1 || poker.count(e.user) <= beforePokes {
		t.Fatal("authorized foreground probe did not restore and trigger vault-ready effects")
	}
	for _, run := range runs {
		var status string
		if err := e.pool.QueryRow(e.ctx, "SELECT status FROM runs WHERE id=$1", run).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "queued" {
			t.Fatalf("restored run status=%s", status)
		}
	}
}
