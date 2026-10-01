package handler

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/fetchctl"
	"github.com/vtmocanu/uzi/api/internal/hub"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestFetcherCredentialIsolationLiveDB pins PRD #1906 M3's credential separation through
// the REAL routers, with every credential genuinely valid for its own surface (each has a
// positive control on its own route, so a refusal below is the boundary, not a broken
// credential):
//   - the fetcher's service token is refused on /api/controller/* and /api/worker/*;
//   - a user's session cookie (with its CSRF header), a registered worker's uzw_ join token
//     and the controller's service token are refused on /api/fetcher/v1/*.
func TestFetcherCredentialIsolationLiveDB(t *testing.T) {
	e := newFCEnv(t, nil, true)
	ctrlTok := "controller-svc-" + uuid.NewString()
	ctrlSum := sha256.Sum256([]byte(ctrlTok))
	fetchSum := sha256.Sum256([]byte(e.svcTok))
	cfg := config.Config{
		JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour,
		WorkerHostingEnabled: true, ControllerTokenSHA256: ctrlSum[:], FetcherTokenSHA256: fetchSum[:],
	}
	q := store.New(e.pool)
	box := newHandlerTestBox(t)
	h := &Handler{pool: e.pool, q: q, box: box, cfg: cfg, wsvc: workersvc.New(q, box, workersvc.Params{}), settings: e.caps, hub: hub.New()}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	routers := map[string]http.Handler{
		"plain": h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim),
		"tls":   h.WorkerRoutes(lim),
	}

	// A registered worker owned by the run's owner, with a real uzw_ join token.
	wkrTok, wkrHash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'fc-iso-worker', $3, 'online')`,
		uuid.New(), e.userID, wkrHash)
	jwt := cliMintJWT(t, e.pool, e.userID)
	run, cred := e.boundRun("running", 1, 1)

	do := func(r http.Handler, method, path, bearer, body string, withCookie bool) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if withCookie {
			req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
			req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	for name, r := range routers {
		// Positive controls: each credential works on its own surface.
		if code := do(r, http.MethodGet, "/api/controller/poll", ctrlTok, "", false); code == http.StatusUnauthorized || code == http.StatusNotFound {
			t.Fatalf("%s: controller token on its own poll = %d; the control is broken", name, code)
		}
		if code := do(r, http.MethodPost, "/api/worker/heartbeat", wkrTok, `{}`, false); code == http.StatusUnauthorized || code == http.StatusNotFound {
			t.Fatalf("%s: worker token on its own heartbeat = %d; the control is broken", name, code)
		}

		// The fetcher token on the controller and worker surfaces.
		for _, rt := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/controller/poll", ""},
			{http.MethodPost, "/api/controller/status", `{}`},
			{http.MethodPost, "/api/worker/heartbeat", `{}`},
			{http.MethodPost, "/api/worker/runs/claim", `{}`},
			{http.MethodGet, "/api/worker/runs/" + run.String() + "/inputs", ""},
		} {
			if code := do(r, rt.method, rt.path, e.svcTok, rt.body, false); code != http.StatusUnauthorized {
				t.Errorf("%s: fetcher token on %s %s = %d, want 401", name, rt.method, rt.path, code)
			}
		}

		// Other credentials on the fetcher surface.
		for _, p := range []string{fetchctl.BeginPath, fetchctl.CompletePath} {
			for who, c := range map[string]struct {
				bearer string
				cookie bool
			}{
				"session cookie":   {"", true},
				"worker uzw_":      {wkrTok, false},
				"controller token": {ctrlTok, false},
			} {
				if code := do(r, http.MethodPost, p, c.bearer, beginBody(cred), c.cookie); code != http.StatusUnauthorized {
					t.Errorf("%s: %s on %s = %d, want 401", name, who, p, code)
				}
			}
		}
	}
	if c := e.counters(run); c.attempts != 0 || c.inflight != 0 {
		t.Fatalf("a refused credential still touched the run: %+v", c)
	}
	// Positive control for the fetcher: its own token is admitted on its own route.
	if code := do(routers["plain"], http.MethodPost, fetchctl.BeginPath, e.svcTok, beginBody(cred), false); code != http.StatusOK {
		t.Fatalf("fetcher token on its own begin = %d, want 200", code)
	}
}
