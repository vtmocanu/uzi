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

// fakeNotifyEarlyResetUserDB is a store.DBTX that answers SetUserNotifyEarlyReset from
// memory, capturing the id + flag the handler passed so a test can assert WHICH id the
// toggle acted on (session vs body). It echoes the captured values back as the
// RETURNING row.
type fakeNotifyEarlyResetUserDB struct {
	gotID     uuid.UUID
	gotNotify bool
	called    bool
}

func (f *fakeNotifyEarlyResetUserDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeNotifyEarlyResetUserDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f *fakeNotifyEarlyResetUserDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "UPDATE users SET notify_early_limit_reset") {
		f.called = true
		if id, ok := args[0].(uuid.UUID); ok {
			f.gotID = id
		}
		if b, ok := args[1].(bool); ok {
			f.gotNotify = b
		}
		return fakeScanRow{scanNotifyEarlyResetUserRow(f.gotID, f.gotNotify)}
	}
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

// scanNotifyEarlyResetUserRow fills only the id + notify_early_limit_reset columns of
// the RETURNING row (the 1st and the 30th/last — see setUserNotifyEarlyReset in
// users.sql.go); the rest keep their zero values, which is all the toggle response
// needs.
func scanNotifyEarlyResetUserRow(id uuid.UUID, notify bool) func(dest ...any) error {
	return func(dest ...any) error {
		if p, ok := dest[0].(*uuid.UUID); ok {
			*p = id
		}
		if len(dest) >= 30 {
			if p, ok := dest[29].(*bool); ok {
				*p = notify
			}
		}
		return nil
	}
}

type notifyEarlyResetUserResp struct {
	User struct {
		ID                    string `json:"id"`
		NotifyEarlyLimitReset bool   `json:"notify_early_limit_reset"`
	} `json:"user"`
}

// TestSetUserNotifyEarlyResetUsesSession: PUT /me/notify-early-reset flips ONLY the
// session user's flag — the store toggle receives the SESSION id, enabled is passed
// through, and the response carries the updated notify_early_limit_reset. The body
// carries only {enabled}; the id is never taken from it. Runs both true and false so
// the persisted flag is asserted to follow the request in either direction.
func TestSetUserNotifyEarlyResetUsesSession(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		sessionID := uuid.New()
		db := &fakeNotifyEarlyResetUserDB{}
		h := &Handler{q: store.New(db)}

		body := `{"enabled": true}`
		if !enabled {
			body = `{"enabled": false}`
		}
		req := httptest.NewRequest(http.MethodPut, "/api/me/notify-early-reset", strings.NewReader(body))
		req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
		rec := httptest.NewRecorder()

		h.SetUserNotifyEarlyReset(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%v: status = %d, want 200; body=%s", enabled, rec.Code, rec.Body.String())
		}
		if !db.called {
			t.Fatalf("enabled=%v: SetUserNotifyEarlyReset was not called", enabled)
		}
		if db.gotID != sessionID {
			t.Errorf("enabled=%v: toggle acted on id %v, want the SESSION id %v", enabled, db.gotID, sessionID)
		}
		if db.gotNotify != enabled {
			t.Errorf("enabled=%v: flag passed to store = %v, want %v", enabled, db.gotNotify, enabled)
		}
		var out notifyEarlyResetUserResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("enabled=%v: decode: %v", enabled, err)
		}
		if out.User.ID != sessionID.String() {
			t.Errorf("enabled=%v: response user id = %s, want session %s", enabled, out.User.ID, sessionID)
		}
		if out.User.NotifyEarlyLimitReset != enabled {
			t.Errorf("enabled=%v: response notify_early_limit_reset = %v, want %v", enabled, out.User.NotifyEarlyLimitReset, enabled)
		}
	}
}

// TestNotifyEarlyResetRejectsSmuggledID: a body carrying an id field cannot smuggle a
// target — DecodeJSON is strict (DisallowUnknownFields), so an unexpected id is a 400
// and the toggle never runs. The target can therefore only ever be the session user,
// never a body-supplied id.
func TestNotifyEarlyResetRejectsSmuggledID(t *testing.T) {
	sessionID := uuid.New()
	smuggle := `{"enabled": true, "id": "` + uuid.New().String() + `"}`

	db := &fakeNotifyEarlyResetUserDB{}
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodPut, "/api/me/notify-early-reset", strings.NewReader(smuggle))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: sessionID, IsActive: true}))
	rec := httptest.NewRecorder()

	h.SetUserNotifyEarlyReset(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field rejected); body=%s", rec.Code, rec.Body.String())
	}
	if db.called {
		t.Fatal("the toggle must not run when the body is rejected")
	}
}

// TestSetUserNotifyEarlyResetRequiresSession: with no user in context the handler is a
// 401 and never touches the store.
func TestSetUserNotifyEarlyResetRequiresSession(t *testing.T) {
	db := &fakeNotifyEarlyResetUserDB{}
	h := &Handler{q: store.New(db)}

	req := httptest.NewRequest(http.MethodPut, "/api/me/notify-early-reset", strings.NewReader(`{"enabled": true}`))
	rec := httptest.NewRecorder()

	h.SetUserNotifyEarlyReset(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if db.called {
		t.Fatal("the toggle must not run without a session user")
	}
}
