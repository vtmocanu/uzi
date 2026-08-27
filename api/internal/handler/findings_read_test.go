package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestListFindingsRejectsBadQueryParams pins the query-param validation of GET /api/findings
// (PRD #333 M4): an unknown bucket or an unparseable repo/run id is a 400 rather than a
// silently-ignored filter, so a typo can never look like an empty backlog. These reject BEFORE
// the service is called, so a nil-wsvc Handler is sufficient — reaching the service would
// nil-panic, which is exactly the "it validated first" property being pinned.
func TestListFindingsRejectsBadQueryParams(t *testing.T) {
	h := &Handler{}
	user := store.User{ID: uuid.New()}

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"unknown bucket", "?bucket=nonsense", http.StatusBadRequest},
		{"unparseable repo", "?repo=not-a-uuid", http.StatusBadRequest},
		{"unparseable run", "?run=not-a-uuid", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/findings/"+c.query, nil)
			req = req.WithContext(mw.ContextWithUser(req.Context(), user))
			rec := httptest.NewRecorder()
			h.ListFindings(rec, req)
			if rec.Code != c.want {
				t.Fatalf("%s = %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestListFindingsRequiresUser(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/findings/", nil) // no user in context
	rec := httptest.NewRecorder()
	h.ListFindings(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", rec.Code)
	}
}

func TestGetFindingIssueDraftRejectsBadID(t *testing.T) {
	h := &Handler{}
	user := store.User{ID: uuid.New()}
	req := httptest.NewRequest(http.MethodGet, "/api/findings/not-a-uuid/issue-draft", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	h.GetFindingIssueDraft(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad finding id = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
