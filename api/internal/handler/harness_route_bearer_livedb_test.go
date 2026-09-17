package handler

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1429 M5: POST /api/repos/{id}/runs and POST /api/repos/{id}/schedules are already
// under RequireUser (routes_repos.go), and M5 threads a new `harness` request field through
// both. This is the router-level differential-auth proof: driven through the REAL h.Routes()
// mounted router (not a direct handler call, which bypasses the middleware chain entirely and
// cannot catch a mis-mount — issue #428's regression class), a request carrying `harness`
// reaches the handler over a uzc_ CLI Bearer credential exactly as it does over the cookie
// path, and is refused with no credential at all. Mirrors
// TestCLITaskRunsBearerReachableLiveDB (cli_auth_livedb_test.go) for POST .../task-runs.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// harnessRouteFixture wires a Handler capable of a REAL issue-lane run create (StartRunForUser
// needs a non-nil forge builder — ErrForgesUnavailable otherwise) without the #66 guardrail
// wiring TestStartRunGuard*LiveDB needs: SetRepoGuard is deliberately left uncalled, so
// guardDefaultBranch is a no-op (service.go's own documented nil-guard behaviour) and the fake
// forge below only has to answer the GetIssue call, not the protected-branches/members probes.
type harnessRouteFixture struct {
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool
	owner  uuid.UUID
}

// harnessRouteFakeForge serves only the one forge call this fixture's create path makes:
// StartRunForUser's GetIssue snapshot. Any other path 404s — nothing else should be called
// with SetRepoGuard left unwired.
func harnessRouteFakeForge(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues/") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 9101, "iid": 7, "title": "harness route issue", "description": "do the thing",
				"state": "opened", "labels": []string{"uzi"}, "web_url": "https://x",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newHarnessRouteFixture(ctx context.Context, t *testing.T) harnessRouteFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
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
	svc := forgesvc.New(q, box, 5*time.Second, nil)
	wsvc := workersvc.New(q, box, workersvc.Params{})
	wsvc.SetForges(svc) // required: StartRunForUser 500s ErrForgesUnavailable without it
	// SetRepoGuard / SetTxBeginner deliberately NOT called: the guard is a documented no-op
	// when nil, and this fixture never resolves a Codex harness (only claude, below), so the
	// atomic-transaction seam PRD #1429 M1 added for a Codex freeze is never reached.

	owner := uuid.New()
	h := &Handler{
		pool: pool, q: q, box: box,
		cfg:  config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour},
		svc:  svc,
		wsvc: wsvc,
	}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim)

	cliMustExec(t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("harness-route-%s@e2e", owner))
	// An Anthropic token so an implicit-harness (or explicit --harness claude) create
	// actually reaches 201 rather than 422 no_usable_credential — orthogonal to the
	// Bearer/cookie reachability under test, exactly like startRunGuardFixture's own token.
	cliMustExec(t, pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		 VALUES ($1, $2, 'anthropic_token', 'anthropic-default', true, $3, 'master')`,
		uuid.New(), owner, []byte("ct"))

	return harnessRouteFixture{h: h, router: router, pool: pool, owner: owner}
}

// seedConnection inserts ONE forge_connections row for the fixture's owner pointed at
// forgeURL and returns its id. Called once per test and reused across every
// seedRunnableIssue call: the (user_id, forge_type, base_url) unique index means a second
// insert with the same forgeURL for the same owner collides (23505).
func (f harnessRouteFixture) seedConnection(ctx context.Context, t *testing.T, forgeURL string) uuid.UUID {
	t.Helper()
	connID := uuid.New()
	cliMustExec(t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'uzi-bot', 1, $4)`,
		connID, f.owner, forgeURL, mustSealT(ctx, t, f.h.box, []byte("glpat-dummy")))
	return connID
}

// seedRunnableIssue inserts an enabled repo under connID + a cached, uzi-labelled issue (the
// PRD #764/#767 eligibility gate) so an issue-lane create can actually reach 201, and points
// the fake forge's project id at the repo. Returns the repo id.
func (f harnessRouteFixture) seedRunnableIssue(ctx context.Context, t *testing.T, connID uuid.UUID, forgeURL string, projectID, iid int64) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	cliMustExec(t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, $5, 'main', true)`,
		repoID, connID, projectID, fmt.Sprintf("g/hr-%d", projectID), forgeURL+"/g/hr")
	cliMustExec(t, f.pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, $2, 'harness route issue', 'opened', '["uzi"]'::jsonb, 'https://x', false, now(), now())`,
		repoID, iid)
	return repoID
}

// mustSealT seals plaintext for a fixture's forge_connections.token_ciphertext column.
func mustSealT(_ context.Context, t *testing.T, box interface{ Seal([]byte) ([]byte, error) }, plain []byte) []byte {
	t.Helper()
	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return sealed
}

// TestCreateRunHarnessFieldBearerReachableLiveDB proves POST /api/repos/{id}/runs — carrying
// the M5 `harness` field — is reachable over a uzc_ CLI Bearer (RequireUser), refused with no
// credential, and unchanged over the cookie/CSRF browser path.
func TestCreateRunHarnessFieldBearerReachableLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newHarnessRouteFixture(ctx, t)
	forge := harnessRouteFakeForge(t)
	connID := f.seedConnection(ctx, t, forge.URL)
	repoID := f.seedRunnableIssue(ctx, t, connID, forge.URL, 7301, 7)
	uzc := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)

	path := "/api/repos/" + repoID.String() + "/runs"
	const body = `{"issue_iid":7,"harness":"claude"}`

	// 1. A valid uzc_ Bearer, no cookie, reaches the owner-scoped handler and the harness
	// field is accepted ⇒ 201.
	if rec := bearerReqBody(f.router, http.MethodPost, path, uzc, body); rec.Code != http.StatusCreated {
		t.Errorf("valid uzc_ POST %s (with harness) = %d, want 201 (RequireUser must accept the CLI Bearer path)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// 2. No credential at all is refused ⇒ 401, proving the 201 above is the credential
	// being honoured, not an unauthenticated route.
	if rec := bearerReqBody(f.router, http.MethodPost, path, "", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-credential POST %s = %d, want 401 (run create is authenticated)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// 3. The browser path is unchanged: a valid session cookie + CSRF header reaches the
	// same handler with the same harness field ⇒ 201 (a second issue so the dedup gate
	// does not collide with check 1's run).
	repoID2 := f.seedRunnableIssue(ctx, t, connID, forge.URL, 7302, 8)
	path2 := "/api/repos/" + repoID2.String() + "/runs"
	const body2 = `{"issue_iid":8,"harness":"claude"}`
	if rec := cookieReq(t, f.router, http.MethodPost, path2, cliMintJWT(t, f.pool, f.owner), body2); rec.Code != http.StatusCreated {
		t.Errorf("valid cookie+CSRF POST %s (with harness) = %d, want 201 (browser path unchanged)\nbody: %s", path2, rec.Code, rec.Body.String())
	}

	// 4. Presence-dispatch is preserved: a valid session cookie plus a BOGUS Authorization
	// header takes the Bearer path (bad token → 401) rather than silently falling back to
	// the cookie — mirrors TestCLITaskRunsBearerReachableLiveDB's identical assertion.
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: cliMintJWT(t, f.pool, f.owner)}) //nolint:gosec // G124: test-only client cookie on an httptest request; Secure/HttpOnly/SameSite are response-side attributes irrelevant to a cookie a unit test sends.
	req.Header.Set("Authorization", "Bearer uzc_not-a-real-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie + bogus Bearer POST %s = %d, want 401 (presence-dispatch must not fall back to cookie)\nbody: %s", path, rec.Code, rec.Body.String())
	}
}

// TestCreateScheduleHarnessFieldBearerReachableLiveDB proves POST /api/repos/{id}/schedules —
// carrying the M5 `harness` field (a prompt-target schedule, no forge/wsvc round-trip needed:
// resolveScheduleHarness validates the enum client-independently and the credential-override
// seam that DOES call into wsvc is skipped whenever credential_override itself is absent) — is
// reachable over a uzc_ CLI Bearer, refused with no credential, and unchanged over cookie/CSRF.
func TestCreateScheduleHarnessFieldBearerReachableLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newHarnessRouteFixture(ctx, t)
	// A bare owned repo suffices — CreateSchedule needs no cached issue or forge call for a
	// prompt target.
	connID, repoID := uuid.New(), uuid.New()
	cliMustExec(t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`,
		connID, f.owner, mustSealT(ctx, t, f.h.box, []byte("glpat-dummy")))
	cliMustExec(t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/hr-sched', 'https://forge.e2e/g/hr-sched', 'main', true)`,
		repoID, connID)
	uzc := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)

	path := "/api/repos/" + repoID.String() + "/schedules"
	const body = `{"target":"prompt","prompt":"weekly task","timing":"recurring","cron_expr":"0 9 * * 1","timezone":"UTC","harness":"codex"}`

	// 1. A valid uzc_ Bearer, no cookie, reaches the owner-scoped handler and the harness
	// pin is accepted ⇒ 201.
	if rec := bearerReqBody(f.router, http.MethodPost, path, uzc, body); rec.Code != http.StatusCreated {
		t.Errorf("valid uzc_ POST %s (with harness) = %d, want 201 (RequireUser must accept the CLI Bearer path)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// 2. No credential at all is refused ⇒ 401.
	if rec := bearerReqBody(f.router, http.MethodPost, path, "", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("no-credential POST %s = %d, want 401 (schedule create is authenticated)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// 3. The browser path is unchanged: a valid session cookie + CSRF header reaches the
	// same handler with the same harness field ⇒ 201.
	if rec := cookieReq(t, f.router, http.MethodPost, path, cliMintJWT(t, f.pool, f.owner), body); rec.Code != http.StatusCreated {
		t.Errorf("valid cookie+CSRF POST %s (with harness) = %d, want 201 (browser path unchanged)\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// 4. Presence-dispatch: a valid session cookie plus a BOGUS Authorization header takes
	// the Bearer path (bad token → 401), not a silent fallback to the cookie.
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: cliMintJWT(t, f.pool, f.owner)}) //nolint:gosec // G124: test-only client cookie on an httptest request; Secure/HttpOnly/SameSite are response-side attributes irrelevant to a cookie a unit test sends.
	req.Header.Set("Authorization", "Bearer uzc_not-a-real-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie + bogus Bearer POST %s = %d, want 401 (presence-dispatch must not fall back to cookie)\nbody: %s", path, rec.Code, rec.Body.String())
	}
}
