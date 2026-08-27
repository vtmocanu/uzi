package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeUserDB is a store.DBTX that answers SetUserJudgeEnabled from memory, capturing
// the id + flag the handler passed so a test can assert WHICH id the toggle acted on
// (session vs path vs body). It echoes the captured values back as the RETURNING row.
type fakeUserDB struct {
	gotID    uuid.UUID
	gotJudge bool
	called   bool
}

func (f *fakeUserDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeUserDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f *fakeUserDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "UPDATE users SET judge_enabled") {
		f.called = true
		if id, ok := args[0].(uuid.UUID); ok {
			f.gotID = id
		}
		if b, ok := args[1].(bool); ok {
			f.gotJudge = b
		}
		return fakeScanRow{scanJudgeUserRow(f.gotID, f.gotJudge)}
	}
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

// scanJudgeUserRow fills only the id + judge_enabled columns of the RETURNING row
// (the 1st and 17th); the rest keep their zero values, which is all the toggle
// response needs.
func scanJudgeUserRow(id uuid.UUID, judge bool) func(dest ...any) error {
	return func(dest ...any) error {
		if p, ok := dest[0].(*uuid.UUID); ok {
			*p = id
		}
		if len(dest) >= 17 {
			if p, ok := dest[16].(*bool); ok {
				*p = judge
			}
		}
		return nil
	}
}

type toggleUserResp struct {
	User struct {
		ID           string `json:"id"`
		JudgeEnabled bool   `json:"judge_enabled"`
	} `json:"user"`
}

// TestSetJudgeEnabledUsesSession: PUT /api/me/judge flips ONLY the session user's
// flag (audit H3) — the store toggle receives the SESSION id, and enabled is passed
// through. The body carries only {enabled}; the id is never taken from it.
func TestSetJudgeEnabledUsesSession(t *testing.T) {
	sessionID := uuid.New()
	db := &fakeUserDB{}
	h := &Handler{q: store.New(db)}

	req := httptest.NewRequest(http.MethodPut, "/api/me/judge", strings.NewReader(`{"enabled": true}`))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
	rec := httptest.NewRecorder()

	h.SetJudgeEnabled(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.called {
		t.Fatal("SetUserJudgeEnabled was not called")
	}
	if db.gotID != sessionID {
		t.Errorf("toggle acted on id %v, want the SESSION id %v", db.gotID, sessionID)
	}
	if !db.gotJudge {
		t.Errorf("enabled flag not passed through")
	}
	var out toggleUserResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.User.ID != sessionID.String() {
		t.Errorf("response user id = %s, want session %s", out.User.ID, sessionID)
	}
}

// TestJudgeToggleRejectsSmuggledID: a body carrying an id field cannot smuggle a
// target — DecodeJSON is strict (DisallowUnknownFields), so an unexpected id is a 400
// and the toggle never runs. This holds for BOTH the self and admin endpoints, making
// "body-supplied ids ignored" (audit H3) enforced at the decode boundary, not just by
// the struct shape.
func TestJudgeToggleRejectsSmuggledID(t *testing.T) {
	smuggle := `{"enabled": true, "id": "` + uuid.New().String() + `"}`

	t.Run("self endpoint", func(t *testing.T) {
		db := &fakeUserDB{}
		h := &Handler{q: store.New(db)}
		req := httptest.NewRequest(http.MethodPut, "/api/me/judge", strings.NewReader(smuggle))
		req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New(), IsActive: true}))
		rec := httptest.NewRecorder()
		h.SetJudgeEnabled(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (unknown field rejected); body=%s", rec.Code, rec.Body.String())
		}
		if db.called {
			t.Fatal("the toggle must not run when the body is rejected")
		}
	})

	t.Run("admin endpoint", func(t *testing.T) {
		db := &fakeUserDB{}
		h := &Handler{q: store.New(db)}
		pathID := uuid.New()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+pathID.String()+"/judge", strings.NewReader(smuggle))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", pathID.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.SetUserJudgeEnabled(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (unknown field rejected); body=%s", rec.Code, rec.Body.String())
		}
		if db.called {
			t.Fatal("the toggle must not run when the body is rejected")
		}
	})
}

// TestSetUserJudgeEnabledUsesPath: the admin per-user toggle takes the TARGET from
// the URL path (audit H3). With a clean {enabled} body, the store toggle receives the
// PATH id and the enabled flag.
func TestSetUserJudgeEnabledUsesPath(t *testing.T) {
	pathID := uuid.New()
	db := &fakeUserDB{}
	h := &Handler{q: store.New(db)}

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+pathID.String()+"/judge", strings.NewReader(`{"enabled": false}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", pathID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()

	h.SetUserJudgeEnabled(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.called {
		t.Fatal("SetUserJudgeEnabled was not called")
	}
	if db.gotID != pathID {
		t.Errorf("toggle acted on id %v, want the PATH id %v", db.gotID, pathID)
	}
	if db.gotJudge {
		t.Errorf("enabled=false from the body should have been passed through")
	}
}

// TestSetUserJudgeEnabledUnknownIDIs404: an admin toggle for a non-existent user id
// returns 404 (the UPDATE matches no row → ErrNoRows), not a masked 500.
func TestSetUserJudgeEnabledUnknownIDIs404(t *testing.T) {
	// A DB whose UPDATE users matches nothing → ErrNoRows → the handler's 404 path.
	h := &Handler{q: store.New(&noRowUserDB{})}

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+uuid.New().String()+"/judge",
		strings.NewReader(`{"enabled": true}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()

	h.SetUserJudgeEnabled(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// noRowUserDB models an UPDATE that matched no row (unknown id) → ErrNoRows.
type noRowUserDB struct{}

func (noRowUserDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (noRowUserDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (noRowUserDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}
