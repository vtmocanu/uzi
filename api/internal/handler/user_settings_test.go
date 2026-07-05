package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeSettingsDB is a store.DBTX holding one user's default_model, so the
// GetMySettings/PutMySettings handlers run end to end (decode -> validate ->
// store -> respond) without a real database. GetUserDefaultModel and
// SetUserDefaultModel both QueryRow a single default_model column; the UPDATE
// path records the written value so the round-trip is observable.
type fakeSettingsDB struct {
	model pgtype.Text
}

func (f *fakeSettingsDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeSettingsDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeSettingsDB: Query not used")
}

func (f *fakeSettingsDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "UPDATE users SET default_model") && len(args) >= 1 {
		if m, ok := args[0].(pgtype.Text); ok {
			f.model = m // SetUserDefaultModel: $1 = default_model
		}
	}
	return fakeSettingsRow{model: f.model}
}

type fakeSettingsRow struct{ model pgtype.Text }

func (r fakeSettingsRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if p, ok := dest[0].(*pgtype.Text); ok {
			*p = r.model
		}
	}
	return nil
}

// authed attaches a session user to a request (own-user endpoints read it).
func authed(req *http.Request) *http.Request {
	return req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()}))
}

func decodeSettings(t *testing.T, body []byte) *string {
	t.Helper()
	var resp struct {
		Settings struct {
			DefaultModel *string `json:"default_model"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings.DefaultModel
}

func TestUserSettingsRequireAuth(t *testing.T) {
	h := &Handler{} // no user in context ⇒ 401 before any store access
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"GET", h.GetMySettings, httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)},
		{"PUT", h.PutMySettings, httptest.NewRequest(http.MethodPut, "/api/me/settings", strings.NewReader(`{"default_model":"opus"}`))},
	} {
		rec := httptest.NewRecorder()
		tc.call(rec, tc.req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated: status = %d, want 401", tc.name, rec.Code)
		}
	}
}

func TestGetMySettingsReturnsStoredModel(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{model: pgtype.Text{String: "sonnet", Valid: true}})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeSettings(t, rec.Body.Bytes())
	if got == nil || *got != "sonnet" {
		t.Fatalf("default_model = %v, want \"sonnet\"", got)
	}
}

func TestGetMySettingsNullModelSerializesAsNull(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})} // model zero ⇒ NULL
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeSettings(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("default_model = %q, want null (inherit)", *got)
	}
}

func TestPutMySettingsRoundTrip(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_model":"opus"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeSettings(t, rec.Body.Bytes()); got == nil || *got != "opus" {
		t.Fatalf("response default_model = %v, want \"opus\"", got)
	}
	if !db.model.Valid || db.model.String != "opus" {
		t.Fatalf("stored model = %+v, want opus", db.model)
	}
}

func TestPutMySettingsBlankClearsToInherit(t *testing.T) {
	db := &fakeSettingsDB{model: pgtype.Text{String: "opus", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_model":""}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeSettings(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("response default_model = %q, want null after clearing", *got)
	}
	if db.model.Valid {
		t.Fatalf("stored model should be NULL after a blank clear, got %+v", db.model)
	}
}

func TestPutMySettingsRejectsInvalidModel(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_model":"claude 3"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a model with interior whitespace; body=%s", rec.Code, rec.Body.String())
	}
}
