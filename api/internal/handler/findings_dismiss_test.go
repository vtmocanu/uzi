package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These pin the PRE-DB validation of the PRD #1183 M3 findings endpoints: each rejects before the
// service/queries are reached, so a nil-q/nil-wsvc Handler is sufficient — reaching the store would
// nil-panic, which is exactly the "it validated first" property being pinned. The DB-backed
// behaviours (counts, owner scoping, reopen) live in findings_dismiss_livedb_test.go.

func findingsBodyReq(method string, user *store.User, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/x", nil)
	} else {
		r = httptest.NewRequest(method, "/x", strings.NewReader(body))
	}
	if user != nil {
		r = r.WithContext(mw.ContextWithUser(r.Context(), *user))
	}
	return r
}

// findingsPathReq builds a request with a chi {id} route param (and optionally a user), for the
// {id}-keyed undo route.
func findingsPathReq(method string, user *store.User, id string) *http.Request {
	r := httptest.NewRequest(method, "/x", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	if user != nil {
		ctx = mw.ContextWithUser(ctx, *user)
	}
	return r.WithContext(ctx)
}

func TestFindingsStatsValidation(t *testing.T) {
	h := &Handler{}
	user := store.User{ID: uuid.New()}

	// No user → 401 (before wsvc).
	rec := httptest.NewRecorder()
	h.FindingsStats(rec, findingsBodyReq(http.MethodGet, nil, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", rec.Code)
	}

	// Unparseable ?repo= → 400 (before wsvc).
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/findings/stats?repo=not-a-uuid", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), user))
	h.FindingsStats(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBulkDismissFindingsValidation(t *testing.T) {
	h := &Handler{}
	user := store.User{ID: uuid.New()}

	// No user → 401 (before the query).
	rec := httptest.NewRecorder()
	h.BulkDismissFindings(rec, findingsBodyReq(http.MethodPost, nil, `{"ids":[],"reason":"wont_do"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", rec.Code)
	}

	// Invalid reason → 400 (before the cap check and the query).
	rec = httptest.NewRecorder()
	h.BulkDismissFindings(rec, findingsBodyReq(http.MethodPost, &user, `{"ids":[],"reason":"meh"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid reason = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// > 100 ids → 400, and the message names the cap (before the query). The reason is valid so the
	// cap is what rejects.
	ids := make([]string, 0, 101)
	for i := 0; i < 101; i++ {
		ids = append(ids, `"`+uuid.NewString()+`"`)
	}
	rec = httptest.NewRecorder()
	h.BulkDismissFindings(rec, findingsBodyReq(http.MethodPost, &user,
		fmt.Sprintf(`{"reason":"wont_do","ids":[%s]}`, strings.Join(ids, ","))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("101 ids = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too many ids") {
		t.Errorf("cap message = %q, want it to name the cap", rec.Body.String())
	}

	// An unparseable id inside a within-cap list is still a 400 (before the query).
	rec = httptest.NewRecorder()
	h.BulkDismissFindings(rec, findingsBodyReq(http.MethodPost, &user, `{"reason":"wont_do","ids":["not-a-uuid"]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUndoDismissFindingValidation(t *testing.T) {
	h := &Handler{}
	user := store.User{ID: uuid.New()}

	// No user → 401 (before the query).
	rec := httptest.NewRecorder()
	h.UndoDismissFinding(rec, findingsPathReq(http.MethodDelete, nil, uuid.NewString()))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", rec.Code)
	}

	// Unparseable path id → 400 (before the query).
	rec = httptest.NewRecorder()
	h.UndoDismissFinding(rec, findingsPathReq(http.MethodDelete, &user, "not-a-uuid"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad path id = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
