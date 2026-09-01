package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeAttributionUserDB is a store.DBTX that answers SetUserAttributionEnabled from
// memory, capturing the id + flag the handler passed so a test can assert WHICH id the
// toggle acted on (session vs body). It echoes the captured values back as the
// RETURNING row.
type fakeAttributionUserDB struct {
	gotID          uuid.UUID
	gotAttribution bool
	called         bool
}

func (f *fakeAttributionUserDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeAttributionUserDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f *fakeAttributionUserDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "UPDATE users SET attribution_enabled") {
		f.called = true
		if id, ok := args[0].(uuid.UUID); ok {
			f.gotID = id
		}
		if b, ok := args[1].(bool); ok {
			f.gotAttribution = b
		}
		return fakeScanRow{scanAttributionUserRow(f.gotID, f.gotAttribution)}
	}
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

// scanAttributionUserRow fills only the id + attribution_enabled columns of the
// RETURNING row (the 1st and the 29th/last — see setUserAttributionEnabled in
// users.sql.go); the rest keep their zero values, which is all the toggle response
// needs.
func scanAttributionUserRow(id uuid.UUID, attribution bool) func(dest ...any) error {
	return func(dest ...any) error {
		if p, ok := dest[0].(*uuid.UUID); ok {
			*p = id
		}
		if len(dest) >= 29 {
			if p, ok := dest[28].(*bool); ok {
				*p = attribution
			}
		}
		return nil
	}
}

type attributionToggleUserResp struct {
	User struct {
		ID                 string `json:"id"`
		AttributionEnabled bool   `json:"attribution_enabled"`
	} `json:"user"`
}

// TestSetAttributionEnabledUsesSession: PUT /me/attribution flips ONLY the session
// user's flag — the store toggle receives the SESSION id, enabled is passed through,
// and the response carries the updated attribution_enabled. The body carries only
// {enabled}; the id is never taken from it.
func TestSetAttributionEnabledUsesSession(t *testing.T) {
	sessionID := uuid.New()
	db := &fakeAttributionUserDB{}
	h := &Handler{q: store.New(db)}

	req := httptest.NewRequest(http.MethodPut, "/api/me/attribution", strings.NewReader(`{"enabled": false}`))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
	rec := httptest.NewRecorder()

	h.SetAttributionEnabled(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.called {
		t.Fatal("SetUserAttributionEnabled was not called")
	}
	if db.gotID != sessionID {
		t.Errorf("toggle acted on id %v, want the SESSION id %v", db.gotID, sessionID)
	}
	if db.gotAttribution {
		t.Errorf("enabled=false from the body should have been passed through")
	}
	var out attributionToggleUserResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.User.ID != sessionID.String() {
		t.Errorf("response user id = %s, want session %s", out.User.ID, sessionID)
	}
	if out.User.AttributionEnabled {
		t.Errorf("response attribution_enabled = true, want the flipped-off value")
	}
}

// TestAttributionToggleRejectsSmuggledID: a body carrying an id field cannot smuggle a
// target — DecodeJSON is strict (DisallowUnknownFields), so an unexpected id is a 400
// and the toggle never runs. The target can therefore only ever be the session user,
// never a body-supplied id.
func TestAttributionToggleRejectsSmuggledID(t *testing.T) {
	sessionID := uuid.New()
	smuggle := `{"enabled": false, "id": "` + uuid.New().String() + `"}`

	db := &fakeAttributionUserDB{}
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodPut, "/api/me/attribution", strings.NewReader(smuggle))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
	rec := httptest.NewRecorder()

	h.SetAttributionEnabled(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field rejected); body=%s", rec.Code, rec.Body.String())
	}
	if db.called {
		t.Fatal("the toggle must not run when the body is rejected")
	}
}

// TestSetAttributionEnabledRequiresSession: with no user in context the handler is a
// 401 and never touches the store.
func TestSetAttributionEnabledRequiresSession(t *testing.T) {
	db := &fakeAttributionUserDB{}
	h := &Handler{q: store.New(db)}

	req := httptest.NewRequest(http.MethodPut, "/api/me/attribution", strings.NewReader(`{"enabled": false}`))
	rec := httptest.NewRecorder()

	h.SetAttributionEnabled(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if db.called {
		t.Fatal("the toggle must not run without a session user")
	}
}
