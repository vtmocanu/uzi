package handler

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/hub"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// M2's CEILING tests (PRD #64), the milestone's real deliverable. They exist as a
// live-DB suite, and against the REAL h.Routes() router, on purpose: the property
// under test is the token's authority CEILING — enforced by the RequireUser masking
// and by which routes carry RequireUser vs RequireAuth — and a fake store cannot
// exhibit cross-user visibility, nor can a hand-rolled middleware chain prove that
// handler.go wired the ACTUAL route the way the security model requires. A suite
// that only checked "uzc_ is 403 by /api/admin/*" would go green with the F1 hole
// wide open, so these test the property, not the route prefix.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// cliTestSecret is a 32-byte HS256 key. Config validation is not run here (we build
// Config directly), so any non-empty secret works for IssueToken/ParseToken.
var cliTestSecret = []byte("0123456789abcdef0123456789abcdef")

// cliLiveDB builds a live-DB-backed Handler wired with the collaborators the ceiling
// tests actually reach (wsvc for run reads, settings for /auth/me + the admin
// settings read), plus its full Routes() router mounting the real RequireUser /
// RequireAdminRO chain. The limiters are generous (these are not rate-limit tests).
func cliLiveDB(t *testing.T) (*Handler, http.Handler, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	h := &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg: config.Config{
			JWTSecret:    cliTestSecret,
			AuthTokenTTL: time.Hour,
		},
		wsvc:     workersvc.New(q, box, workersvc.Params{}),
		settings: settings.New(&settingsStore{}, time.Minute),
		hub:      hub.New(),
	}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim)
	return h, router, pool
}

func cliMustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// cliSeedUser inserts a fresh user (unique email; the runner shares one DB across
// the LiveDB set) and returns its id.
func cliSeedUser(t *testing.T, pool *pgxpool.Pool, isAdmin bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', $3)`,
		id, fmt.Sprintf("cli-%s@e2e", id), isAdmin)
	return id
}

// cliInsertToken inserts a cli_tokens row for userID and returns the plaintext token
// plus the row id. expiresAt nil = NULL (never expires); revoked toggles the flag.
// Those two knobs are the fail-closed half of GetCLITokenByHash's NULL trap — a NULL
// expiry is accepted while a past expiry or a revoked row is not.
func cliInsertToken(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, scope string, expiresAt *time.Time, revoked bool) (string, uuid.UUID) {
	t.Helper()
	token, hash, prefix, err := clitoken.Generate(scope)
	if err != nil {
		t.Fatalf("clitoken.Generate: %v", err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO cli_tokens (user_id, name, token_hash, token_prefix, scope, expires_at, revoked)
		 VALUES ($1, 'test', $2, $3, $4, $5, $6) RETURNING id`,
		userID, hash, prefix, scope, expiresAt, revoked).Scan(&id); err != nil {
		t.Fatalf("insert cli token: %v", err)
	}
	return token, id
}

// cliMintToken inserts a NULL-expiry, un-revoked cli_tokens row and returns the
// plaintext token — the never-expiring agent/CI token the NULL-trap path serves,
// which is exactly what most of these tests care about; expiry is not what they
// exercise.
func cliMintToken(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, scope string) string {
	t.Helper()
	token, _ := cliInsertToken(t, pool, userID, scope, nil, false)
	return token
}

// cookieReq drives router on the browser (cookie + CSRF) path for userID's session
// JWT, with an optional JSON body. The cli-tokens CRUD routes are cookie-only
// (RequireAuth), so their success/gate paths are reachable only this way.
func cookieReq(t *testing.T, router http.Handler, method, path, jwt, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt})
	req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// cliSeedJudgedRun seeds a forge connection, a repo, a completed run owned by
// ownerID, and a JUDGED review (verdict + one recommendation) on that run. The
// review's presence is load-bearing for the /review trio assertion: an unjudged
// foreign run returns 404 too, but only a judged one proves VERDICT CONTENT never
// crosses to a masked admin token. Returns the target run id.
func cliSeedJudgedRun(t *testing.T, pool *pgxpool.Pool, ownerID uuid.UUID) uuid.UUID {
	t.Helper()
	connID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, ownerID, []byte{0x1})
	cliMustExec(t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'desc', 'completed', 'issue')`, runID, ownerID, repoID)
	cliMustExec(t, pool,
		`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'text', $2)`,
		runID, []byte(`{"text":"hello"}`))

	var reviewID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO run_reviews (target_run_id, user_id, verdict, summary_md, status)
		 VALUES ($1, $2, 'issues', 'a summary that must not cross', 'complete') RETURNING id`,
		runID, ownerID).Scan(&reviewID); err != nil {
		t.Fatalf("insert review: %v", err)
	}
	cliMustExec(t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
		 VALUES ($1, 'improve_uzi', 'x', 'because', 'high')`, reviewID)
	return runID
}

// bearerReq drives router with an Authorization: Bearer credential and no cookie.
func bearerReq(router http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// bearerReqBody is bearerReq with a JSON body, for the write routes (e.g. task-runs)
// whose handlers 400 on an empty body. token "" sends no Authorization header (the
// no-credential path); body "" sends no body.
func bearerReqBody(router http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// cliSeedOwnedRepo seeds a forge connection and one enabled repo (default_branch
// 'main') owned by ownerID, and returns the repo id. It mirrors the connection+repo
// half of cliSeedJudgedRun, for tests that only need an owned repo to POST to.
func cliSeedOwnedRepo(t *testing.T, pool *pgxpool.Pool, ownerID uuid.UUID) uuid.UUID {
	t.Helper()
	connID, repoID := uuid.New(), uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, ownerID, []byte{0x1})
	cliMustExec(t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	return repoID
}

// cliMintJWT issues a valid session JWT cookie value for userID, reading the row's
// live token_version so RequireAuth's revocation check passes (the users default is
// not 0).
func cliMintJWT(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) string {
	t.Helper()
	var tokenVersion int32
	if err := pool.QueryRow(context.Background(), `SELECT token_version FROM users WHERE id = $1`, userID).Scan(&tokenVersion); err != nil {
		t.Fatalf("read token_version: %v", err)
	}
	tok, err := auth.IssueToken(cliTestSecret, userID.String(), tokenVersion, time.Hour)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	return tok
}

// cliCSRFHeader computes the CSRF header value ValidateCSRF expects for a state-
// changing request: hex(nonce).hex(hmac_sha256(key=authCookieValue, nonce)).
func cliCSRFHeader(t *testing.T, jwt string) string {
	t.Helper()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(jwt))
	mac.Write(nonce)
	// Encoding must match production generateCSRFToken (base64url, "." separator);
	// ValidateCSRF base64url-decodes both halves before recomputing the MAC.
	enc := base64.RawURLEncoding
	return enc.EncodeToString(nonce) + "." + enc.EncodeToString(mac.Sum(nil))
}

// -------------------------------------------------------------------------
// (a) The load-bearing trio: an admin's uzc_ gets 404 on ANOTHER user's run,
// messages, AND review — the review fixture JUDGED — while the owner's token and
// the admin's uza_ both see it. That triangulation proves the 404 is the F1 masking
// (an admin's default-scope token reduced to owner-only), not a broken fixture.
// -------------------------------------------------------------------------

func TestCLICeilingTrioCrossUser404LiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)

	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)    // masked to IsAdmin=false
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO) // keeps IsAdmin
	ownerUzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)

	paths := []string{
		"/api/runs/" + runID.String(),
		"/api/runs/" + runID.String() + "/messages",
		"/api/runs/" + runID.String() + "/review",
	}
	for _, p := range paths {
		// The ceiling: admin's uzc_ is owner-only ⇒ another user's run is 404.
		if rec := bearerReq(router, http.MethodGet, p, adminUzc); rec.Code != http.StatusNotFound {
			t.Errorf("admin uzc_ GET %s = %d, want 404 (masking makes it owner-only)\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// The owner sees their own run: proves the fixture is genuinely visible, so the
		// 404 above is the masking, not a missing row.
		if rec := bearerReq(router, http.MethodGet, p, ownerUzc); rec.Code != http.StatusOK {
			t.Errorf("owner uzc_ GET %s = %d, want 200 (owner sees own run)\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// The admin's uza_ (unmasked) sees it: proves the account really is admin, so
		// the uzc_ 404 is specifically the scope ceiling, not a non-admin account.
		if rec := bearerReq(router, http.MethodGet, p, adminUza); rec.Code != http.StatusOK {
			t.Errorf("admin uza_ GET %s = %d, want 200 (admin_ro sees any run)\nbody: %s", p, rec.Code, rec.Body.String())
		}
	}

	// The /review body must be a real 404, not a 200 {"review":null}: verdict content
	// (the seeded review + recommendation) must never reach the masked admin token.
	rec := bearerReq(router, http.MethodGet, "/api/runs/"+runID.String()+"/review", adminUzc)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin uzc_ /review = %d, want 404 (a null review would leak that the run exists + verdict crossing)\nbody: %s", rec.Code, rec.Body.String())
	}
	// And the owner's /review is genuinely a judged, non-null review — otherwise the
	// judged-fixture requirement is not actually met.
	rec = bearerReq(router, http.MethodGet, "/api/runs/"+runID.String()+"/review", ownerUzc)
	var body struct {
		Review *json.RawMessage `json:"review"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("owner /review decode: %v (body %s)", err, rec.Body.String())
	}
	if body.Review == nil {
		t.Fatalf("owner /review is null; the trio fixture must be JUDGED for the test to prove verdict non-crossing")
	}
}

// -------------------------------------------------------------------------
// (b) Self-report honesty: /auth/me over a uzc_ reports is_admin:false (the masking
// copy is what makes this true), over a uza_ reports is_admin:true.
// -------------------------------------------------------------------------

func TestCLISelfReportHonestyLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	uzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	meAdmin := func(token string) bool {
		rec := bearerReq(router, http.MethodGet, "/api/auth/me", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("/auth/me = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			User struct {
				IsAdmin bool `json:"is_admin"`
			} `json:"user"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("/auth/me decode: %v (body %s)", err, rec.Body.String())
		}
		return body.User.IsAdmin
	}

	if meAdmin(uzc) {
		t.Errorf("/auth/me over uzc_ reported is_admin:true; the masking must report this credential's authority (false)")
	}
	if !meAdmin(uza) {
		t.Errorf("/auth/me over uza_ reported is_admin:false; an admin_ro token keeps admin")
	}
}

// -------------------------------------------------------------------------
// (c/d) Cross-credential-class + spend containment: a Bearer credential is rejected
// on the cookie-only routes — the D18 POST /api/workers (mint of a plaintext uzw_
// whose claim yields decrypted secrets) and the D21 POST /api/runs/{id}/rejudge
// (one of two cookie-only /runs routes, with wait-on-limit; catches the "wrap the
// whole /runs group" shortcut), plus the other PAT-writing / factory-killing /
// run-minting routes.
// -------------------------------------------------------------------------

func TestCLIRejectsBearerOnCookieOnlyRoutesLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	runID := uuid.New()

	cases := []struct {
		name, method, path string
	}{
		{"mint worker (D18: strictly worse than PAT-write)", http.MethodPost, "/api/workers"},
		{"rejudge (D21: one of two cookie-only /runs routes, with wait-on-limit)", http.MethodPost, "/api/runs/" + runID.String() + "/rejudge"},
		{"forge connection (writes a bot PAT)", http.MethodPost, "/api/forge/connections"},
		{"delete anthropic token (factory kill)", http.MethodDelete, "/api/me/secrets/anthropic_token"},
		{"create chat (mints a run)", http.MethodPost, "/api/chats"},
		{"logout (must not bump token_version)", http.MethodPost, "/api/auth/logout"},
		{"cli-token CRUD self-mint (Decision 16)", http.MethodPost, "/api/me/cli-tokens"},
		// PRD #111 D23 SPLIT THE GROUP THESE TWO LIVE IN, and this is the half that
		// catches a mistake there. GET /me/rate-limits moved out to RequireUser; these
		// two consent switches stayed cookie-only, and they must NOT have travelled
		// with it. route_limiter_mounts_test pins the LIMITER, not the auth
		// middleware, so an implementation that widened all three passes it.
		{"autopilot opt-in (spends unattended, D23 group split)", http.MethodPut, "/api/me/autopilot"},
		{"judge opt-in (spends unattended, D23 group split)", http.MethodPut, "/api/me/judge"},
		// PRD #35 Decision 7. Both wait-on-limit routes are cookie-only for the same
		// reason as their neighbours: consent to uzi holding an issue's one-active lock
		// and a worker's disk for up to RUN_LIMIT_MAX_PARK on the caller's behalf. Listed
		// here because route_limiter_mounts_test pins the LIMITER, not the auth
		// middleware, so an implementation that mounted these on RequireUser would pass
		// that test and silently widen a consent switch to a stolen CLI token.
		{"wait-on-limit default (park consent)", http.MethodPut, "/api/me/wait-on-limit"},
		{"wait-on-limit per run (park consent)", http.MethodPut, "/api/runs/" + runID.String() + "/wait-on-limit"},
		// The /me/settings group was split like /me/rate-limits: GET moved to
		// RequireUser so a uzc_ can read it (see TestCLIReachesSettingsOverBearerLiveDB),
		// but the PUT write path stayed cookie-only and must NOT have travelled with it.
		{"settings write (split group, PUT stays cookie-only)", http.MethodPut, "/api/me/settings"},
	}
	for _, tc := range cases {
		rec := bearerReq(router, tc.method, tc.path, uzc)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %s %s over Bearer = %d, want 401 (cookie-only route rejects the bearer path)\nbody: %s",
				tc.name, tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestCLIReachesRateLimitsOverBearerLiveDB is D23's other half: the GET actually
// MOVED. Asserting only that the two PUTs still 401 would pass on a tree where
// nothing changed at all.
//
// The rationale is non-additivity, not a sensitivity ranking (see handler.go): every
// inference this enables is already available at finer granularity through routes
// already reachable by this same token — GET /api/runs carries a timestamped
// per-run cost series, and POST /repos/{id}/runs lets a stolen uzc_ SPEND the quota
// outright. It is a GET of the caller's own row: no outbound call, no poke, no
// IsAdmin branch.
func TestCLIReachesRateLimitsOverBearerLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)

	rec := bearerReq(router, http.MethodGet, "/api/me/rate-limits", uzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me/rate-limits over Bearer = %d, want 200 — D23 moved it to "+
			"RequireUser so `uzi token list` can show live eligibility\nbody: %s", rec.Code, rec.Body.String())
	}
	// A token-less user's meters are an empty array, which is the shape the CLI
	// decodes; asserting it here keeps the move from passing on an error envelope
	// that happened to be 200.
	if body := rec.Body.String(); !strings.Contains(body, `"tokens"`) {
		t.Fatalf("response carries no tokens array: %s", body)
	}
	// And an UNAUTHENTICATED request is still refused — the move is cookie→bearer,
	// not authenticated→public.
	if rec := bearerReq(router, http.MethodGet, "/api/me/rate-limits", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/me/rate-limits = %d, want 401", rec.Code)
	}
}

// TestCLIReachesSettingsOverBearerLiveDB mirrors the rate-limits move for
// GET /api/me/settings: the read was split out of the cookie-only /me/settings
// group onto RequireUser so a uzc_ Bearer can decode the caller's own settings
// (including sidebar_token_ids), while the PUT write stays cookie-only (asserted
// in TestCLIRejectsBearerOnCookieOnlyRoutesLiveDB). Asserting only the PUT-401
// half would pass on a tree where the GET never moved, so this pins the GET-200.
func TestCLIReachesSettingsOverBearerLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)

	rec := bearerReq(router, http.MethodGet, "/api/me/settings", uzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me/settings over Bearer = %d, want 200 — the GET was split to "+
			"RequireUser so the CLI can read the caller's own settings\nbody: %s", rec.Code, rec.Body.String())
	}
	// The caller's own settings ride a {"settings": {...}} envelope, which is the
	// shape the CLI decodes; asserting it keeps the move from passing on an error
	// envelope that happened to be 200.
	if body := rec.Body.String(); !strings.Contains(body, `"settings"`) {
		t.Fatalf("response carries no settings object: %s", body)
	}
	// And an UNAUTHENTICATED request is still refused — the move is cookie→bearer,
	// not authenticated→public.
	if rec := bearerReq(router, http.MethodGet, "/api/me/settings", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/me/settings = %d, want 401", rec.Code)
	}
}

// -------------------------------------------------------------------------
// (e) CSRF-bypass shape: a cookie request carrying a bogus Authorization header is
// rejected on the bearer path and NEVER silently falls back to the cookie path;
// and the cookie path still enforces CSRF on writes through RequireUser.
// -------------------------------------------------------------------------

func TestCLICSRFBypassShapeLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)

	// Baseline: a valid cookie alone reaches the RequireUser handler (GET, no CSRF).
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid cookie GET /auth/me = %d, want 200 (cookie path must work)\nbody: %s", rec.Code, rec.Body.String())
	}

	// The attack: the SAME valid cookie plus a bogus Authorization header. Presence-
	// dispatch takes the bearer path (bogus token → 401) and must NOT fall back to the
	// cookie path — which would be the CSRF bypass.
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt})
	req.Header.Set("Authorization", "Bearer uzc_not-a-real-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie + bogus Bearer GET /auth/me = %d, want 401 (must NOT fall back to the cookie path)\nbody: %s", rec.Code, rec.Body.String())
	}

	// The cookie path still enforces CSRF on writes through RequireUser: a valid cookie
	// POST without the CSRF header is 403 (the unchanged RequireAuth pin).
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+uuid.New().String()+"/inputs", nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST /runs/{id}/inputs without CSRF = %d, want 403 (cookie path enforces CSRF through RequireUser)\nbody: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (f) Admin surface: a uza_ reads all 9 admin GETs and is rejected by all 4 admin
// writes; a uzc_ is rejected by every admin read; /api/vault/* rejects any CLI
// token; live demotion of the owner kills the uza_'s admin reads with no revoke.
// -------------------------------------------------------------------------

var adminReadPaths = []string{
	"/api/admin/users",
	"/api/admin/settings",
	"/api/admin/vault-migration",
	"/api/admin/slack/status",
	"/api/admin/workers",
	"/api/admin/runs",
	"/api/admin/usage",
	"/api/admin/rate-limits",
	// The standing-credential inventory. It belongs here for a sharper reason than
	// the others: it is the one admin read whose BODY is itself security-relevant, so
	// "a user-scope uzc_ cannot reach it" is the assertion that keeps a token holder
	// from enumerating every credential in the factory.
	"/api/admin/cli-tokens",
}

func TestCLIAdminSurfaceLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	uzc := cliMintToken(t, pool, admin, clitoken.ScopeUser) // admin owner, but user scope

	// A uza_ reads every admin GET in adminReadPaths; the same admin's uzc_ is
	// rejected by every one (the masking, not the route prefix, is doing the work
	// here). Deliberately not a count — the list grows, and a tally in a comment goes
	// stale exactly like a line number.
	for _, p := range adminReadPaths {
		if rec := bearerReq(router, http.MethodGet, p, uza); rec.Code != http.StatusOK {
			t.Errorf("uza_ GET %s = %d, want 200\nbody: %s", p, rec.Code, rec.Body.String())
		}
		if rec := bearerReq(router, http.MethodGet, p, uzc); rec.Code != http.StatusForbidden {
			t.Errorf("uzc_ GET %s = %d, want 403 (admin's default-scope token is masked to non-admin)\nbody: %s", p, rec.Code, rec.Body.String())
		}
	}

	// These admin WRITES are cookie-only: even a uza_ Bearer is rejected at the
	// middleware (RequireAuth), so a read-only token reaching a write handler is
	// structurally impossible.
	writes := []struct{ method, path string }{
		{http.MethodPatch, "/api/admin/users/" + uuid.New().String()},
		{http.MethodPut, "/api/admin/users/" + uuid.New().String() + "/judge"},
		{http.MethodPut, "/api/admin/settings"},
	}
	for _, wr := range writes {
		if rec := bearerReq(router, wr.method, wr.path, uza); rec.Code != http.StatusUnauthorized {
			t.Errorf("uza_ %s %s = %d, want 401 (admin writes are cookie-only)\nbody: %s", wr.method, wr.path, rec.Code, rec.Body.String())
		}
	}

	// /api/vault/* rejects any CLI token — the password surface is cookie-only.
	if rec := bearerReq(router, http.MethodGet, "/api/vault/status", uza); rec.Code != http.StatusUnauthorized {
		t.Errorf("uza_ GET /api/vault/status = %d, want 401 (vault is cookie-only)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(router, http.MethodPost, "/api/vault/unlock", uzc); rec.Code != http.StatusUnauthorized {
		t.Errorf("uzc_ POST /api/vault/unlock = %d, want 401 (vault is cookie-only)\nbody: %s", rec.Code, rec.Body.String())
	}

	// Live demotion: clearing the owner's is_admin makes the uza_'s admin reads fail
	// on the very next request — no revocation step, because admin-ness is read live
	// from the row (never cached in the credential).
	cliMustExec(t, pool, `UPDATE users SET is_admin = false WHERE id = $1`, admin)
	if rec := bearerReq(router, http.MethodGet, "/api/admin/users", uza); rec.Code != http.StatusForbidden {
		t.Errorf("after demotion, uza_ GET /api/admin/users = %d, want 403 (live demotion, no revoke)\nbody: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (f2) PRD #602 M4 differential-auth for the agent-source mount, through the REAL
// router + middleware stack. The two writes (POST sync, POST apply) are the primary
// supply-chain surface, so this pins that they sit under the cookie-only admin WRITE
// group (RequireAuth + RequireAdmin), not the read group:
//
//   - GET /admin/agent-source is reachable by a read-only admin uza_ Bearer (200),
//     while a masked-to-non-admin uzc_ is 403 — same read-group property as the other
//     admin reads.
//   - POST sync/apply reject a read-only admin uza_ Bearer with 401 (cookie-only: the
//     write group's RequireAuth rejects the Bearer path before any handler runs) AND a
//     non-admin SESSION with 403 (RequireAdmin), while an admin session passes auth
//     (not 401/403) — proving the routes are genuinely mounted behind the write gate,
//     not merely absent (an absent route would 404, failing the reject assertions).
//
// A direct handler call cannot prove any of this: only the mounted router runs the
// middleware chain that IS the control.
func TestCLIAgentSourceAuthMountLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	nonAdmin := cliSeedUser(t, pool, false)
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO) // read-only admin
	uzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)    // admin owner, masked to non-admin

	const readPath = "/api/admin/agent-source"

	// Read: uza_ reaches it (200), uzc_ is masked out (403).
	if rec := bearerReq(router, http.MethodGet, readPath, uza); rec.Code != http.StatusOK {
		t.Errorf("uza_ GET %s = %d, want 200 (read-only admin reads agent-source)\nbody: %s", readPath, rec.Code, rec.Body.String())
	}
	if rec := bearerReq(router, http.MethodGet, readPath, uzc); rec.Code != http.StatusForbidden {
		t.Errorf("uzc_ GET %s = %d, want 403 (masked non-admin)\nbody: %s", readPath, rec.Code, rec.Body.String())
	}

	// Writes: both POSTs reject the read-only admin Bearer (401) and a non-admin
	// session (403), while an admin session passes the auth gate (not 401/403).
	writePaths := []string{"/api/admin/agent-source/sync", "/api/admin/agent-source/apply", "/api/admin/agent-source/resolve-latest"}
	for _, p := range writePaths {
		// Read-only admin uza_ Bearer: cookie-only write group rejects the Bearer path.
		if rec := bearerReqBody(router, http.MethodPost, p, uza, `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("uza_ POST %s = %d, want 401 (agent-source writes are cookie-only)\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// Non-admin session (valid cookie + CSRF): passes RequireAuth, rejected by RequireAdmin.
		if rec := cookieReq(t, router, http.MethodPost, p, cliMintJWT(t, pool, nonAdmin), `{}`); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin session POST %s = %d, want 403 (RequireAdmin)\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// Admin session: passes the whole auth gate — the route IS mounted behind the
		// write group (not absent). The handler then runs (500 here: cliLiveDB wires no
		// reconciler), so assert only that auth did NOT reject it.
		if rec := cookieReq(t, router, http.MethodPost, p, cliMintJWT(t, pool, admin), `{}`); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("admin session POST %s = %d, want the auth gate to PASS (not 401/403); the route must be mounted behind the write group\nbody: %s", p, rec.Code, rec.Body.String())
		}
	}
}

// -------------------------------------------------------------------------
// (g) revoke-all revokes every un-revoked token of the CALLER and nobody else's,
// and is idempotent. Cookie-only, so it exercises the real RequireAuth+CSRF path.
// -------------------------------------------------------------------------

func TestCLIRevokeAllLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	caller := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	cliMintToken(t, pool, caller, clitoken.ScopeUser)
	cliMintToken(t, pool, caller, clitoken.ScopeUser)
	cliMintToken(t, pool, other, clitoken.ScopeUser)

	countRevoked := func(userID uuid.UUID) (revoked, total int) {
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FILTER (WHERE revoked), count(*) FROM cli_tokens WHERE user_id = $1`, userID).
			Scan(&revoked, &total); err != nil {
			t.Fatalf("count: %v", err)
		}
		return
	}

	jwt := cliMintJWT(t, pool, caller)
	revokeAll := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/me/cli-tokens/revoke-all", nil)
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt})
		req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := revokeAll(); code != http.StatusNoContent {
		t.Fatalf("revoke-all = %d, want 204", code)
	}
	if rev, total := countRevoked(caller); rev != total || total != 2 {
		t.Errorf("caller: %d/%d revoked, want 2/2", rev, total)
	}
	if rev, _ := countRevoked(other); rev != 0 {
		t.Errorf("other user's token was revoked (%d); revoke-all must be caller-scoped", rev)
	}

	// Idempotent: a second call is a no-op that still returns 204 and touches nothing.
	if code := revokeAll(); code != http.StatusNoContent {
		t.Fatalf("second revoke-all = %d, want 204 (idempotent)", code)
	}
	if rev, _ := countRevoked(other); rev != 0 {
		t.Errorf("other user's token revoked after idempotent second call (%d)", rev)
	}
}

// -------------------------------------------------------------------------
// (h) admin_ro mint gate (privilege-escalation guard, cli_tokens.go:123). A NON-admin
// minting scope=admin_ro is 403; an ADMIN minting admin_ro succeeds — so the gate is
// proven to gate on admin-ness, not to always-fail. Cookie-only path (RequireAuth):
// this route is DELIBERATELY not Bearer-reachable, so a stolen uzc_ can never mint a
// uza_ (Decision 16). is_admin is read live from the row, never from the credential.
// -------------------------------------------------------------------------

func TestCLIAdminROMintGateLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	nonAdmin := cliSeedUser(t, pool, false)

	// A non-admin minting admin_ro is forbidden — the guard, not a masking artifact
	// (this is the cookie path, where no masking runs).
	rec := cookieReq(t, router, http.MethodPost, "/api/me/cli-tokens",
		cliMintJWT(t, pool, nonAdmin), `{"name":"esc","scope":"admin_ro"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin mint admin_ro = %d, want 403 (privilege-escalation guard)\nbody: %s", rec.Code, rec.Body.String())
	}

	// An admin minting admin_ro succeeds (201 Created): proves the gate keys off
	// admin-ness, not that admin_ro always fails.
	rec = cookieReq(t, router, http.MethodPost, "/api/me/cli-tokens",
		cliMintJWT(t, pool, admin), `{"name":"ok","scope":"admin_ro"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("admin mint admin_ro = %d, want 201 (gate passes for an admin)\nbody: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (i) The fail-closed half of GetCLITokenByHash's NULL trap: a past-expiry token and a
// revoked token are both rejected at the RequireUser bearer path (401). Every other
// LiveDB token is NULL-expiry + un-revoked, so this is the only place the closed half
// is exercised (elsewhere it is only mutation-proven).
// -------------------------------------------------------------------------

func TestCLIExpiredRevokedReject401LiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)

	past := time.Now().Add(-time.Hour)
	expired, _ := cliInsertToken(t, pool, user, clitoken.ScopeUser, &past, false)
	revoked, _ := cliInsertToken(t, pool, user, clitoken.ScopeUser, nil, true)

	// Sanity: a NULL-expiry, un-revoked token of the SAME user IS accepted, so a 401
	// below is the expiry/revocation and not a broken fixture or a wrong route.
	valid := cliMintToken(t, pool, user, clitoken.ScopeUser)
	if rec := bearerReq(router, http.MethodGet, "/api/auth/me", valid); rec.Code != http.StatusOK {
		t.Fatalf("valid uzc_ GET /auth/me = %d, want 200 (fixture sanity)\nbody: %s", rec.Code, rec.Body.String())
	}

	if rec := bearerReq(router, http.MethodGet, "/api/auth/me", expired); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired token GET /auth/me = %d, want 401 (fail closed on a past expires_at)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(router, http.MethodGet, "/api/auth/me", revoked); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token GET /auth/me = %d, want 401 (fail closed on revoked)\nbody: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (j) cli-tokens CRUD Bearer-reject completeness + single-delete owner-scoping. The
// POST is already pinned in the shared cookie-only group (c/d); GET, DELETE /{id}, and
// revoke-all share that group but are pinned here explicitly. And a single-delete of
// ANOTHER user's token id is a 404, never a cross-user revoke — mirroring the
// revoke-all cross-user test (g).
// -------------------------------------------------------------------------

func TestCLICRUDBearerRejectAndOwnerScopeLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)

	// Every cli-tokens CRUD verb is cookie-only: a Bearer credential is rejected (401).
	bearerReject := []struct{ name, method, path string }{
		{"list", http.MethodGet, "/api/me/cli-tokens"},
		{"revoke one", http.MethodDelete, "/api/me/cli-tokens/" + uuid.New().String()},
		{"revoke all", http.MethodPost, "/api/me/cli-tokens/revoke-all"},
	}
	for _, tc := range bearerReject {
		if rec := bearerReq(router, tc.method, tc.path, uzc); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s over Bearer = %d, want 401 (cli-tokens CRUD is cookie-only)\nbody: %s", tc.name, rec.Code, rec.Body.String())
		}
	}

	// Single-delete owner-scoping: an attacker with a valid session deleting the
	// victim's token id touches zero rows ⇒ 404, never a cross-user revoke.
	owner := cliSeedUser(t, pool, false)
	attacker := cliSeedUser(t, pool, false)
	_, victimID := cliInsertToken(t, pool, owner, clitoken.ScopeUser, nil, false)

	if rec := cookieReq(t, router, http.MethodDelete, "/api/me/cli-tokens/"+victimID.String(),
		cliMintJWT(t, pool, attacker), ""); rec.Code != http.StatusNotFound {
		t.Errorf("attacker DELETE owner's token = %d, want 404 (never cross-user revoke)\nbody: %s", rec.Code, rec.Body.String())
	}
	// The victim's own DELETE succeeds (204): proves the 404 was the scoping, not a
	// missing/already-revoked row.
	if rec := cookieReq(t, router, http.MethodDelete, "/api/me/cli-tokens/"+victimID.String(),
		cliMintJWT(t, pool, owner), ""); rec.Code != http.StatusNoContent {
		t.Errorf("owner DELETE own token = %d, want 204 (proves the 404 was scoping, not a missing row)\nbody: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (k) POST /repos/{id}/task-runs (uzi handoff, PRD #400) is RequireUser, not the
// cookie-only RequireAuth: a CLI Bearer token must reach it exactly like POST
// /repos/{id}/runs. This is the issue #428 regression — the route was mounted in the
// cookie-only group, so every CLI Bearer 401'd (uzi handoff exited 3). Driven through
// the real mounted router so the middleware chain (RequireUser's presence-dispatch)
// is what decides, not a direct handler call.
// -------------------------------------------------------------------------

func TestCLITaskRunsBearerReachableLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	repoID := cliSeedOwnedRepo(t, pool, user)

	path := "/api/repos/" + repoID.String() + "/task-runs"
	const body = `{"context":"do the thing"}`

	// 1. A valid uzc_ Bearer, no cookie, reaches the owner-scoped handler ⇒ 201. The
	// regression made this 401 (route was cookie-only), which is what broke uzi handoff.
	if rec := bearerReqBody(router, http.MethodPost, path, uzc, body); rec.Code != http.StatusCreated {
		t.Errorf("valid uzc_ POST %s = %d, want 201 (RequireUser must accept the CLI Bearer path)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// 2. No credential at all is still refused ⇒ 401. Proves the 201 above is the
	// credential being honoured, not an unauthenticated route.
	if rec := bearerReqBody(router, http.MethodPost, path, "", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-credential POST %s = %d, want 401 (task-runs is authenticated)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// 3. The browser path is unchanged: a valid session cookie + a valid CSRF header
	// reaches the same handler ⇒ 201.
	if rec := cookieReq(t, router, http.MethodPost, path, cliMintJWT(t, pool, user), body); rec.Code != http.StatusCreated {
		t.Errorf("valid cookie+CSRF POST %s = %d, want 201 (browser path unchanged)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// 4. Presence-dispatch is preserved: a valid session cookie plus a BOGUS
	// Authorization header takes the bearer path (bad token → 401) and must NOT fall
	// back to the cookie path — mirrors TestCLICSRFBypassShapeLiveDB's bogus-Bearer
	// assertion, now for a write route.
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: cliMintJWT(t, pool, user)})
	req.Header.Set("Authorization", "Bearer uzc_not-a-real-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie + bogus Bearer POST %s = %d, want 401 (presence-dispatch must not fall back to cookie)\nbody: %s", path, rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (l) PRD #365 M1 moved THREE filing WRITES from the cookie-only RequireAuth path
// to RequireUser, so `uzi review file` / `uzi findings file` / `uzi findings dismiss`
// work from a uzc_ Bearer token:
//
//	POST /api/findings/{id}/dismiss
//	POST /api/findings/{id}/issue
//	POST /api/runs/{id}/review/recommendations/{recID}/issue
//
// This is the positive counterpart to TestCLIRejectsBearerOnCookieOnlyRoutesLiveDB
// (which must still pass — these three routes are deliberately NOT in its list). The
// crux is NOT 401: pre-M1 the cookie-only mount rejected the Bearer credential at the
// middleware before any handler ran, so a uzc_ POST returned 401. After M1 the
// credential is honoured and the handler runs. Driven through the real mounted
// router so RequireUser's presence-dispatch is what decides, not a direct handler call.
//
// For a random (nonexistent) coordinate every one of the three returns a CLEAN 404 at
// its owner-scoped lookup, WITHOUT ever calling the forge — so no fake forge is needed,
// and the 404 doubles as the owner-scoping assertion over the Bearer path (no leak).
// The exact ordering that makes each a 404 (and not a 400) was confirmed by reading the
// handlers: FileIssue parses run/rec/body/repo then 404s at GetReviewForTarget;
// FileFinding accepts an empty {} body then 404s at GetIncidentalFinding; DismissFinding
// decodes+validates the reason (so a VALID {"reason":"wont_do"} is required to reach the
// clean 404) then 404s at GetIncidentalFinding.
func TestCLIReachesFilingRoutesOverBearerLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)

	runID := uuid.New()
	recID := uuid.New()
	findingID := uuid.New()
	repoID := uuid.New()

	cases := []struct {
		name, path, body string
		want             int
	}{
		{
			// A valid reason is mandatory to reach the owner lookup: an empty/invalid body
			// is a 400 (still not 401), but a valid one gives the cleaner owner-scoped 404.
			name: "findings dismiss",
			path: "/api/findings/" + findingID.String() + "/dismiss",
			body: `{"reason":"wont_do"}`,
			want: http.StatusNotFound,
		},
		{
			name: "findings file",
			path: "/api/findings/" + findingID.String() + "/issue",
			body: `{}`,
			want: http.StatusNotFound,
		},
		{
			name: "review file",
			path: "/api/runs/" + runID.String() + "/review/recommendations/" + recID.String() + "/issue",
			body: `{"repo_id":"` + repoID.String() + `","title":"t","description":"d"}`,
			want: http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		rec := bearerReqBody(router, http.MethodPost, tc.path, uzc, tc.body)
		// The crux: RequireUser accepted the Bearer token. The pre-M1 cookie-only mount
		// would have returned 401 here.
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("PRD #365 M1: %s POST %s over Bearer = 401 — the route is still cookie-only; M1 must move it to RequireUser\nbody: %s",
				tc.name, tc.path, rec.Body.String())
			continue
		}
		// And the exact deterministic code for a foreign/nonexistent coordinate: an
		// owner-scoped clean 404, no existence oracle, proving owner-scoping over Bearer.
		if rec.Code != tc.want {
			t.Errorf("PRD #365 M1: %s POST %s over Bearer = %d, want %d (owner-scoped clean not-found, no forge call)\nbody: %s",
				tc.name, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// TestCLIFilingRoutesStillEnforceCSRFLiveDB is the browser-path counterpart: the mount
// move (cookie-only RequireAuth → RequireUser) must NOT open a CSRF hole. RequireUser's
// presence-dispatch takes the cookie path when no Authorization header is present, and
// that path still enforces CSRF on writes — so a valid session cookie POST WITHOUT the
// CSRF header is 403 for all three moved routes. The 403 fires at the CSRF check before
// the handler, so random UUIDs suffice and no seeding is needed. Mirrors the CSRF-write
// assertion at the tail of TestCLICSRFBypassShapeLiveDB.
func TestCLIFilingRoutesStillEnforceCSRFLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)

	runID := uuid.New()
	recID := uuid.New()
	findingID := uuid.New()

	paths := []struct{ name, path string }{
		{"findings dismiss", "/api/findings/" + findingID.String() + "/dismiss"},
		{"findings file", "/api/findings/" + findingID.String() + "/issue"},
		{"review file", "/api/runs/" + runID.String() + "/review/recommendations/" + recID.String() + "/issue"},
	}
	for _, tc := range paths {
		// A valid auth cookie, NO CSRF header: the cookie path must reject the write at
		// the CSRF check (403), proving RequireUser preserves CSRF for the moved routes.
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("PRD #365 M1: %s cookie POST %s without CSRF = %d, want 403 (RequireUser must preserve CSRF on the browser path; the mount move must not open a CSRF hole)\nbody: %s",
				tc.name, tc.path, rec.Code, rec.Body.String())
		}
	}
}
