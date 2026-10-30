package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestBrowserSlotCookieParsing(t *testing.T) {
	value := "v1." + uuid.NewString() + "." + strings.Repeat("A", 43)
	for _, tc := range []struct {
		name             string
		headers          []string
		presented, valid bool
	}{
		{"absent", []string{"other=value"}, false, false},
		{"valid", []string{"other=x; " + vaultSlotCookieName + "=" + value}, true, true},
		{"duplicate headers", []string{vaultSlotCookieName + "=" + value, vaultSlotCookieName + "=" + value}, true, false},
		{"duplicate same header", []string{vaultSlotCookieName + "=" + value + "; " + vaultSlotCookieName + "=" + value}, true, false},
		{"oversize", []string{vaultSlotCookieName + "=" + strings.Repeat("x", 4096)}, true, false},
		{"empty", []string{vaultSlotCookieName + "="}, true, false},
		{"missing equals", []string{vaultSlotCookieName}, true, false},
		{"quoted", []string{vaultSlotCookieName + "=\"" + value + "\""}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/auth/me", nil)
			for _, header := range tc.headers {
				req.Header.Add("Cookie", header)
			}
			_, presented, valid := slotCookie(req)
			if presented != tc.presented || valid != tc.valid {
				t.Fatalf("presented=%t valid=%t", presented, valid)
			}
		})
	}
}

func TestBrowserHookGuardsPreserveUnlockBudget(t *testing.T) {
	for _, tc := range []struct{ name, method, path, csrf, passive, upgrade, browser string }{
		{"missing csrf", "GET", "/api/auth/me", "", "", "", "valid"},
		{"head missing csrf", "HEAD", "/api/auth/me", "", "", "", "valid"},
		{"malformed csrf", "GET", "/api/auth/me", "bad", "", "", "valid"},
		{"passive", "GET", "/api/auth/me", "valid", "1", "", "valid"},
		{"websocket", "GET", "/api/auth/me", "valid", "", "WebSocket", "valid"},
		{"ws path", "GET", "/api/ws", "valid", "", "", "valid"},
		{"legacy", "GET", "/api/auth/me", "valid", "", "", ""},
		{"noncanonical", "GET", "/api/auth/me", "valid", "", "", "bad"},
		{"unlock", "POST", "/api/vault/unlock", "valid", "", "", "valid"},
		{"lock", "POST", "/api/vault/lock", "valid", "", "", "valid"},
		{"enable", "POST", "/api/vault/enable", "valid", "", "", "valid"},
		{"disable", "POST", "/api/vault/disable", "valid", "", "", "valid"},
		{"passphrase", "POST", "/api/vault/passphrase", "valid", "", "", "valid"},
		{"logout", "POST", "/api/auth/logout", "valid", "", "", "valid"},
		{"settings", "PUT", "/api/me/settings", "valid", "", "", "valid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := store.User{ID: uuid.New(), IsActive: true}
			h := &Handler{vault: testVault(t), cfg: config.Config{CookieSecure: true, JWTSecret: []byte("test-signing-key"), AuthTokenTTL: time.Hour}, vaultUnlockLimiter: mw.NewLimiter(1, time.Hour, nil)}
			session, err := h.freshSession(user)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			session.emit(rec)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			for _, cookie := range rec.Result().Cookies() {
				req.AddCookie(cookie)
				if cookie.Name == auth.CSRFCookieName && tc.csrf == "valid" {
					req.Header.Set(auth.CSRFHeaderName, cookie.Value)
				}
			}
			if tc.csrf == "bad" {
				req.Header.Set(auth.CSRFHeaderName, "bad")
			}
			req.Header.Set("X-Uzi-Passive", tc.passive)
			req.Header.Set("Upgrade", tc.upgrade)
			req.AddCookie(&http.Cookie{Name: vaultSlotCookieName, Value: "invalid", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			claims := *session.claims
			if tc.browser != "valid" {
				claims.BrowserSlotID = tc.browser
			}
			out := httptest.NewRecorder()
			h.browserAuthHook(out, req, user, &claims)
			if _, ok := h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(user.ID)); !ok {
				t.Fatal("guard consumed unlock budget")
			}
			if len(out.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("guard changed cookies")
			}
		})
	}
}

func TestPreparedBrowserSessionExpiryAndHTTP(t *testing.T) {
	h := &Handler{cfg: config.Config{JWTSecret: []byte("test-signing-key"), CookieSecure: true, AuthTokenTTL: time.Hour}}
	user := store.User{ID: uuid.New(), TokenVersion: 7}
	session, err := h.freshSession(user)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	session.emit(rec)
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Domain != "" || !cookie.Secure || !cookie.Expires.Equal(session.claims.ExpiresAt.Time) {
			t.Fatal("incorrect session cookie attributes")
		}
	}
	req := h.mintRequest(session.claims)
	if req == nil || !req.SessionExpiresAt.Equal(session.claims.ExpiresAt.Time) {
		t.Fatal("mint expiry differs from signed expiry")
	}
	h.cfg.CookieSecure = false
	if h.mintRequest(session.claims) != nil {
		t.Fatal("HTTP offered slot")
	}
	clearRec := httptest.NewRecorder()
	clearVaultSlot(clearRec)
	cookies := clearRec.Result().Cookies()
	clear := cookies[len(cookies)-1]
	if clear.Name != vaultSlotCookieName || clear.Domain != "" || clear.Path != "/" || !clear.Secure || !clear.HttpOnly || clear.SameSite != http.SameSiteStrictMode || clear.MaxAge != -1 {
		t.Fatal("incorrect slot deletion")
	}
}

func TestBrowserMalformedSlotConsumesBudget(t *testing.T) {
	h := &Handler{vault: testVault(t), cfg: config.Config{CookieSecure: true, JWTSecret: []byte("test-signing-key"), AuthTokenTTL: time.Hour}, vaultUnlockLimiter: mw.NewLimiter(1, time.Hour, nil)}
	user := store.User{ID: uuid.New()}
	session, err := h.freshSession(user)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	session.emit(rec)
	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	for _, cookie := range rec.Result().Cookies() {
		req.AddCookie(cookie)
		if cookie.Name == auth.CSRFCookieName {
			req.Header.Set(auth.CSRFHeaderName, cookie.Value)
		}
	}
	req.Header.Add("Cookie", vaultSlotCookieName+"=bad")
	out := httptest.NewRecorder()
	h.browserAuthHook(out, req, user, session.claims)
	if _, ok := h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(user.ID)); ok {
		t.Fatal("malformed presentation did not consume budget")
	}
	if !slotCleared(out) {
		t.Fatal("malformed presentation kept the slot cookie")
	}
}

// A cookie under any other name, such as the unprefixed one a same-site sibling
// origin can plant with a Domain attribute, is never read, charged or cleared.
func TestBrowserUnprefixedSlotCookieIgnored(t *testing.T) {
	h := &Handler{vault: testVault(t), cfg: config.Config{CookieSecure: true, JWTSecret: []byte("test-signing-key"), AuthTokenTTL: time.Hour}, vaultUnlockLimiter: mw.NewLimiter(1, time.Hour, nil)}
	user := store.User{ID: uuid.New()}
	session, err := h.freshSession(user)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	session.emit(rec)
	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	for _, cookie := range rec.Result().Cookies() {
		req.AddCookie(cookie)
		if cookie.Name == auth.CSRFCookieName {
			req.Header.Set(auth.CSRFHeaderName, cookie.Value)
		}
	}
	planted := "v1." + uuid.NewString() + "." + strings.Repeat("A", 43)
	req.Header.Add("Cookie", "uzi_vault_slot="+planted+"; uzi_vault_slot=bad")
	out := httptest.NewRecorder()
	h.browserAuthHook(out, req, user, session.claims)
	if _, ok := h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(user.ID)); !ok {
		t.Fatal("unprefixed cookie consumed unlock budget")
	}
	if len(out.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("unprefixed cookie changed cookies")
	}
}

func TestBrowserTransientRestoreKeepsSlotCookie(t *testing.T) {
	h := &Handler{vault: testVault(t), cfg: config.Config{CookieSecure: true, JWTSecret: []byte("test-signing-key"), AuthTokenTTL: time.Hour}, vaultUnlockLimiter: mw.NewLimiter(1, time.Hour, nil)}
	user := store.User{ID: uuid.New()}
	session, err := h.freshSession(user)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	session.emit(rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/auth/me", nil).WithContext(ctx)
	for _, cookie := range rec.Result().Cookies() {
		req.AddCookie(cookie)
		if cookie.Name == auth.CSRFCookieName {
			req.Header.Set(auth.CSRFHeaderName, cookie.Value)
		}
	}
	req.Header.Add("Cookie", vaultSlotCookieName+"=v1."+uuid.NewString()+"."+strings.Repeat("A", 43))
	out := httptest.NewRecorder()
	h.browserAuthHook(out, req, user, session.claims)
	if _, ok := h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(user.ID)); ok {
		t.Fatal("restore attempt did not reserve budget")
	}
	if slotCleared(out) {
		t.Fatal("transient restore failure cleared the slot cookie")
	}
}
