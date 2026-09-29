package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
)

// PRD #1907 M3: the /api/v1 mount and GET /api/v1/whoami, driven through the
// PRODUCTION router (h.Routes via v1Routers), never a hand-built one, so the mount
// order (RequireV1Caller, then the per-user limiter) is what is measured.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// v1DecodeWhoami decodes a whoami body strictly: an unknown key (an email, an admin
// flag, any store.User column) fails the decode.
func v1DecodeWhoami(t *testing.T, body string) apitypes.V1WhoamiDTO {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var got apitypes.V1WhoamiDTO
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode whoami %q: %v", body, err)
	}
	return got
}

// TestV1WhoamiLiveDB (D13): whoami reports the caller's own user, the product for a
// uzp_ caller (null for uzc_), and the token's scopes, and nothing else.
func TestV1WhoamiLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)

	owner := cliSeedUser(t, pool, true) // an admin: whoami must still not say so
	cliMustExec(t, pool, `UPDATE users SET display_name = 'Ada Owner' WHERE id = $1`, owner)
	productID := v1SeedProduct(t, h.q, owner)
	var productName string
	if err := pool.QueryRow(t.Context(), `SELECT name FROM products WHERE id = $1`, productID).Scan(&productName); err != nil {
		t.Fatalf("read product name: %v", err)
	}
	readOnly := v1MintProductToken(t, h.q, owner, productID, []string{producttoken.ScopeJobsRead}, nil)

	t.Run("uzp_ caller", func(t *testing.T) {
		got := v1Send(routes, http.MethodGet, "/api/v1/whoami", readOnly.token, nil)
		if got.status != http.StatusOK {
			t.Fatalf("status %d %q, want 200", got.status, got.body)
		}
		w := v1DecodeWhoami(t, got.body)
		if w.User.ID != owner.String() || w.User.DisplayName != "Ada Owner" {
			t.Errorf("user = %+v, want id %s display_name %q", w.User, owner, "Ada Owner")
		}
		if w.Product == nil || w.Product.ID != productID.String() || w.Product.Name != productName {
			t.Errorf("product = %+v, want {%s %s}", w.Product, productID, productName)
		}
		if !slices.Equal(w.Scopes, []string{producttoken.ScopeJobsRead}) {
			t.Errorf("scopes = %v, want exactly the token's [jobs:read]", w.Scopes)
		}
		for _, leak := range []string{"email", "is_admin", "password", "token"} {
			if strings.Contains(got.body, leak) {
				t.Errorf("whoami body %q mentions %q", got.body, leak)
			}
		}
	})

	t.Run("uzc_ caller has a null product and every scope", func(t *testing.T) {
		tok := cliMintToken(t, pool, owner, clitoken.ScopeUser)
		got := v1Send(routes, http.MethodGet, "/api/v1/whoami", tok, nil)
		if got.status != http.StatusOK {
			t.Fatalf("status %d %q, want 200", got.status, got.body)
		}
		// Present-as-null on the wire, as the spec's required+nullable product says.
		if !strings.Contains(got.body, `"product":null`) {
			t.Errorf("body %q lacks \"product\":null", got.body)
		}
		w := v1DecodeWhoami(t, got.body)
		if w.User.ID != owner.String() || w.Product != nil || !slices.Equal(w.Scopes, producttoken.Scopes) {
			t.Errorf("whoami = %+v, want the owner, no product, scopes %v", w, producttoken.Scopes)
		}
	})

	t.Run("a user without a display name gets an empty string", func(t *testing.T) {
		other := cliSeedUser(t, pool, false)
		pt := v1MintProductToken(t, h.q, other, v1SeedProduct(t, h.q, other), producttoken.Scopes, nil)
		got := v1Send(routes, http.MethodGet, "/api/v1/whoami", pt.token, nil)
		if got.status != http.StatusOK || !strings.Contains(got.body, `"display_name":""`) {
			t.Fatalf("got %d %q, want 200 with display_name \"\"", got.status, got.body)
		}
		w := v1DecodeWhoami(t, got.body)
		if w.User.ID != other.String() || !slices.Equal(w.Scopes, producttoken.Scopes) {
			t.Errorf("whoami = %+v, want user %s with both scopes", w, other)
		}
	})
}

// TestV1SubtreeRequiresV1CallerLiveDB (PRD #1907 D2, M2 review): EVERY route in the
// production /api/v1 subtree sits behind RequireV1Caller. Each one answers a
// cookie-only request (a valid session cookie plus a valid CSRF header), an unknown
// uzp_ and a uza_ with RequireV1Caller's own 401. The walk is over the live table, so
// a route PRD #1908 adds is covered with no edit here.
func TestV1SubtreeRequiresV1CallerLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)

	cookieUser := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, cookieUser)
	uza := cliMintToken(t, pool, cliSeedUser(t, pool, true), clitoken.ScopeAdminRO)
	unknown := v1UnknownProductToken(t)

	// Positive control on the cookie: the same cookie+CSRF IS accepted by an internal
	// RequireUser route, so a 401 below is RequireV1Caller refusing it, not a bad cookie.
	cookieReq := func(method, path string) v1Resp {
		req := httptest.NewRequest(method, path, bytes.NewReader(nil))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only request cookie.
		req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return v1Resp{rec.Code, rec.Body.String()}
	}
	if got := cookieReq(http.MethodGet, "/api/runs/"); got.status != http.StatusOK {
		t.Fatalf("control: the session cookie on GET /api/runs/: %d %q, want 200", got.status, got.body)
	}

	cr, ok := routes.(chi.Routes)
	if !ok {
		t.Fatalf("Routes is %T, not a chi.Routes", routes)
	}
	placeholder := uuid.NewString()
	var walked []string
	if err := chi.Walk(cr, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !v1IsV1(pattern) {
			return nil
		}
		walked = append(walked, method+" "+pattern)
		path, err := v1ConcretePath(pattern, placeholder)
		if err != nil {
			t.Errorf("%s %s: %v", method, pattern, err)
			return nil
		}
		if found := cr.Find(chi.NewRouteContext(), method, path); found != pattern {
			t.Errorf("%s %s: concrete path %q routes to %q, not the walked pattern", method, pattern, path, found)
			return nil
		}
		for _, c := range []struct {
			name string
			got  v1Resp
		}{
			{"cookie+CSRF only", cookieReq(method, path)},
			{"unknown uzp_", v1Send(routes, method, path, unknown, nil)},
			{"uza_", v1Send(routes, method, path, uza, nil)},
			{"uzp_ with a cookie", func() v1Resp {
				req := httptest.NewRequest(method, path, nil)
				req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only request cookie.
				req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
				req.Header.Set("Authorization", "Bearer "+unknown)
				rec := httptest.NewRecorder()
				routes.ServeHTTP(rec, req)
				return v1Resp{rec.Code, rec.Body.String()}
			}()},
		} {
			// RequireV1Caller's exact body, so a 401 from any other layer does not pass.
			if c.got.status != http.StatusUnauthorized || c.got.body != "{\"error\":\"invalid token\"}\n" {
				t.Errorf("%s %s with %s: %d %q, want RequireV1Caller's 401", method, pattern, c.name, c.got.status, c.got.body)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	t.Logf("walked %d /api/v1 routes: %v", len(walked), walked)
	if !slices.Contains(walked, "GET /api/v1/whoami") {
		t.Fatalf("GET /api/v1/whoami was not walked (%v): the subtree is not mounted", walked)
	}
}

// TestV1RateLimitPerUserLiveDB (D15): the /api/v1 limiter runs after RequireV1Caller
// and keys on the user, so a second token of the same user shares the budget while
// another user's does not; a refused (401) request spends none.
func TestV1RateLimitPerUserLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	lim := func() *mw.Limiter { return mw.NewLimiter(1_000_000, time.Hour, nil) }
	const budget = 2
	authLimiter := mw.NewLimiter(budget, time.Hour, nil)
	routes := h.Routes(authLimiter, lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim())

	a := cliSeedUser(t, pool, false)
	productA := v1SeedProduct(t, h.q, a)
	tokA1 := v1MintProductToken(t, h.q, a, productA, producttoken.Scopes, nil)
	tokA2 := v1MintProductToken(t, h.q, a, productA, producttoken.Scopes, nil)
	b := cliSeedUser(t, pool, false)
	tokB := v1MintProductToken(t, h.q, b, v1SeedProduct(t, h.q, b), producttoken.Scopes, nil)

	// Refused requests first: they must not consume user A's budget (auth runs first
	// and there is no user to key on).
	for range budget + 1 {
		if got := v1Send(routes, http.MethodGet, "/api/v1/whoami", v1UnknownProductToken(t), nil); got.status != http.StatusUnauthorized {
			t.Fatalf("unknown token: %d %q, want 401", got.status, got.body)
		}
	}
	for i := range budget {
		if got := v1Send(routes, http.MethodGet, "/api/v1/whoami", tokA1.token, nil); got.status != http.StatusOK {
			t.Fatalf("user A request %d: %d %q, want 200", i+1, got.status, got.body)
		}
	}
	// Over budget, through A's SECOND token: the bucket is the user's, not the token's.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tokA2.token)
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("user A over budget via a second token: %d (Retry-After %q) %q, want 429 with Retry-After",
			rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if got := v1Send(routes, http.MethodGet, "/api/v1/whoami", tokB.token, nil); got.status != http.StatusOK {
		t.Fatalf("user B, own budget: %d %q, want 200", got.status, got.body)
	}
}
