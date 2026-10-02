package middleware

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
	cliErr  error // returned for a matching hash instead of the row, when set

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
		if f.cliErr != nil {
			return store.CliToken{}, f.cliErr
		}
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
	return v1DoCtx(t, context.Background(), st, authz, withCookie, probe)
}

// v1DoCtx is v1Do with the request carrying ctx.
func v1DoCtx(t *testing.T, ctx context.Context, st V1CallerStore, authz string, withCookie bool, probe *v1Probe) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil).WithContext(ctx)
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
			name: "uzp_ owner inactive", authz: func(fx v1Fixture) string { return "Bearer " + fx.uzp },
			mutate: func(fx v1Fixture) {
				u := fx.st.users[fx.user.ID]
				u.IsActive = false
				fx.st.users[fx.user.ID] = u
			}, wantProduct: 1,
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

// A store error other than "no such row" means the token was never checked: 503
// auth_unavailable, never the downstream handler, never a last_used touch. The same
// site failing with a wrapped pgx.ErrNoRows is an ordinary refusal (401).
func TestRequireV1CallerLookupErrorUnavailable(t *testing.T) {
	sites := []struct {
		name   string
		tok    func(fx v1Fixture) string
		inject func(fx v1Fixture, err error)
	}{
		{"uzp_ token lookup", func(fx v1Fixture) string { return fx.uzp }, func(fx v1Fixture, err error) { fx.st.productErr = err }},
		{"uzc_ token lookup", func(fx v1Fixture) string { return fx.uzc }, func(fx v1Fixture, err error) { fx.st.cliErr = err }},
		{"uzp_ user lookup", func(fx v1Fixture) string { return fx.uzp }, func(fx v1Fixture, err error) { fx.st.userErr = err }},
		{"uzc_ user lookup", func(fx v1Fixture) string { return fx.uzc }, func(fx v1Fixture, err error) { fx.st.userErr = err }},
	}
	for _, site := range sites {
		t.Run(site.name+" fails", func(t *testing.T) {
			fx := newV1Fixture(t, clitoken.ScopeUser)
			site.inject(fx, errors.New("db down"))
			var probe v1Probe
			rec := v1Do(t, fx.st, "Bearer "+site.tok(fx), false, &probe)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "auth_unavailable") {
				t.Fatalf("body = %q, want reason auth_unavailable", rec.Body.String())
			}
			// No Retry-After: there is no honest estimate of an outage's length.
			if got := rec.Header().Get("Retry-After"); got != "" {
				t.Fatalf("Retry-After = %q, want none", got)
			}
			if strings.Contains(rec.Body.String(), v1Unauthorized) {
				t.Fatalf("body = %q, must not read as an invalid token", rec.Body.String())
			}
			if probe.called {
				t.Fatal("the downstream handler ran on an unavailable lookup")
			}
			if fx.st.productTouches != 0 || fx.st.cliTouches != 0 {
				t.Fatalf("an unavailable lookup touched last_used (product=%d cli=%d)", fx.st.productTouches, fx.st.cliTouches)
			}
		})
		t.Run(site.name+" wrapped ErrNoRows", func(t *testing.T) {
			fx := newV1Fixture(t, clitoken.ScopeUser)
			site.inject(fx, fmt.Errorf("q: %w", pgx.ErrNoRows))
			var probe v1Probe
			rec := v1Do(t, fx.st, "Bearer "+site.tok(fx), false, &probe)
			assertV1Unauthorized(t, rec, &probe)
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

// RequireScope refuses, at construction, a scope outside producttoken.Scopes: a typo
// would otherwise build a route that 403s every caller.
func TestRequireScopePanicsOnUnknownScope(t *testing.T) {
	for _, bad := range []string{"", "jobs:write", "JOBS:READ", "jobs:read "} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("RequireScope(%q) did not panic", bad)
					return
				}
				if msg, _ := r.(string); !strings.Contains(msg, "unknown scope") {
					t.Errorf("RequireScope(%q) panicked with %v, want an unknown-scope message", bad, r)
				}
			}()
			_ = RequireScope(bad)
		}()
	}
	for _, good := range producttoken.Scopes {
		_ = RequireScope(good) // must not panic
	}
}

// A lookup error that is not "no such row" answers 503 and is logged at Warn, with no
// token material; a plain unknown token (ErrNoRows) logs nothing and stays 401. The Warn
// is decided by the REQUEST context, not the error chain: a store error wrapping
// context.Canceled or DeadlineExceeded on a live request (a DB dial or pool timeout) is
// logged, while any store error on an already-cancelled request is still 503 but silent.
func TestRequireV1CallerLogsLookupErrors(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dbDown := errors.New("db down: connection refused")
	canceled := fmt.Errorf("query: %w", context.Canceled)
	deadline := fmt.Errorf("query: %w", context.DeadlineExceeded)
	cases := []struct {
		name       string
		tok        func(fx v1Fixture) string
		mutate     func(fx v1Fixture)
		cancelReq  bool   // send the request with an already-cancelled context
		want503    bool   // else 401
		wantLog    string // "" = nothing logged
		wantErrLog string // the error text the log must carry
	}{
		{name: "unknown uzp_", tok: func(v1Fixture) string { return "uz" + "p_" + strings.Repeat("a", 43) }},
		{name: "unknown uzc_", tok: func(v1Fixture) string { return "uz" + "c_" + strings.Repeat("a", 43) }},
		{name: "product lookup error", tok: func(fx v1Fixture) string { return fx.uzp },
			mutate: func(fx v1Fixture) { fx.st.productErr = dbDown }, want503: true,
			wantLog: "product token lookup failed", wantErrLog: "db down"},
		{name: "product user lookup error", tok: func(fx v1Fixture) string { return fx.uzp },
			mutate: func(fx v1Fixture) { fx.st.userErr = dbDown }, want503: true,
			wantLog: "product token user lookup failed", wantErrLog: "db down"},
		{name: "cli lookup error", tok: func(fx v1Fixture) string { return fx.uzc },
			mutate: func(fx v1Fixture) { fx.st.cliErr = dbDown }, want503: true,
			wantLog: "cli token lookup failed", wantErrLog: "db down"},
		{name: "cli user lookup error", tok: func(fx v1Fixture) string { return fx.uzc },
			mutate: func(fx v1Fixture) { fx.st.userErr = dbDown }, want503: true,
			wantLog: "cli token user lookup failed", wantErrLog: "db down"},
		{name: "product user missing (ErrNoRows)", tok: func(fx v1Fixture) string { return fx.uzp },
			mutate: func(fx v1Fixture) { delete(fx.st.users, fx.user.ID) }},
		// Store errors that wrap a context error on a LIVE request are real faults (a DB
		// dial or pool timeout): logged, 503.
		{name: "product lookup wraps Canceled", tok: func(fx v1Fixture) string { return fx.uzp },
			mutate: func(fx v1Fixture) { fx.st.productErr = canceled }, want503: true,
			wantLog: "product token lookup failed", wantErrLog: "context canceled"},
		{name: "cli lookup wraps DeadlineExceeded", tok: func(fx v1Fixture) string { return fx.uzc },
			mutate: func(fx v1Fixture) { fx.st.cliErr = deadline }, want503: true,
			wantLog: "cli token lookup failed", wantErrLog: "deadline exceeded"},
		{name: "product user lookup wraps Canceled", tok: func(fx v1Fixture) string { return fx.uzp },
			mutate: func(fx v1Fixture) { fx.st.userErr = canceled }, want503: true,
			wantLog: "product token user lookup failed", wantErrLog: "context canceled"},
		{name: "cli user lookup wraps DeadlineExceeded", tok: func(fx v1Fixture) string { return fx.uzc },
			mutate: func(fx v1Fixture) { fx.st.userErr = deadline }, want503: true,
			wantLog: "cli token user lookup failed", wantErrLog: "deadline exceeded"},
		// The client already went away: still unavailable, but not an operator signal.
		{name: "cancelled request, product lookup error", tok: func(fx v1Fixture) string { return fx.uzp },
			mutate: func(fx v1Fixture) { fx.st.productErr = dbDown }, cancelReq: true, want503: true},
		{name: "cancelled request, cli user lookup error", tok: func(fx v1Fixture) string { return fx.uzc },
			mutate: func(fx v1Fixture) { fx.st.userErr = canceled }, cancelReq: true, want503: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			fx := newV1Fixture(t, clitoken.ScopeUser)
			if tc.mutate != nil {
				tc.mutate(fx)
			}
			tok := tc.tok(fx)
			ctx := context.Background()
			if tc.cancelReq {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var probe v1Probe
			rec := v1DoCtx(t, ctx, fx.st, "Bearer "+tok, false, &probe)
			if tc.want503 {
				if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "auth_unavailable") {
					t.Fatalf("status = %d body %q, want 503 auth_unavailable", rec.Code, rec.Body.String())
				}
				if probe.called {
					t.Fatal("the downstream handler ran on an unavailable lookup")
				}
			} else {
				assertV1Unauthorized(t, rec, &probe)
			}
			logged := buf.String()
			if tc.wantLog == "" {
				if logged != "" {
					t.Fatalf("logged %q, want nothing", logged)
				}
				return
			}
			if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, tc.wantLog) ||
				!strings.Contains(logged, "answering 503") || !strings.Contains(logged, tc.wantErrLog) {
				t.Fatalf("log %q lacks a WARN %q with the error %q", logged, tc.wantLog, tc.wantErrLog)
			}
			// No token material: neither the token, its body, nor its sha256 (hex).
			body := tok[4:]
			for _, secret := range []string{tok, body[:12], hex.EncodeToString(clitoken.Hash(tok))[:16]} {
				if strings.Contains(logged, secret) {
					t.Fatalf("log %q contains token material %q", logged, secret)
				}
			}
		})
	}
}
