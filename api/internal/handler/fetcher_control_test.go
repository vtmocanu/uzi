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
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// PRD #1906 M3, the DB-free half of the fetcher control routes (the LiveDB file covers the
// rest): mounted only with a configured service token, service Bearer required, bodies
// decoded strictly before anything else.
func fetcherRouters(token string) (plain, tls http.Handler) {
	cfg := config.Config{}
	if token != "" {
		sum := sha256.Sum256([]byte(token))
		cfg.FetcherTokenSHA256 = sum[:]
	}
	h := &Handler{cfg: cfg}
	lim := mw.NewLimiter(1000, time.Minute, nil)
	return h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim), h.WorkerRoutes(lim)
}

func fetcherPost(r http.Handler, path, bearer, body string) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

func TestFetcherRoutesUnmountedWithoutToken(t *testing.T) {
	plain, tls := fetcherRouters("")
	for _, p := range []string{fetchctl.BeginPath, fetchctl.CompletePath} {
		for name, r := range map[string]http.Handler{"plain": plain, "tls": tls} {
			if code := fetcherPost(r, p, "Bearer anything", `{}`); code != http.StatusNotFound {
				t.Errorf("%s %s with no token configured = %d, want 404", name, p, code)
			}
		}
	}
}

func TestFetcherRoutesServiceAuthAndStrictDecode(t *testing.T) {
	tok := "svc-" + uuid.NewString()
	plain, tls := fetcherRouters(tok)
	for _, p := range []string{fetchctl.BeginPath, fetchctl.CompletePath} {
		for name, bearer := range map[string]string{"missing": "", "empty": "Bearer ", "blank": "Bearer    ", "basic": "Basic " + tok, "wrong": "Bearer " + tok + "x"} {
			if code := fetcherPost(plain, p, bearer, `{}`); code != http.StatusUnauthorized {
				t.Errorf("%s bearer on %s = %d, want 401", name, p, code)
			}
		}
		for _, body := range []string{`{"credential":"c","run_id":"r"}`, `{"credential":"c"} {"x":1}`, `not json`} {
			for name, r := range map[string]http.Handler{"plain": plain, "tls": tls} {
				if code := fetcherPost(r, p, "Bearer "+tok, body); code != http.StatusBadRequest {
					t.Errorf("%s %s body %q = %d, want 400", name, p, body, code)
				}
			}
		}
	}
	// A well-formed body on a handler with no database reaches the handler and fails closed.
	if code := fetcherPost(plain, fetchctl.BeginPath, "Bearer "+tok, `{"credential":"c","url":"https://x.example/"}`); code != http.StatusServiceUnavailable {
		t.Errorf("begin with no database = %d, want 503", code)
	}
}

// The DB-free half of the credential separation (TestFetcherCredentialIsolationLiveDB adds
// the worker surface and positive controls with real credentials): with hosting and the
// fetcher both configured, the fetcher token is refused on /api/controller/*, and the
// controller token, a uzw_-shaped Bearer and a session cookie are refused on
// /api/fetcher/v1/*, on both listeners.
func TestFetcherAndControllerTokensAreNotInterchangeable(t *testing.T) {
	fetchTok := "fetcher-svc-" + uuid.NewString()
	ctrlTok := "controller-svc-" + uuid.NewString()
	fs, cs := sha256.Sum256([]byte(fetchTok)), sha256.Sum256([]byte(ctrlTok))
	h := &Handler{cfg: config.Config{WorkerHostingEnabled: true, ControllerTokenSHA256: cs[:], FetcherTokenSHA256: fs[:]}}
	lim := mw.NewLimiter(1000, time.Minute, nil)
	for name, r := range map[string]http.Handler{"plain": h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim), "tls": h.WorkerRoutes(lim)} {
		for _, p := range []string{"/api/controller/status", "/api/controller/workers/" + uuid.NewString() + "/drain"} {
			if code := fetcherPost(r, p, "Bearer "+fetchTok, `{}`); code != http.StatusUnauthorized {
				t.Errorf("%s: fetcher token on %s = %d, want 401", name, p, code)
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil)
		req.Header.Set("Authorization", "Bearer "+fetchTok)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: fetcher token on controller poll = %d, want 401", name, rec.Code)
		}
		for _, p := range []string{fetchctl.BeginPath, fetchctl.CompletePath} {
			for who, bearer := range map[string]string{"controller": "Bearer " + ctrlTok, "uzw_": "Bearer uzw_" + uuid.NewString()} {
				if code := fetcherPost(r, p, bearer, `{"credential":"c","url":"https://x.example/"}`); code != http.StatusUnauthorized {
					t.Errorf("%s: %s token on %s = %d, want 401", name, who, p, code)
				}
			}
			req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"credential":"c","url":"https://x.example/"}`))
			req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: "a.b.c"}) //nolint:gosec // G124: test-only client cookie on an httptest request.
			req.Header.Set(auth.CSRFHeaderName, "x")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: session cookie on %s = %d, want 401", name, p, rec.Code)
			}
		}
	}
}
