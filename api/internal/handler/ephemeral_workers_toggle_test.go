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

// fakeEphemeralUserDB is a store.DBTX that answers SetUserEphemeralWorkersEnabled from
// memory, capturing the id + flag the handler passed so a test can assert WHICH id the
// toggle acted on (session vs body). It echoes the captured values back as the
// RETURNING row.
type fakeEphemeralUserDB struct {
	gotID        uuid.UUID
	gotEphemeral bool
	called       bool
}

func (f *fakeEphemeralUserDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeEphemeralUserDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f *fakeEphemeralUserDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "UPDATE users SET ephemeral_workers_enabled") {
		f.called = true
		if id, ok := args[0].(uuid.UUID); ok {
			f.gotID = id
		}
		if b, ok := args[1].(bool); ok {
			f.gotEphemeral = b
		}
		return fakeScanRow{scanEphemeralUserRow(f.gotID, f.gotEphemeral)}
	}
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

// scanEphemeralUserRow fills only the id + ephemeral_workers_enabled columns of the
// RETURNING row (the 1st and the 26th/last — see setUserEphemeralWorkersEnabled in
// users.sql.go); the rest keep their zero values, which is all the toggle response
// needs.
func scanEphemeralUserRow(id uuid.UUID, ephemeral bool) func(dest ...any) error {
	return func(dest ...any) error {
		if p, ok := dest[0].(*uuid.UUID); ok {
			*p = id
		}
		if len(dest) >= 26 {
			if p, ok := dest[25].(*bool); ok {
				*p = ephemeral
			}
		}
		return nil
	}
}

type ephemeralToggleUserResp struct {
	User struct {
		ID                      string `json:"id"`
		EphemeralWorkersEnabled bool   `json:"ephemeral_workers_enabled"`
	} `json:"user"`
}

// TestSetEphemeralWorkersEnabledUsesSession: PUT /me/ephemeral-workers flips ONLY the
// session user's flag — the store toggle receives the SESSION id, enabled is passed
// through, and the response carries the updated ephemeral_workers_enabled. The body
// carries only {enabled}; the id is never taken from it.
func TestSetEphemeralWorkersEnabledUsesSession(t *testing.T) {
	sessionID := uuid.New()
	db := &fakeEphemeralUserDB{}
	h := &Handler{q: store.New(db)}

	req := httptest.NewRequest(http.MethodPut, "/api/me/ephemeral-workers", strings.NewReader(`{"enabled": true}`))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
	rec := httptest.NewRecorder()

	h.SetEphemeralWorkersEnabled(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.called {
		t.Fatal("SetUserEphemeralWorkersEnabled was not called")
	}
	if db.gotID != sessionID {
		t.Errorf("toggle acted on id %v, want the SESSION id %v", db.gotID, sessionID)
	}
	if !db.gotEphemeral {
		t.Errorf("enabled flag not passed through")
	}
	var out ephemeralToggleUserResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.User.ID != sessionID.String() {
		t.Errorf("response user id = %s, want session %s", out.User.ID, sessionID)
	}
	if !out.User.EphemeralWorkersEnabled {
		t.Errorf("response ephemeral_workers_enabled = false, want the flipped-on value")
	}
}

// TestEphemeralToggleRejectsSmuggledID: a body carrying an id field cannot smuggle a
// target — DecodeJSON is strict (DisallowUnknownFields), so an unexpected id is a 400
// and the toggle never runs. The target can therefore only ever be the session user,
// never a body-supplied id.
func TestEphemeralToggleRejectsSmuggledID(t *testing.T) {
	sessionID := uuid.New()
	smuggle := `{"enabled": true, "id": "` + uuid.New().String() + `"}`

	db := &fakeEphemeralUserDB{}
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodPut, "/api/me/ephemeral-workers", strings.NewReader(smuggle))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
	rec := httptest.NewRecorder()

	h.SetEphemeralWorkersEnabled(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field rejected); body=%s", rec.Code, rec.Body.String())
	}
	if db.called {
		t.Fatal("the toggle must not run when the body is rejected")
	}
}

// TestSetEphemeralWorkersEnabledRequiresSession: with no user in context the handler
// is a 401 and never touches the store.
func TestSetEphemeralWorkersEnabledRequiresSession(t *testing.T) {
	db := &fakeEphemeralUserDB{}
	h := &Handler{q: store.New(db)}

	req := httptest.NewRequest(http.MethodPut, "/api/me/ephemeral-workers", strings.NewReader(`{"enabled": true}`))
	rec := httptest.NewRecorder()

	h.SetEphemeralWorkersEnabled(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if db.called {
		t.Fatal("the toggle must not run without a session user")
	}
}
