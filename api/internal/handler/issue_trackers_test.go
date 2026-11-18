package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// issueTrackerRoutes is every issue tracker route of PRD #4173 M4, with a method and a path
// that resolve through the real router.
var issueTrackerRoutes = []struct{ method, path, body string }{
	{http.MethodPost, "/api/issue-trackers/connections", `{"kind":"youtrack","base_url":"https://x.example.com","token":"t"}`},
	{http.MethodGet, "/api/issue-trackers/connections", ""},
	{http.MethodDelete, "/api/issue-trackers/connections/11111111-1111-1111-1111-111111111111", ""},
	{http.MethodGet, "/api/issue-trackers/connections/11111111-1111-1111-1111-111111111111/projects", ""},
	{http.MethodGet, "/api/issue-trackers/connections/11111111-1111-1111-1111-111111111111/tags", ""},
	{http.MethodPut, "/api/repos/22222222-2222-2222-2222-222222222222/issue-tracker-link", `{"connection_id":"11111111-1111-1111-1111-111111111111","project_id":"0-1","tag_id":"5-1"}`},
}

func issueTrackerTestRouter() http.Handler {
	h := &Handler{cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	return h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
}

// Every issue tracker route is session-only: no cookie is a 401 whatever the method.
func TestIssueTrackerRoutesRefuseARequestWithoutASession(t *testing.T) {
	router := issueTrackerTestRouter()
	for _, rt := range issueTrackerRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401\n%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A session cookie without the CSRF header is refused on every state-changing route, and a
// Bearer token does not stand in for the cookie.
func TestIssueTrackerRoutesRefuseAStateChangeWithoutCSRF(t *testing.T) {
	router := issueTrackerTestRouter()
	for _, rt := range issueTrackerRoutes {
		if rt.method == http.MethodGet {
			continue
		}
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: "any-session-value"}) //nolint:gosec // G124: test-only client cookie on an httptest request.
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403\n%s", rec.Code, rec.Body.String())
			}

			bearer := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			bearer.Header.Set("Authorization", "Bearer uzc_whatever")
			bearer.Header.Set("Content-Type", "application/json")
			rec = httptest.NewRecorder()
			router.ServeHTTP(rec, bearer)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("bearer status = %d, want 401\n%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A handler with no issue tracker service answers an authenticated request with 503, and an
// unauthenticated one with 401, rather than panicking.
func TestIssueTrackerRoutesAnswer503WithoutAService(t *testing.T) {
	h := &Handler{}
	handlers := map[string]http.HandlerFunc{
		"create": h.CreateIssueTrackerConnection, "list": h.ListIssueTrackerConnections,
		"delete": h.DeleteIssueTrackerConnection, "projects": h.ListIssueTrackerProjects,
		"tags": h.ListIssueTrackerTags, "link": h.LinkRepoToIssueTracker,
	}
	for name, fn := range handlers {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			fn(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("no user: status = %d, want 401", rec.Code)
			}
			req = req.WithContext(mw.ContextWithUser(context.Background(), store.User{}))
			rec = httptest.NewRecorder()
			fn(rec, req)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "issue trackers are not configured") {
				t.Fatalf("status = %d %s, want 503 naming the missing configuration", rec.Code, rec.Body.String())
			}
		})
	}
}
