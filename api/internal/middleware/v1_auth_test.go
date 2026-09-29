package middleware

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeV1Store is a V1CallerStore over one product token and one CLI token. It counts
// every lookup per table so the tests can prove the dispatch never falls back.
type fakeV1Store struct {
	productHash []byte
	productRow  store.GetProductTokenForAuthRow
	productErr  error // returned for a matching hash instead of the row, when set

	cliHash []byte
	cliRow  store.CliToken

	users   map[uuid.UUID]store.User
	userErr error

	touchErr error

	productLookups, cliLookups, userLookups int
	productTouches, cliTouches              int
}

func (f *fakeV1Store) GetProductTokenForAuth(_ context.Context, h []byte) (store.GetProductTokenForAuthRow, error) {
	f.productLookups++
	if f.productHash != nil && bytes.Equal(h, f.productHash) {
		if f.productErr != nil {
			return store.GetProductTokenForAuthRow{}, f.productErr
		}
		return f.productRow, nil
	}
	return store.GetProductTokenForAuthRow{}, pgx.ErrNoRows
}

func (f *fakeV1Store) GetCLITokenByHash(_ context.Context, h []byte) (store.CliToken, error) {
	f.cliLookups++
	if f.cliHash != nil && bytes.Equal(h, f.cliHash) {
		return f.cliRow, nil
	}
	return store.CliToken{}, pgx.ErrNoRows
}

func (f *fakeV1Store) GetUserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	f.userLookups++
	if f.userErr != nil {
		return store.User{}, f.userErr
	}
	u, ok := f.users[id]
	if !ok {
		return store.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeV1Store) TouchProductToken(context.Context, store.TouchProductTokenParams) error {
	f.productTouches++
	return f.touchErr
}

func (f *fakeV1Store) TouchCLIToken(context.Context, store.TouchCLITokenParams) error {
	f.cliTouches++
	return f.touchErr
}

// v1Fixture is an ADMIN, active user holding one uzp_ token (jobs:read only) and one
// uzc_ token whose row scope is set by cliScope.
type v1Fixture struct {
	st       *fakeV1Store
	user     store.User
	uzp, uzc string
}

func newV1Fixture(t *testing.T, cliScope string) v1Fixture {
	t.Helper()
	user := store.User{ID: uuid.New(), Email: "v1@example.test", IsAdmin: true, IsActive: true}

	uzp, uzpHash, _, err := producttoken.Generate()
	if err != nil {
		t.Fatalf("producttoken.Generate: %v", err)
	}
	// The uzc_ value is minted with the user-class prefix whatever the row scope, so the
	// admin_ro case is "a cli_tokens row with scope admin_ro presented under a
	// uzc_-shaped value" (the prefix is only a label).
	uzc, _, _, err := clitoken.Generate(clitoken.ScopeUser)
	if err != nil {
		t.Fatalf("clitoken.Generate: %v", err)
	}
	uzcHash := clitoken.Hash(uzc)

	st := &fakeV1Store{
		productHash: uzpHash,
		productRow: store.GetProductTokenForAuthRow{
			ID: uuid.New(), UserID: user.ID, ProductID: uuid.New(),
			Scopes: []string{producttoken.ScopeJobsRead}, ProductName: "Acme",
		},
		cliHash: uzcHash,
		cliRow:  store.CliToken{ID: uuid.New(), UserID: user.ID, TokenHash: uzcHash, Scope: cliScope},
		users:   map[uuid.UUID]store.User{user.ID: user},
	}
	return v1Fixture{st: st, user: user, uzp: uzp, uzc: uzc}
}

// v1Probe records what RequireV1Caller put in the context.
type v1Probe struct {
	called    bool
	principal V1Principal
	hasP      bool
	user      store.User
	hasUser   bool
}

func (p *v1Probe) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.called = true
		p.principal, p.hasP = V1PrincipalFromContext(r.Context())
		p.user, p.hasUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
}

// v1Do sends one request through RequireV1Caller. authz "" sends no Authorization
// header; withCookie adds a session cookie and a CSRF header, which must never matter.
func v1Do(t *testing.T, st V1CallerStore, authz string, withCookie bool, probe *v1Probe) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if withCookie {
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: "a-session-jwt"}) //nolint:gosec // G124: test-only request cookie.
		req.Header.Set(auth.CSRFHeaderName, "csrf")
	}
	rec := httptest.NewRecorder()
	RequireV1Caller(st, config.Config{})(probe.handler()).ServeHTTP(rec, req)
	return rec
}

func assertV1Unauthorized(t *testing.T, rec *httptest.ResponseRecorder, probe *v1Probe) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), v1Unauthorized) {
		t.Fatalf("body = %q, want the generic %q", rec.Body.String(), v1Unauthorized)
	}
	if probe.called {
		t.Fatal("the downstream handler ran on a refused request")
	}
}

func TestRequireV1CallerProductTokenPath(t *testing.T) {
	for _, withCookie := range []bool{false, true} {
		t.Run(map[bool]string{false: "bearer only", true: "cookie also present"}[withCookie], func(t *testing.T) {
			fx := newV1Fixture(t, clitoken.ScopeUser)
			var probe v1Probe
			rec := v1Do(t, fx.st, "Bearer "+fx.uzp, withCookie, &probe)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (body %q)", rec.Code, rec.Body.String())
			}
			if fx.st.productLookups != 1 || fx.st.cliLookups != 0 {
				t.Fatalf("lookups product=%d cli=%d, want 1/0: a uzp_ must resolve against product_tokens only",
					fx.st.productLookups, fx.st.cliLookups)
			}
			if !probe.hasP || !probe.hasUser {
				t.Fatalf("principal in ctx=%v, user in ctx=%v; want both", probe.hasP, probe.hasUser)
			}
			p := probe.principal
			if p.Kind != V1CallerProductToken || p.TokenID != fx.st.productRow.ID ||
				!p.ProductID.Valid || p.ProductID.UUID != fx.st.productRow.ProductID || p.ProductName != "Acme" {
				t.Fatalf("principal = %+v, want the product token's identity", p)
			}
			if !slices.Equal(p.Scopes, []string{producttoken.ScopeJobsRead}) {
				t.Fatalf("scopes = %v, want exactly the token's [jobs:read]", p.Scopes)
			}
			if p.User.ID != fx.user.ID || probe.user.ID != fx.user.ID {
				t.Fatalf("user = %s / ctx user %s, want %s", p.User.ID, probe.user.ID, fx.user.ID)
			}
			if p.User.IsAdmin || probe.user.IsAdmin {
				t.Fatal("IsAdmin survived RequireV1Caller for an admin's uzp_ token (D3)")
			}
			if fx.st.productTouches != 1 || fx.st.cliTouches != 0 {
				t.Fatalf("touches product=%d cli=%d, want 1/0", fx.st.productTouches, fx.st.cliTouches)
			}
		})
	}
}

func TestRequireV1CallerCLITokenScopeUser(t *testing.T) {
	fx := newV1Fixture(t, clitoken.ScopeUser)
	var probe v1Probe
	rec := v1Do(t, fx.st, "Bearer "+fx.uzc, false, &probe)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}
	if fx.st.cliLookups != 1 || fx.st.productLookups != 0 {
		t.Fatalf("lookups cli=%d product=%d, want 1/0", fx.st.cliLookups, fx.st.productLookups)
	}
	p := probe.principal
	if p.Kind != V1CallerCLIToken || p.TokenID != fx.st.cliRow.ID || p.ProductID.Valid || p.ProductName != "" {
		t.Fatalf("principal = %+v, want a CLI caller with no product", p)
	}
	if !slices.Equal(p.Scopes, producttoken.Scopes) {
		t.Fatalf("scopes = %v, want both %v (D5)", p.Scopes, producttoken.Scopes)
	}
	if p.User.IsAdmin || probe.user.IsAdmin {
		t.Fatal("IsAdmin survived RequireV1Caller for an admin's uzc_ token (D3)")
	}
	if fx.st.cliTouches != 1 {
		t.Fatalf("cli touches = %d, want 1", fx.st.cliTouches)
	}
}

func TestRequireV1CallerRefusals(t *testing.T) {
	uza, _, _, err := clitoken.Generate(clitoken.ScopeAdminRO)
	if err != nil {
		t.Fatalf("clitoken.Generate: %v", err)
	}
	unknownUzp := "uz" + "p_" + strings.Repeat("a", 43)

	cases := []struct {
		name       string
		cliScope   string
		authz      func(fx v1Fixture) string
		withCookie bool
		mutate     func(fx v1Fixture)
		// wantProduct/wantCLI are the exact lookup counts: the dispatch never falls back.
		wantProduct, wantCLI int
	}{
		{name: "no header", authz: func(v1Fixture) string { return "" }},
		{name: "cookie only", authz: func(v1Fixture) string { return "" }, withCookie: true},
		{name: "basic scheme", authz: func(fx v1Fixture) string { return "Basic " + fx.uzp }},
		{name: "empty bearer", authz: func(v1Fixture) string { return "Bearer " }},
		{name: "uza_ class", authz: func(v1Fixture) string { return "Bearer " + uza }},
		{name: "worker class", authz: func(v1Fixture) string { return "Bearer uz" + "w_" + strings.Repeat("b", 43) }},
		{name: "unknown uzp_", authz: func(v1Fixture) string { return "Bearer " + unknownUzp }, wantProduct: 1},
		{name: "unknown uzp_ with a session cookie", authz: func(v1Fixture) string { return "Bearer " + unknownUzp }, withCookie: true, wantProduct: 1},
		{name: "unknown uzc_", authz: func(v1Fixture) string { return "Bearer uz" + "c_" + strings.Repeat("c", 43) }, wantCLI: 1},
		{
			name: "uzc_-shaped value on an admin_ro row", cliScope: clitoken.ScopeAdminRO,
			authz: func(fx v1Fixture) string { return "Bearer " + fx.uzc }, wantCLI: 1,
		},
		{
			name: "uzp_ lookup error", authz: func(fx v1Fixture) string { return "Bearer " + fx.uzp },
			mutate: func(fx v1Fixture) { fx.st.productErr = errors.New("db down") }, wantProduct: 1,
		},
		{
			name: "uzp_ owner inactive", authz: func(fx v1Fixture) string { return "Bearer " + fx.uzp },
			mutate: func(fx v1Fixture) {
				u := fx.st.users[fx.user.ID]
				u.IsActive = false
				fx.st.users[fx.user.ID] = u
			}, wantProduct: 1,
		},
		{
			name: "uzp_ user lookup error", authz: func(fx v1Fixture) string { return "Bearer " + fx.uzp },
			mutate: func(fx v1Fixture) { fx.st.userErr = errors.New("db down") }, wantProduct: 1,
		},
		{
			name: "uzc_ owner inactive", authz: func(fx v1Fixture) string { return "Bearer " + fx.uzc },
			mutate: func(fx v1Fixture) {
				u := fx.st.users[fx.user.ID]
				u.IsActive = false
				fx.st.users[fx.user.ID] = u
			}, wantCLI: 1,
		},
		{
			name: "uzc_ row hash mismatch", authz: func(fx v1Fixture) string { return "Bearer " + fx.uzc },
			mutate: func(fx v1Fixture) { fx.st.cliRow.TokenHash = clitoken.Hash("other") }, wantCLI: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := tc.cliScope
			if scope == "" {
				scope = clitoken.ScopeUser
			}
			fx := newV1Fixture(t, scope)
			if tc.mutate != nil {
				tc.mutate(fx)
			}
			var probe v1Probe
			rec := v1Do(t, fx.st, tc.authz(fx), tc.withCookie, &probe)
			assertV1Unauthorized(t, rec, &probe)
			if fx.st.productLookups != tc.wantProduct || fx.st.cliLookups != tc.wantCLI {
				t.Fatalf("lookups product=%d cli=%d, want %d/%d (deterministic dispatch, no fallback)",
					fx.st.productLookups, fx.st.cliLookups, tc.wantProduct, tc.wantCLI)
			}
			if fx.st.productTouches != 0 || fx.st.cliTouches != 0 {
				t.Fatalf("a refused request touched last_used (product=%d cli=%d)", fx.st.productTouches, fx.st.cliTouches)
			}
		})
	}
}

// A failing last-used touch is a forensic miss, not an auth decision.
func TestRequireV1CallerTouchFailureDoesNotFailRequest(t *testing.T) {
	fx := newV1Fixture(t, clitoken.ScopeUser)
	fx.st.touchErr = errors.New("db hiccup")
	for _, tok := range []string{fx.uzp, fx.uzc} {
		var probe v1Probe
		if rec := v1Do(t, fx.st, "Bearer "+tok, false, &probe); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 despite the touch error", rec.Code)
		}
	}
}

func TestRequireScope(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	do := func(ctx context.Context, scope string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		RequireScope(scope)(ok).ServeHTTP(rec, req)
		return rec.Code
	}
	readOnly := context.WithValue(context.Background(), v1PrincipalKey,
		V1Principal{Kind: V1CallerProductToken, Scopes: []string{producttoken.ScopeJobsRead}})

	if got := do(context.Background(), producttoken.ScopeJobsRead); got != http.StatusUnauthorized {
		t.Errorf("no principal: status = %d, want 401", got)
	}
	// A context user alone (the internal API's auth) is not a v1 principal.
	if got := do(ContextWithUser(context.Background(), store.User{ID: uuid.New()}), producttoken.ScopeJobsRead); got != http.StatusUnauthorized {
		t.Errorf("user without principal: status = %d, want 401", got)
	}
	if got := do(readOnly, producttoken.ScopeJobsRun); got != http.StatusForbidden {
		t.Errorf("missing scope: status = %d, want 403", got)
	}
	if got := do(readOnly, producttoken.ScopeJobsRead); got != http.StatusNoContent {
		t.Errorf("held scope: status = %d, want 204", got)
	}
}

// End to end through both middlewares: a uzc_ caller holds both scopes, a jobs:read
// product token is refused jobs:run with 403.
func TestRequireV1CallerThenRequireScope(t *testing.T) {
	fx := newV1Fixture(t, clitoken.ScopeUser)
	chain := func(scope string) http.Handler {
		return RequireV1Caller(fx.st, config.Config{})(RequireScope(scope)(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	}
	cases := []struct {
		tok, scope string
		want       int
	}{
		{fx.uzp, producttoken.ScopeJobsRead, http.StatusNoContent},
		{fx.uzp, producttoken.ScopeJobsRun, http.StatusForbidden},
		{fx.uzc, producttoken.ScopeJobsRead, http.StatusNoContent},
		{fx.uzc, producttoken.ScopeJobsRun, http.StatusNoContent},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/x", nil)
		req.Header.Set("Authorization", "Bearer "+c.tok)
		rec := httptest.NewRecorder()
		chain(c.scope).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s… on %s: status = %d, want %d", c.tok[:4], c.scope, rec.Code, c.want)
		}
	}
}
