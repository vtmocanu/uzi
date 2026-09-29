package handler

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
