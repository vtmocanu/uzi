package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/hub"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #112 M1, the half of the route-move coverage that runs in the ORDINARY
// `go test ./...` gate rather than behind the LiveDB sweep.
//
// It exists because the M1 diff is otherwise invisible to the test suite: with the
// route moved back to RequireAuth the whole of `go test ./...` stays green (measured,
// 41 packages, exit 0), since every pre-existing WS test calls h.ServeWS directly with
// an injected context user and never touches the router, and the TLS-listener test
// pins /api/ws by ROUTING — and both guards 401 an uncredentialed GET, so even the
// status is unchanged. A gate nobody runs by default is a weak place for the only
// evidence, so the two cases that need no token ROW live here.
//
// The cases that genuinely need a store — a VALID uzc_/uza_ must exist in cli_tokens
// for RequireUser to resolve it — are in ws_bearer_livedb_test.go.

var errWSGuardDB = errors.New("ws guard fake: no rows")

// wsGuardDB is a store.DBTX that answers nothing and RECORDS every statement it is
// asked to run. The recording is the point: RequireUser's two branches are told apart
// by WHICH table was read, not by the status code, because a fake that fails every
// lookup makes both branches answer 401 and a status assertion alone could not
// distinguish "took the bearer branch and refused" from "fell back to the cookie".
type wsGuardDB struct {
	mu      sync.Mutex
	queries []string
}

func (d *wsGuardDB) record(q string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, q)
}

// saw reports whether any recorded statement contained substr.
func (d *wsGuardDB) saw(substr string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, q := range d.queries {
		if strings.Contains(q, substr) {
			return true
		}
	}
	return false
}

func (d *wsGuardDB) Exec(_ context.Context, q string, _ ...interface{}) (pgconn.CommandTag, error) {
	d.record(q)
	return pgconn.CommandTag{}, errWSGuardDB
}

func (d *wsGuardDB) Query(_ context.Context, q string, _ ...interface{}) (pgx.Rows, error) {
	d.record(q)
	return nil, errWSGuardDB
}

func (d *wsGuardDB) QueryRow(_ context.Context, q string, _ ...interface{}) pgx.Row {
	d.record(q)
	return wsGuardRow{}
}

type wsGuardRow struct{}

func (wsGuardRow) Scan(...any) error { return errWSGuardDB }

// The two table fragments that identify which RequireUser branch ran. Taken from the
// generated SQL: getCLITokenByHash selects "FROM cli_tokens", getUserByID (the cookie
// branch's only lookup) selects "FROM users".
const (
	wsGuardCLITokenRead = "FROM cli_tokens"
	wsGuardUserRead     = "FROM users"
)

var wsGuardSecret = []byte("0123456789abcdef0123456789abcdef")

// wsGuardRouter builds the REAL h.Routes() router over the recording fake. wsvc is
// wired with a real run so that a guard which LEAKS lands on a live ServeWS and
// answers a status, rather than panicking on a nil service and burying the finding in
// a stack trace.
func wsGuardRouter(t *testing.T) (http.Handler, *wsGuardDB, uuid.UUID) {
	t.Helper()
	db := &wsGuardDB{}
	runID := uuid.New()
	owner := uuid.New()
	st := &runsStore{ownerID: owner, run: store.Run{ID: runID, UserID: owner, Status: "running"}}
	h := &Handler{
		q:    store.New(db),
		cfg:  config.Config{JWTSecret: wsGuardSecret, AuthTokenTTL: time.Hour},
		wsvc: workersvc.New(st, newHandlerTestBox(t), workersvc.Params{}),
		hub:  hub.New(),
	}
	noLimit := mw.NewLimiter(100000, time.Minute, nil)
	return h.Routes(noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit), db, runID
}

// -------------------------------------------------------------------------
// (b) An uncredentialed upgrade is 401, and the route is genuinely behind auth
// MIDDLEWARE rather than leaning on ServeWS's own context check.
//
// The second half is not pedantry, it is what the first half cannot see. Mutating the
// route to `r.Get("/ws", h.ServeWS)` with NO guard at all still answers 401 — ServeWS
// refuses a request with no context user, and its body is byte-identical to
// RequireAuth's missing-cookie refusal ("authentication required"). A status-only
// assertion here is therefore printable by two mechanisms, one of which is an
// unguarded route. So the discriminator is a MALFORMED cookie: only RequireAuth emits
// "invalid or expired session" (grep: one non-test site, middleware/auth.go:58), and
// ServeWS has no way to produce it.
// -------------------------------------------------------------------------

func TestWSRouteRejectsUncredentialedUpgrade(t *testing.T) {
	router, db, runID := wsGuardRouter(t)
	url := "/api/ws?run=" + runID.String()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("uncredentialed GET /api/ws = %d, want 401 (404 would mean the route is not mounted at all)\nbody: %s",
			rec.Code, rec.Body.String())
	}
	if db.saw(wsGuardCLITokenRead) || db.saw(wsGuardUserRead) {
		t.Errorf("an uncredentialed upgrade reached the database (cli_tokens=%v users=%v); it must be refused before any lookup",
			db.saw(wsGuardCLITokenRead), db.saw(wsGuardUserRead))
	}

	// A cookie that is PRESENT but not parseable gets the middleware's own diagnostic,
	// which is the proof the middleware ran at all.
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: "not-a-jwt"})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed-cookie GET /api/ws = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
	// One definition of the expected string. Written twice — once in the check, once
	// in the message — editing one silently desyncs them, and the message would then
	// name a string the check never looked for.
	const wantMiddlewareRefusal = "invalid or expired session"
	if !strings.Contains(rec.Body.String(), wantMiddlewareRefusal) {
		t.Errorf("malformed-cookie refusal body = %s, want the auth middleware's %q — anything else (notably \"authentication required\") means /ws is mounted with NO auth guard and is being saved only by ServeWS's own context check",
			strings.TrimSpace(rec.Body.String()), wantMiddlewareRefusal)
	}
}

// -------------------------------------------------------------------------
// (d) The CSRF-bypass shape, on THIS route: a request carrying a valid session cookie
// AND a bogus Authorization header must be refused on the bearer branch and must NEVER
// fall back to the cookie. "Try bearer, on failure try cookie" is the classic bypass —
// an attacker appends a junk Authorization header to skip the CSRF-checked branch.
//
// The assertion is on the QUERY TRACE, not the status: this fake fails every lookup, so
// a fallback would ALSO answer 401 and a status-only check would pass with the hole
// open. Correct behaviour reads cli_tokens and never touches users.
// -------------------------------------------------------------------------

func TestWSRouteBogusBearerDoesNotFallBackToCookie(t *testing.T) {
	router, db, runID := wsGuardRouter(t)
	jwt, err := auth.IssueToken(wsGuardSecret, uuid.NewString(), 0, time.Hour)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	url := "/api/ws?run=" + runID.String()

	// Positive control on the INSTRUMENT: the same cookie with no Authorization header
	// takes the cookie branch and is observed doing so. Without this, the "users was
	// never read" assertion below could be satisfied by a trace that records nothing.
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt})
	router.ServeHTTP(httptest.NewRecorder(), req)
	if !db.saw(wsGuardUserRead) {
		t.Fatalf("a cookie-only upgrade did not read users; the query trace cannot observe the cookie branch, so the fallback assertion below would be vacuous\nrecorded: %v", db.queries)
	}

	// The attack, on a FRESH trace so the control above cannot colour it.
	router, db, runID = wsGuardRouter(t)
	req = httptest.NewRequest(http.MethodGet, "/api/ws?run="+runID.String(), nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt})
	req.Header.Set("Authorization", "Bearer uzc_not-a-real-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie + bogus Bearer GET /api/ws = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
	if !db.saw(wsGuardCLITokenRead) {
		// FATAL, and it names the MOUNT rather than the dispatch predicate, because
		// this is the discriminator between two very different faults. If the route is
		// reverted to RequireAuth there is no bearer branch on it at all, so BOTH this
		// check and the users-read check below would fire — and the one below alleges a
		// live CSRF bypass in cli_auth.go, which is untouched and correct in that case.
		// Sending a reader to audit working auth code is the most expensive wrong
		// answer available here, so stopping at this one keeps the alarming claim below
		// printable only when it is true.
		t.Fatalf("a request with an Authorization header never read cli_tokens, so NO bearer branch ran at all.\n"+
			"    Check handler.go's MOUNT for /ws (is it RequireUser, or has it reverted to RequireAuth?) — NOT cli_auth.go's dispatch, which this does not implicate.\n"+
			"    recorded: %v", db.queries)
	}
	if db.saw(wsGuardUserRead) {
		t.Errorf("a request whose Bearer token failed went on to read users — RequireUser fell back to the COOKIE branch, which is the CSRF-bypass shape (append a junk Authorization header to skip the CSRF-checked path)\nrecorded: %v", db.queries)
	}
}
