package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func browserRequest(t *testing.T, method, token string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, "/api/vault/status", nil)
	rec := httptest.NewRecorder()
	if err := auth.SetAuthCookies(rec, token, auth.CookieOptions{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		r.AddCookie(c)
		if c.Name == auth.CSRFCookieName {
			r.Header.Set(auth.CSRFHeaderName, c.Value)
		}
	}
	return r
}

func TestBrowserHookRefreshChain(t *testing.T) {
	for _, composed := range []bool{false, true} {
		for _, slot := range []string{"", "browser-one"} {
			t.Run(strings.Join([]string{map[bool]string{false: "RequireAuth", true: "RequireUser"}[composed], slot}, "/"), func(t *testing.T) {
				cfg := authTestCfg()
				user := store.User{ID: uuid.New(), IsActive: true, TokenVersion: 3}
				token, incoming, err := auth.PrepareToken(cfg.JWTSecret, user.ID.String(), 3, 10*time.Minute, slot)
				if err != nil {
					t.Fatal(err)
				}
				req := browserRequest(t, http.MethodPost, token)
				rec := httptest.NewRecorder()
				order := []string{}
				hook := func(_ http.ResponseWriter, r *http.Request, got store.User, claims *auth.Claims) {
					order = append(order, "hook")
					if got.ID != user.ID || !reflect.DeepEqual(claims, incoming) {
						t.Fatal("hook lost authenticated input")
					}
					if !auth.ValidateCSRFToken(r) || len(rec.Header().Values("Set-Cookie")) != 0 {
						t.Fatal("hook ran after refresh or against outgoing pair")
					}
					ctxClaims, ok := ClaimsFromContext(r.Context())
					if !ok || !reflect.DeepEqual(ctxClaims, incoming) {
						t.Fatal("hook context lost incoming claims")
					}
					// Hooks cannot mutate the identity/lifetime used by refresh or handler.
					claims.BrowserSlotID = "mutated"
					claims.ExpiresAt.Time = time.Now().Add(-time.Hour)
					ctxClaims.ExpiresAt.Time = time.Now().Add(-time.Hour)
				}
				st := fakeAuthStore{user: user}
				mw := RequireAuth(st, cfg, hook)
				if composed {
					mw = RequireUser(st, cfg, hook)
				}
				name := runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name()
				wantName := ".RequireAuth."
				if composed {
					wantName = ".RequireUser."
				}
				if !strings.Contains(name, wantName) {
					t.Fatalf("route coverage closure identity: %s", name)
				}
				mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					order = append(order, "handler")
					claims, ok := ClaimsFromContext(r.Context())
					if !ok || !reflect.DeepEqual(claims, incoming) {
						t.Fatal("handler did not receive incoming claims")
					}
					if !auth.ValidateCSRFToken(r) {
						t.Fatal("incoming pair replaced before handler")
					}
					w.WriteHeader(http.StatusNoContent)
				})).ServeHTTP(rec, req)
				if rec.Code != http.StatusNoContent || !reflect.DeepEqual(order, []string{"hook", "handler"}) {
					t.Fatalf("status/order: %d %v", rec.Code, order)
				}
				out := browserRequestFromCookies(rec.Result().Cookies())
				if !auth.ValidateCSRFToken(out) {
					t.Fatal("outgoing pair invalid")
				}
				cookie, err := out.Cookie(auth.AuthCookieName)
				if err != nil {
					t.Fatal(err)
				}
				refreshed, err := auth.ParseToken(cfg.JWTSecret, cookie.Value)
				if err != nil {
					t.Fatal(err)
				}
				if refreshed.BrowserSlotID != slot || refreshed.TokenVersion != 3 || refreshed.UserID != user.ID.String() || !refreshed.ExpiresAt.After(incoming.ExpiresAt.Time) {
					t.Fatal("refresh lost identity or failed to extend TTL")
				}
				// An outgoing JWT cannot authorize the incoming CSRF header.
				out.Header.Set(auth.CSRFHeaderName, req.Header.Get(auth.CSRFHeaderName))
				if auth.ValidateCSRFToken(out) {
					t.Fatal("incoming header accepted with outgoing JWT")
				}
			})
		}
	}
}

func browserRequestFromCookies(cookies []*http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	for _, c := range cookies {
		r.AddCookie(c)
		if c.Name == auth.CSRFCookieName {
			r.Header.Set(auth.CSRFHeaderName, c.Value)
		}
	}
	return r
}

func TestBrowserHookRejectedAndPassiveRequests(t *testing.T) {
	for _, tc := range []struct {
		name          string
		active        bool
		version       int32
		csrf, passive bool
		want          int
		hook          bool
	}{
		{"inactive", false, 3, true, false, 401, false},
		{"revoked", true, 4, true, false, 401, false},
		{"missing csrf", true, 3, false, false, 403, false},
		{"passive", true, 3, true, true, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := authTestCfg()
			user := store.User{ID: uuid.New(), IsActive: tc.active, TokenVersion: tc.version}
			token, _, err := auth.PrepareToken(cfg.JWTSecret, user.ID.String(), 3, 10*time.Minute, "slot")
			if err != nil {
				t.Fatal(err)
			}
			r := browserRequest(t, http.MethodPost, token)
			if !tc.csrf {
				r.Header.Del(auth.CSRFHeaderName)
			}
			if tc.passive {
				r.Header.Set(passiveHeader, "1")
			}
			called := false
			rec, reached := run(RequireAuth(fakeAuthStore{user: user}, cfg, func(http.ResponseWriter, *http.Request, store.User, *auth.Claims) { called = true }), r)
			if rec.Code != tc.want || called != tc.hook || reached != tc.hook || len(rec.Result().Cookies()) != 0 {
				t.Fatalf("status=%d hook=%v reached=%v cookies=%d", rec.Code, called, reached, len(rec.Result().Cookies()))
			}
		})
	}
}

func TestRequireUserBearerIgnoresBrowserHook(t *testing.T) {
	token, hash, _, err := clitoken.Generate(clitoken.ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	user := store.User{ID: uuid.New(), IsActive: true}
	st := fakeAuthStore{user: user, row: store.CliToken{UserID: user.ID, TokenHash: hash, Scope: clitoken.ScopeUser}}
	mw := RequireUser(st, authTestCfg(), func(http.ResponseWriter, *http.Request, store.User, *auth.Claims) {
		t.Fatal("bearer invoked browser hook")
	})
	req := bearerRequest(context.Background(), token)
	// Both credentials present still dispatch exclusively to Bearer.
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: "invalid-cookie", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ClaimsFromContext(r.Context()); ok {
			t.Fatal("bearer exposed cookie claims")
		}
		if got, ok := UserFromContext(r.Context()); !ok || got.ID != user.ID {
			t.Fatal("bearer lost user")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)
	if rec.Code != 204 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("bearer response %d", rec.Code)
	}
}

func TestBrowserHookRejectsMismatchedIncomingPair(t *testing.T) {
	cfg := authTestCfg()
	user := store.User{ID: uuid.New(), IsActive: true}
	old, _, err := auth.PrepareToken(cfg.JWTSecret, user.ID.String(), 0, 10*time.Minute, "old")
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := auth.PrepareToken(cfg.JWTSecret, user.ID.String(), 0, 10*time.Minute, "current")
	if err != nil {
		t.Fatal(err)
	}
	staleHeader := browserRequest(t, http.MethodPost, old).Header.Get(auth.CSRFHeaderName)
	req := browserRequest(t, http.MethodPost, current)
	req.Header.Set(auth.CSRFHeaderName, staleHeader)
	rec, reached := run(RequireAuth(fakeAuthStore{user: user}, cfg, func(http.ResponseWriter, *http.Request, store.User, *auth.Claims) {
		t.Fatal("mismatched pair reached hook")
	}), req)
	if rec.Code != http.StatusForbidden || reached || len(rec.Result().Cookies()) != 0 {
		t.Fatal("mismatched pair refreshed or authenticated")
	}
}

func TestBrowserHookSafeRequestRequiresStrictAuthorization(t *testing.T) {
	cfg := authTestCfg()
	user := store.User{ID: uuid.New(), IsActive: true}
	token, _, err := auth.PrepareToken(cfg.JWTSecret, user.ID.String(), 0, time.Hour, "slot")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			req := browserRequest(t, method, token)
			req.Header.Del(auth.CSRFHeaderName)
			called := false
			rec, reached := run(RequireUser(fakeAuthStore{user: user}, cfg, func(_ http.ResponseWriter, r *http.Request, _ store.User, _ *auth.Claims) {
				called = true
				if auth.ValidateCSRFToken(r) {
					t.Fatal("safe method incorrectly authorized slot access")
				}
			}), req)
			if rec.Code != http.StatusOK || !reached || !called {
				t.Fatal("ordinary safe-method session semantics changed")
			}
		})
	}
}

func TestBrowserLegacyClaimsRemainValid(t *testing.T) {
	cfg := authTestCfg()
	user := store.User{ID: uuid.New(), IsActive: true}
	claims := auth.Claims{UserID: user.ID.String(), RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	token, err := auth.SignClaims(cfg.JWTSecret, claims)
	if err != nil {
		t.Fatal(err)
	}
	rec, reached := run(RequireAuth(fakeAuthStore{user: user}, cfg), browserRequest(t, http.MethodGet, token))
	if rec.Code != 200 || !reached {
		t.Fatal("legacy JWT rejected")
	}
}
