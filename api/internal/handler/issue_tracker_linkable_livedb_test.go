package handler

// Live-DB test of the issue_tracker_linkable flag on the repo response (PRD #4173 M6
// follow-up, issue #27): the link control is offered only where a link can succeed, so
// the flag follows the operator's allowlist or the user's own connection, not just the
// repo's never-enabled state. Driven through the real router (PATCH /api/repos/{id}).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRepoIssueTrackerLinkableLiveDB(t *testing.T) {
	f := newITFixture(t, true)
	patch := func(repo uuid.UUID) (linkable bool, linked bool) {
		t.Helper()
		rec := f.do(f.jwt, http.MethodPatch, "/api/repos/"+repo.String(), `{"repo_skills_enabled":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch = %d\n%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Repo struct {
				Linkable bool            `json:"issue_tracker_linkable"`
				Link     json.RawMessage `json:"issue_tracker_link"`
			} `json:"repo"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Repo.Linkable, len(out.Repo.Link) > 0 && string(out.Repo.Link) != "null"
	}

	t.Run("nothing configured: no allowlist and no connection", func(t *testing.T) {
		f.h.cfg.IssueTrackerAllowedBaseURLs = nil
		if linkable, _ := patch(f.freshRepo()); linkable {
			t.Error("a never-enabled repo was linkable with nothing configured")
		}
	})
	t.Run("an allowlist makes a never-enabled repo linkable", func(t *testing.T) {
		f.h.cfg.IssueTrackerAllowedBaseURLs = []string{itTestHost}
		if linkable, _ := patch(f.freshRepo()); !linkable {
			t.Error("a never-enabled repo was not linkable with an allowlist")
		}
	})
	t.Run("an enabled repo is not linkable", func(t *testing.T) {
		f.h.cfg.IssueTrackerAllowedBaseURLs = []string{itTestHost}
		repo := f.freshRepo()
		if _, err := f.q.SetRepoEnabledForUser(context.Background(), store.SetRepoEnabledForUserParams{ID: repo, Enabled: true, UserID: f.user}); err != nil {
			t.Fatal(err)
		}
		if linkable, _ := patch(repo); linkable {
			t.Error("an enabled repo was linkable")
		}
	})
	t.Run("a connection alone makes it linkable, a link makes it not", func(t *testing.T) {
		f.h.cfg.IssueTrackerAllowedBaseURLs = nil
		_, _, conn := f.connect(f.jwt, itToken("a"))
		// connect is refused with an empty allowlist in the real service; the fixture's
		// service allows itTestHost, and the handler config is a separate read.
		if conn == uuid.Nil {
			t.Fatal("no connection created")
		}
		repo := f.freshRepo()
		if linkable, _ := patch(repo); !linkable {
			t.Error("a never-enabled repo was not linkable with a connection")
		}
		rec := f.do(f.jwt, http.MethodPut, "/api/repos/"+repo.String()+"/issue-tracker-link", linkBody(conn, "5-1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("link = %d\n%s", rec.Code, rec.Body.String())
		}
		linkable, linked := patch(repo)
		if linkable || !linked {
			t.Errorf("linked repo: linkable=%v linked=%v, want false/true", linkable, linked)
		}
	})
}
