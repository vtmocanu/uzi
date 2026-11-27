package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestListProjectsIssueTrackerLinkableLiveDB drives ListProjects, the handler the Repos
// page renders from: a never-enabled, unlinked repo carries issue_tracker_linkable only
// when an issue tracker is configured for the user (PRD #4173 M6 follow-up, issue #27).
func TestListProjectsIssueTrackerLinkableLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	f.h.settings = settings.New(store.New(f.pool), time.Minute)
	f.forge.mu.Lock()
	f.forge.emptyProjects = true
	f.forge.mu.Unlock()
	// awaiting_first_enable is never set back once cleared, so the row is born awaiting.
	repoID := uuid.New()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled, awaiting_first_enable)
		 VALUES ($1, $2, 201, 'g/lp-201', 'https://forge.example/g/lp', 'main', false, true)`,
		repoID, f.connID)

	list := func() bool {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/connections/x/projects", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", f.connID.String())
		r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), f.owner), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		f.h.ListProjects(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		var body struct {
			Repos []struct {
				ID                   string `json:"id"`
				IssueTrackerLinkable bool   `json:"issue_tracker_linkable"`
			} `json:"repos"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Repos) != 1 || body.Repos[0].ID != repoID.String() {
			t.Fatalf("repos = %+v, want only %s", body.Repos, repoID)
		}
		return body.Repos[0].IssueTrackerLinkable
	}

	if list() {
		t.Error("linkable with nothing configured, want false")
	}
	f.h.cfg.IssueTrackerAllowedBaseURLs = []string{"https://issues.example.com"}
	if !list() {
		t.Error("not linkable with an allowlist configured, want true")
	}
}
