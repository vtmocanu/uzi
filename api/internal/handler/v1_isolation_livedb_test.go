package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/hub"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1907 M2: RequireV1Caller and the structural isolation of product tokens.
//
// The load-bearing test here is TestV1IsolationLiveDB: a REAL uzp_ token, minted
// through the store, is sent to every route of BOTH production routers outside
// /api/v1 and must get exactly what an unknown Bearer of the same shape gets. That is
// the D1 claim ("RequireUser never reads product_tokens, so a uzp_ token is unknown to
// every internal route") measured on the live route tables rather than asserted per
// mount. It iterates chi.Walk, so a route added tomorrow is covered with no edit here.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// v1LiveDB opens the live pool and builds a Handler through handler.New, the same
// constructor cmd/server uses, with hosting ON (so the /api/controller group is
// mounted, as it is on a hosted deployment) and the hosted service wired as main does.
func v1LiveDB(t *testing.T) (*Handler, *pgxpool.Pool) {
	t.Helper()
	return v1LiveDBMax(t, 0)
}

// v1LiveDBMax is v1LiveDB with the pool capped at maxConns connections (0 keeps pgx's default), for
// the tests that measure what holds a connection.
func v1LiveDBMax(t *testing.T, maxConns int32) (*Handler, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	if maxConns > 0 {
		pcfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	// A controller credential exists but is neither caller's: /api/controller answers
	// both with its own 401.
	_, controllerHash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("jointoken.Generate: %v", err)
	}
	cfg := config.Config{
		JWTSecret:             cliTestSecret,
		AuthTokenTTL:          time.Hour,
		WorkerHostingEnabled:  true,
		ControllerTokenSHA256: controllerHash,
		WorkerHeartbeatStale:  time.Minute,
	}
	wsvc := workersvc.New(q, box, workersvc.Params{})
	// As cmd/server wires it: a job create needs the transaction beginner (its cap check and
	// inserts are one atomic unit), so the /api/v1/jobs tests run through the same service.
	wsvc.SetTxBeginner(pool)
	h := New(pool, q, cfg, box, nil, wsvc, nil, hub.New(), settings.New(q, time.Minute))
	h.SetHostedSvc(hostedsvc.New(q, box, time.Now, cfg.WorkerHeartbeatStale))
	return h, pool
}

// v1Routers builds both production routers the way cmd/server does: Routes with nine
// limiters, WorkerRoutes sharing the proposal limiter instance. The limiters are
// generous (per-IP/per-user budgets would otherwise make the second request of a pair
// differ from the first for a reason unrelated to the token).
func v1Routers(h *Handler) (routes, workerRoutes http.Handler) {
	lim := func() *mw.Limiter { return mw.NewLimiter(1_000_000, time.Hour, nil) }
	proposal := lim()
	routes = h.Routes(lim(), lim(), lim(), lim(), proposal, lim(), lim(), lim(), lim(), lim())
	return routes, h.WorkerRoutes(proposal)
}

// v1Product is a product plus one product token minted through the store queries.
type v1Product struct {
	productID uuid.UUID
	tokenID   uuid.UUID
	token     string
}

func v1SeedProduct(t *testing.T, q *store.Queries, ownerID uuid.UUID) uuid.UUID {
	t.Helper()
	p, err := q.CreateProduct(context.Background(), store.CreateProductParams{
		Name:        "v1-test-" + uuid.NewString(),
		Description: "PRD #1907 M2 fixture",
		CreatedBy:   pgtype.UUID{Bytes: ownerID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	return p.ID
}

func v1MintProductToken(t *testing.T, q *store.Queries, userID, productID uuid.UUID, scopes []string, expiresAt *time.Time) v1Product {
	t.Helper()
	token, hash, prefix, err := producttoken.Generate()
	if err != nil {
		t.Fatalf("producttoken.Generate: %v", err)
	}
	var exp pgtype.Timestamptz
	if expiresAt != nil {
		exp = pgtype.Timestamptz{Time: *expiresAt, Valid: true}
	}
	row, err := q.CreateProductToken(context.Background(), store.CreateProductTokenParams{
		UserID: userID, ProductID: productID, Name: "v1 test",
		TokenHash: hash, TokenPrefix: prefix, Scopes: scopes, ExpiresAt: exp,
	})
	if err != nil {
		t.Fatalf("CreateProductToken: %v", err)
	}
	return v1Product{productID: productID, tokenID: row.ID, token: token}
}

// v1UnknownProductToken is a uzp_ value of exactly the minted shape (prefix plus a
// 256-bit base64url body) that no row carries.
func v1UnknownProductToken(t *testing.T) string {
	t.Helper()
	tok, _, _, err := producttoken.Generate()
	if err != nil {
		t.Fatalf("producttoken.Generate: %v", err)
	}
	return tok
}

// v1ProbeRouter mounts RequireV1Caller exactly as M3 will (on the /api/v1 subtree),
// with one probe route per scope. The probe reports the context the middleware built.
func v1ProbeRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(mw.RequireV1Caller(h.q, h.cfg))
		probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, okU := mw.UserFromContext(r.Context())
			p, okP := mw.V1PrincipalFromContext(r.Context())
			product := ""
			if p.ProductID.Valid {
				product = p.ProductID.UUID.String()
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "user=%s ctxAdmin=%t principalAdmin=%t okU=%t okP=%t kind=%s token=%s product=%s scopes=%s",
				u.ID, u.IsAdmin, p.User.IsAdmin, okU, okP, p.Kind, p.TokenID, product, strings.Join(p.Scopes, ","))
		})
		r.With(mw.RequireScope(producttoken.ScopeJobsRead)).Get("/probe/read", probe)
		r.With(mw.RequireScope(producttoken.ScopeJobsRun)).Get("/probe/run", probe)
	})
	return r
}

type v1Resp struct {
	status int
	body   string
}

// v1Send drives router with one request: an optional Bearer, optional extra headers,
// and a JSON content type with an empty body (authentication runs before any body is
// read; a public route answers both callers the same 4xx for it). A per-request
// timeout bounds any handler that would wait on the request context.
func v1Send(router http.Handler, method, path, bearer string, hdr http.Header) v1Resp {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(method, path, nil).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return v1Resp{status: rec.Code, body: rec.Body.String()}
}

// v1PathParam matches one chi path parameter, capturing the optional regexp after
// the first ':' ({id} or {slot:[a-z]+}).
var v1PathParam = regexp.MustCompile(`\{[^}:]*(?::([^}]*))?\}`)

// v1RegexpCandidates are tried, in order, for a {param:regexp} segment the uuid
// placeholder does not satisfy. Extend it when a route adds a constraint none meets.
// No production route carries a regexp param today (TestV1WalkHelpers exercises the
// generation on synthetic patterns); the walk's chi Find check is the backstop that
// reddens if a future one is filled with a value chi cannot route.
var v1RegexpCandidates = []string{"a", "x", "abc", "1", "0", "123"}

// v1ConcretePath turns a chi pattern into a request path: a plain {param} becomes
// placeholder, a {param:regexp} becomes the first of placeholder and
// v1RegexpCandidates that the WHOLE regexp matches (chi anchors it to the segment), and
// a catch-all "*" becomes "x". A regexp no candidate satisfies is an error, never a
// silently unroutable path: a request chi cannot route 404s before any auth runs and
// would make every comparison over it vacuous.
func v1ConcretePath(pattern, placeholder string) (string, error) {
	var firstErr error
	p := v1PathParam.ReplaceAllStringFunc(pattern, func(seg string) string {
		m := v1PathParam.FindStringSubmatch(seg)
		if m[1] == "" {
			return placeholder
		}
		re, err := regexp.Compile("^(?:" + m[1] + ")$")
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("pattern %q: param regexp %q: %w", pattern, m[1], err)
			}
			return seg
		}
		for _, c := range append([]string{placeholder}, v1RegexpCandidates...) {
			if re.MatchString(c) {
				return c
			}
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("pattern %q: no placeholder satisfies param regexp %q; extend v1RegexpCandidates", pattern, m[1])
		}
		return seg
	})
	if firstErr != nil {
		return "", firstErr
	}
	return strings.ReplaceAll(p, "*", "x"), nil
}

// v1RoutingMiss reports whether a response is chi's own "no route" answer rather than
// anything a route produced: the default NotFound (http.NotFound's plain-text body)
// or the default MethodNotAllowed (405, empty body). Every uzi handler and middleware
// answers with a JSON body, so neither can be mistaken for a handler's own 404.
func v1RoutingMiss(r v1Resp) bool {
	return (r.status == http.StatusNotFound && r.body == "404 page not found\n") ||
		(r.status == http.StatusMethodNotAllowed && r.body == "")
}

// v1IsV1 reports whether a pattern is inside the /api/v1 subtree, the only exclusion.
func v1IsV1(pattern string) bool {
	return pattern == "/api/v1" || strings.HasPrefix(pattern, "/api/v1/")
}

// v1SideEffects is everything a uzp_ request could plausibly change for its owner. The
// uzp_ token's last_used_at/last_used_ip are the sharpest signal: only RequireV1Caller
// touches them, so any internal route that resolved the token would leave a stamp.
type v1SideEffects struct {
	TokenLastUsedNull, TokenLastIPNull, TokenRevoked bool
	UserIsAdmin, UserIsActive                        bool
	UserTokenVersion                                 int32
	UserLastLoginNull                                bool
	Runs, CLITokens, ProductTokens, Products         int64
	ProductEnabled, ProductDeleted                   bool
}

func v1Snapshot(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, pt v1Product) v1SideEffects {
	t.Helper()
	ctx := context.Background()
	var s v1SideEffects
	if err := pool.QueryRow(ctx,
		`SELECT last_used_at IS NULL, last_used_ip IS NULL, revoked FROM product_tokens WHERE id = $1`, pt.tokenID).
		Scan(&s.TokenLastUsedNull, &s.TokenLastIPNull, &s.TokenRevoked); err != nil {
		t.Fatalf("snapshot token: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT is_admin, is_active, token_version, last_login IS NULL FROM users WHERE id = $1`, userID).
		Scan(&s.UserIsAdmin, &s.UserIsActive, &s.UserTokenVersion, &s.UserLastLoginNull); err != nil {
		t.Fatalf("snapshot user: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM runs WHERE user_id = $1),
		(SELECT count(*) FROM cli_tokens WHERE user_id = $1),
		(SELECT count(*) FROM product_tokens WHERE user_id = $1),
		(SELECT count(*) FROM products WHERE created_by = $1)`, userID).
		Scan(&s.Runs, &s.CLITokens, &s.ProductTokens, &s.Products); err != nil {
		t.Fatalf("snapshot counts: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT enabled, deleted_at IS NOT NULL FROM products WHERE id = $1`, pt.productID).
		Scan(&s.ProductEnabled, &s.ProductDeleted); err != nil {
		t.Fatalf("snapshot product: %v", err)
	}
	return s
}

// TestV1IsolationLiveDB is PRD #1907 M2's isolation test. See the file comment.
//
// Normalisation: none. Status and body are compared byte for byte; no route is
// exempted and no body is rewritten. (Response headers are not compared: chi's
// RequestID middleware makes none of them part of the body, and the PRD's property is
// status plus body.) The only exclusion is the /api/v1 subtree, which M3 mounts and
// which is RequireV1Caller's by design.
func TestV1IsolationLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, workerRoutes := v1Routers(h)

	// An ADMIN owner with a live, never-expiring, all-scopes token: the most powerful
	// uzp_ there is, so any route that resolved it would have the most to show.
	owner := cliSeedUser(t, pool, true)
	pt := v1MintProductToken(t, h.q, owner, v1SeedProduct(t, h.q, owner), producttoken.Scopes, nil)
	unknown := v1UnknownProductToken(t)
	if len(unknown) != len(pt.token) || !producttoken.HasPrefix(unknown) {
		t.Fatalf("unknown token %d chars, real %d: the control must have the real token's shape", len(unknown), len(pt.token))
	}

	before := v1Snapshot(t, pool, owner, pt)
	if !before.TokenLastUsedNull {
		t.Fatal("fixture token already has last_used_at; the side-effect check would be vacuous")
	}

	// A path placeholder for every {param}: a fresh uuid no row carries.
	placeholder := uuid.NewString()

	type walked struct{ router, method, pattern string }
	var seen []walked
	statuses := map[int]int{}
	// Routes whose answer to the unknown uzp_ is RequireUser's own 401: the positive
	// proof that walked requests reach an auth layer (see the floor below).
	requireUserRefusals := 0
	for _, rt := range []struct {
		name   string
		router http.Handler
	}{{"Routes", routes}, {"WorkerRoutes", workerRoutes}} {
		cr, ok := rt.router.(chi.Routes)
		if !ok {
			t.Fatalf("%s is not a chi.Routes", rt.name)
		}
		err := chi.Walk(cr, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			if v1IsV1(pattern) {
				return nil
			}
			seen = append(seen, walked{rt.name, method, pattern})
			path, err := v1ConcretePath(pattern, placeholder)
			if err != nil {
				t.Errorf("%s %s %s: %v", rt.name, method, pattern, err)
				return nil
			}
			// The request must route to the walked route itself. Without this a path chi
			// cannot route (a regexp param the placeholder fails, a mangled prefix) gets
			// chi's 404 for BOTH tokens, and the equality below passes having tested no
			// auth layer at all.
			if found := cr.Find(chi.NewRouteContext(), method, path); found != pattern {
				t.Errorf("%s %s %s: concrete path %q routes to %q, not the walked pattern", rt.name, method, pattern, path, found)
				return nil
			}
			// The unknown token first, then the real one: if anything differed, the real
			// token's response is the one that carries the leak.
			got0 := v1Send(rt.router, method, path, unknown, nil)
			got1 := v1Send(rt.router, method, path, pt.token, nil)
			statuses[got1.status]++
			if v1RoutingMiss(got0) || v1RoutingMiss(got1) {
				t.Errorf("%s %s %s (%s): a chi routing miss (%d %q), not a response from the route",
					rt.name, method, pattern, path, got1.status, got1.body)
			}
			if got0.status == http.StatusUnauthorized && got0.body == "{\"error\":\"invalid CLI token\"}\n" {
				requireUserRefusals++
			}
			if got0 != got1 {
				t.Errorf("%s %s %s (%s): real uzp_ got %d %q, unknown uzp_ got %d %q",
					rt.name, method, pattern, path, got1.status, got1.body, got0.status, got0.body)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", rt.name, err)
		}
	}

	// /api/ws explicitly, as a real WebSocket upgrade for a run id, not only the bare
	// walked GET: the upgrade must be refused identically before ServeWS runs.
	wsHdr := http.Header{
		"Connection":            {"Upgrade"},
		"Upgrade":               {"websocket"},
		"Sec-Websocket-Version": {"13"},
		"Sec-Websocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
		"Origin":                {"http://example.test"},
	}
	wsPath := "/api/ws?run_id=" + placeholder
	ws0 := v1Send(routes, http.MethodGet, wsPath, unknown, wsHdr)
	ws1 := v1Send(routes, http.MethodGet, wsPath, pt.token, wsHdr)
	if ws0 != ws1 {
		t.Errorf("/api/ws upgrade: real uzp_ got %d %q, unknown got %d %q", ws1.status, ws1.body, ws0.status, ws0.body)
	}
	if ws1.status != http.StatusUnauthorized {
		t.Errorf("/api/ws upgrade with a uzp_ token: status %d, want 401", ws1.status)
	}

	// Non-vacuity: both routers were walked, the route set is not trivially small, and
	// the named surfaces (the WebSocket, public routes, the worker and controller
	// protocols on both listeners) were in it.
	count := map[string]int{}
	has := map[string]bool{}
	for _, w := range seen {
		count[w.router]++
		has[w.router+" "+w.method+" "+w.pattern] = true
	}
	t.Logf("walked %d routes outside /api/v1: Routes=%d WorkerRoutes=%d; statuses %v; RequireUser refusals %d",
		len(seen), count["Routes"], count["WorkerRoutes"], statuses, requireUserRefusals)
	// Positive reach: a large share of the internal API is behind RequireUser, and
	// every such route must answer the unknown uzp_ with RequireUser's own 401. A floor
	// well below today's figure (logged above) catches a walk whose requests stopped
	// reaching the auth layer without pinning an inventory count.
	if requireUserRefusals < 50 {
		t.Fatalf("only %d walked routes answered with RequireUser's 401: the requests are not reaching the auth layer", requireUserRefusals)
	}
	if count["Routes"] < 100 || count["WorkerRoutes"] < 10 {
		t.Fatalf("walked Routes=%d WorkerRoutes=%d: too few for the production routers, the walk is not covering them",
			count["Routes"], count["WorkerRoutes"])
	}
	for _, must := range []string{
		"Routes GET /api/ws",
		"Routes GET /api/health",
		"Routes GET /api/version",
		"Routes GET /api/runs/",
		"Routes POST /api/worker/heartbeat",
		"WorkerRoutes POST /api/worker/heartbeat",
		"WorkerRoutes GET /api/controller/poll",
	} {
		if !has[must] {
			t.Errorf("route %q was not walked", must)
		}
	}

	// No side effect: in particular the token was never touched, which only
	// RequireV1Caller does.
	if after := v1Snapshot(t, pool, owner, pt); after != before {
		t.Fatalf("internal routes changed state for the uzp_ owner:\n before %+v\n after  %+v", before, after)
	}

	// Positive control: the same token IS live, and a v1 request DOES stamp it, so the
	// NULL above is a measurement rather than a token that could never be touched.
	if got := v1Send(v1ProbeRouter(h), http.MethodGet, "/api/v1/probe/read", pt.token, nil); got.status != http.StatusOK {
		t.Fatalf("positive control: the real uzp_ on /api/v1: %d %q, want 200", got.status, got.body)
	}
	if after := v1Snapshot(t, pool, owner, pt); after.TokenLastUsedNull || after.TokenLastIPNull {
		t.Fatalf("positive control: RequireV1Caller did not stamp last_used (%+v)", after)
	}
}

// TestV1AdminStripLiveDB (PRD #1907 D3): an admin's uzp_ and an admin's uzc_ (row scope
// user) both pass RequireV1Caller, and neither the context user nor the principal
// carries IsAdmin. The users row itself stays admin (the clear is on a copy).
func TestV1AdminStripLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	router := v1ProbeRouter(h)

	admin := cliSeedUser(t, pool, true)
	pt := v1MintProductToken(t, h.q, admin, v1SeedProduct(t, h.q, admin), producttoken.Scopes, nil)
	uzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)

	for _, tc := range []struct{ name, token, kind string }{
		{"uzp_", pt.token, string(mw.V1CallerProductToken)},
		{"uzc_", uzc, string(mw.V1CallerCLIToken)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := v1Send(router, http.MethodGet, "/api/v1/probe/read", tc.token, nil)
			if got.status != http.StatusOK {
				t.Fatalf("status %d %q, want 200", got.status, got.body)
			}
			for _, want := range []string{
				"user=" + admin.String(), "okU=true", "okP=true", "kind=" + tc.kind,
				"ctxAdmin=false", "principalAdmin=false",
			} {
				if !strings.Contains(got.body, want) {
					t.Errorf("probe %q lacks %q", got.body, want)
				}
			}
		})
	}
	var stillAdmin bool
	if err := pool.QueryRow(context.Background(), `SELECT is_admin FROM users WHERE id = $1`, admin).Scan(&stillAdmin); err != nil || !stillAdmin {
		t.Fatalf("users.is_admin = %t (err %v): the clear must touch the context copy only", stillAdmin, err)
	}
}

// TestV1CallerLiveDB covers RequireV1Caller's dispatch and fail-closed checks against
// real rows (PRD #1907 D2, D4, D5).
func TestV1CallerLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	router := v1ProbeRouter(h)
	ctx := context.Background()

	owner := cliSeedUser(t, pool, false)
	productID := v1SeedProduct(t, h.q, owner)
	readOnly := v1MintProductToken(t, h.q, owner, productID, []string{producttoken.ScopeJobsRead}, nil)

	// A second user's valid session cookie, to prove a cookie never matters.
	cookieUser := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, cookieUser)
	withCookie := func(method, path, bearer string) v1Resp {
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only request cookie.
		req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return v1Resp{rec.Code, rec.Body.String()}
	}
	want401 := func(t *testing.T, got v1Resp) {
		t.Helper()
		if got.status != http.StatusUnauthorized || !strings.Contains(got.body, "invalid token") {
			t.Fatalf("got %d %q, want the generic 401", got.status, got.body)
		}
	}

	t.Run("uzp_ with a valid cookie takes the product path", func(t *testing.T) {
		got := withCookie(http.MethodGet, "/api/v1/probe/read", readOnly.token)
		if got.status != http.StatusOK {
			t.Fatalf("status %d %q, want 200", got.status, got.body)
		}
		for _, want := range []string{
			"user=" + owner.String(), "kind=" + string(mw.V1CallerProductToken),
			"token=" + readOnly.tokenID.String(), "product=" + productID.String(), "scopes=jobs:read",
		} {
			if !strings.Contains(got.body, want) {
				t.Errorf("probe %q lacks %q (the cookie user is %s)", got.body, want, cookieUser)
			}
		}
	})
	t.Run("unknown uzp_ with a valid cookie is 401", func(t *testing.T) {
		want401(t, withCookie(http.MethodGet, "/api/v1/probe/read", v1UnknownProductToken(t)))
	})
	t.Run("cookie alone is 401", func(t *testing.T) {
		want401(t, withCookie(http.MethodGet, "/api/v1/probe/read", ""))
	})
	t.Run("uza_ is refused", func(t *testing.T) {
		admin := cliSeedUser(t, pool, true)
		want401(t, v1Send(router, http.MethodGet, "/api/v1/probe/read", cliMintToken(t, pool, admin, clitoken.ScopeAdminRO), nil))
	})
	t.Run("a uzc_-shaped value on an admin_ro row is refused", func(t *testing.T) {
		admin := cliSeedUser(t, pool, true)
		tok := "uz" + "c_" + strings.TrimPrefix(v1UnknownProductToken(t), producttoken.Prefix)
		cliMustExec(t, pool,
			`INSERT INTO cli_tokens (user_id, name, token_hash, token_prefix, scope) VALUES ($1, 'ro', $2, 'uzc_x', 'admin_ro')`,
			admin, clitoken.Hash(tok))
		want401(t, v1Send(router, http.MethodGet, "/api/v1/probe/read", tok, nil))
	})
	t.Run("uzc_ scope user holds both scopes and is touched", func(t *testing.T) {
		tok, id := cliInsertToken(t, pool, owner, clitoken.ScopeUser, nil, false)
		for _, path := range []string{"/api/v1/probe/read", "/api/v1/probe/run"} {
			got := v1Send(router, http.MethodGet, path, tok, nil)
			if got.status != http.StatusOK || !strings.Contains(got.body, "scopes=jobs:run,jobs:read") ||
				!strings.Contains(got.body, "kind="+string(mw.V1CallerCLIToken)) || !strings.Contains(got.body, "product= ") {
				t.Fatalf("%s: %d %q, want 200 as a CLI caller with both scopes and no product", path, got.status, got.body)
			}
		}
		var touched bool
		if err := pool.QueryRow(ctx, `SELECT last_used_at IS NOT NULL FROM cli_tokens WHERE id = $1`, id).Scan(&touched); err != nil || !touched {
			t.Fatalf("cli token last_used_at stamped = %t (err %v)", touched, err)
		}
	})
	t.Run("missing scope is 403", func(t *testing.T) {
		got := v1Send(router, http.MethodGet, "/api/v1/probe/run", readOnly.token, nil)
		if got.status != http.StatusForbidden {
			t.Fatalf("jobs:read token on a jobs:run route: %d %q, want 403", got.status, got.body)
		}
	})

	// Each fail-closed D4 condition, applied to its own fresh token.
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name    string
		expires *time.Time
		breakIt func(t *testing.T, owner, product, token uuid.UUID)
	}{
		{name: "revoked", breakIt: func(t *testing.T, _, _, token uuid.UUID) {
			cliMustExec(t, pool, `UPDATE product_tokens SET revoked = true WHERE id = $1`, token)
		}},
		{name: "expired", expires: &past},
		{name: "product disabled", breakIt: func(t *testing.T, _, product, _ uuid.UUID) {
			cliMustExec(t, pool, `UPDATE products SET enabled = false WHERE id = $1`, product)
		}},
		{name: "product soft-deleted", breakIt: func(t *testing.T, _, product, _ uuid.UUID) {
			if n, err := h.q.SoftDeleteProduct(ctx, product); err != nil || n != 1 {
				t.Fatalf("SoftDeleteProduct: %d, %v", n, err)
			}
		}},
		{name: "owner deactivated", breakIt: func(t *testing.T, owner, _, _ uuid.UUID) {
			cliMustExec(t, pool, `UPDATE users SET is_active = false WHERE id = $1`, owner)
		}},
	} {
		t.Run(tc.name+" is 401", func(t *testing.T) {
			u := cliSeedUser(t, pool, false)
			p := v1SeedProduct(t, h.q, u)
			// A token that is live before the break (the future expiry for the expired
			// case is replaced by the past one at mint).
			exp := &future
			if tc.expires != nil {
				exp = tc.expires
			}
			pt := v1MintProductToken(t, h.q, u, p, producttoken.Scopes, exp)
			if tc.breakIt != nil {
				if got := v1Send(router, http.MethodGet, "/api/v1/probe/read", pt.token, nil); got.status != http.StatusOK {
					t.Fatalf("control: the token before the break: %d %q, want 200", got.status, got.body)
				}
				tc.breakIt(t, u, p, pt.tokenID)
			}
			want401(t, v1Send(router, http.MethodGet, "/api/v1/probe/read", pt.token, nil))
		})
	}
}

// Guard for v1ConcretePath / v1IsV1 (pure, runs without a database).
func TestV1WalkHelpers(t *testing.T) {
	ph := "0b0e2a52-5f0c-4c55-9d3a-6f4e0b1c2d3e"
	for in, want := range map[string]string{
		"/api/runs/{id}": "/api/runs/" + ph,
		"/api/runs/{id}/review/recommendations/{recID}": "/api/runs/" + ph + "/review/recommendations/" + ph,
		// A regexp param gets a value its regexp accepts, not the uuid.
		"/api/branding/logo/{slot:[a-z]+}": "/api/branding/logo/a",
		"/api/n/{n:[0-9]+}":                "/api/n/1",
		// A regexp the uuid itself satisfies keeps the uuid.
		"/api/u/{id:[0-9a-f-]+}": "/api/u/" + ph,
		"/api/x/*":               "/api/x/x",
		"/api/health":            "/api/health",
	} {
		if got, err := v1ConcretePath(in, ph); err != nil || got != want {
			t.Errorf("v1ConcretePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// No candidate satisfies it: a loud error, never an unroutable path.
	if got, err := v1ConcretePath("/api/z/{z:[A-Z]+}", ph); err == nil {
		t.Errorf("v1ConcretePath with an unsatisfiable regexp = %q, want an error", got)
	}
	for in, want := range map[v1Resp]bool{
		{http.StatusNotFound, "404 page not found\n"}:                    true,
		{http.StatusMethodNotAllowed, ""}:                                true,
		{http.StatusNotFound, "{\"error\":\"run not found\"}\n"}:         false,
		{http.StatusUnauthorized, "{\"error\":\"invalid CLI token\"}\n"}: false,
	} {
		if got := v1RoutingMiss(in); got != want {
			t.Errorf("v1RoutingMiss(%+v) = %t, want %t", in, got, want)
		}
	}
	for in, want := range map[string]bool{
		"/api/v1": true, "/api/v1/": true, "/api/v1/whoami": true,
		"/api/v10": false, "/api/v1x/a": false, "/api/vault": false, "/api/runs/": false,
	} {
		if got := v1IsV1(in); got != want {
			t.Errorf("v1IsV1(%q) = %t, want %t", in, got, want)
		}
	}
	if !slices.Equal(producttoken.Scopes, []string{producttoken.ScopeJobsRun, producttoken.ScopeJobsRead}) {
		t.Fatalf("producttoken.Scopes order changed; update the uzc_ scopes assertion in TestV1CallerLiveDB")
	}
}
