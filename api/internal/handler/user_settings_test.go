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

// fakeSettingsDB is a store.DBTX holding one user's default_model, theme, and
// sidebar token set, so the GetMySettings/PutMySettings handlers run end to end
// (decode -> validate -> store -> respond) without a real database. The
// SetUserDefaultModel/SetUserTheme UPDATEs QueryRow a single Text RETURNING
// column and SetUserSidebarTokens a uuid[] one; GetUserSettings QueryRows three
// (default_model, theme, sidebar_token_ids). The UPDATE paths record the
// written value so the round-trip is observable.
type fakeSettingsDB struct {
	model      pgtype.Text
	theme      pgtype.Text
	sidebarIDs []uuid.UUID
}

func (f *fakeSettingsDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeSettingsDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeSettingsDB: Query not used")
}

func (f *fakeSettingsDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "UPDATE users SET default_model") && len(args) >= 1:
		if m, ok := args[0].(pgtype.Text); ok {
			f.model = m // SetUserDefaultModel: $1 = default_model
		}
	case strings.Contains(sql, "UPDATE users SET theme") && len(args) >= 1:
		if t, ok := args[0].(pgtype.Text); ok {
			f.theme = t // SetUserTheme: $1 = theme
		}
	case strings.Contains(sql, "UPDATE users SET sidebar_token_ids") && len(args) >= 1:
		if ids, ok := args[0].([]uuid.UUID); ok {
			f.sidebarIDs = ids // SetUserSidebarTokens: $1 = sidebar_token_ids
		}
	}
	return fakeSettingsRow{model: f.model, theme: f.theme, sidebarIDs: f.sidebarIDs}
}

type fakeSettingsRow struct {
	model      pgtype.Text
	theme      pgtype.Text
	sidebarIDs []uuid.UUID
}

func (r fakeSettingsRow) Scan(dest ...any) error {
	switch len(dest) {
	case 1:
		// A single-column RETURNING (SetUserDefaultModel / SetUserTheme); the
		// handler discards it, so any Text value suffices.
		if p, ok := dest[0].(*pgtype.Text); ok {
			*p = r.model
		}
	case 3:
		// GetUserSettings: SELECT default_model, theme, sidebar_token_ids.
		if p, ok := dest[0].(*pgtype.Text); ok {
			*p = r.model
		}
		if p, ok := dest[1].(*pgtype.Text); ok {
			*p = r.theme
		}
		if p, ok := dest[2].(*[]uuid.UUID); ok {
			*p = r.sidebarIDs
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

// decodeTheme pulls the theme field out of a /me/settings response.
func decodeTheme(t *testing.T, body []byte) *string {
	t.Helper()
	var resp struct {
		Settings struct {
			Theme *string `json:"theme"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings.Theme
}

func TestGetMySettingsReturnsStoredTheme(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{theme: pgtype.Text{String: "mission", Valid: true}})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeTheme(t, rec.Body.Bytes()); got == nil || *got != "mission" {
		t.Fatalf("theme = %v, want \"mission\"", got)
	}
}

func TestPutMySettingsThemeRoundTrip(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"theme":"mission"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeTheme(t, rec.Body.Bytes()); got == nil || *got != "mission" {
		t.Fatalf("response theme = %v, want \"mission\"", got)
	}
	if !db.theme.Valid || db.theme.String != "mission" {
		t.Fatalf("stored theme = %+v, want mission", db.theme)
	}
}

func TestPutMySettingsNullThemeClearsOverride(t *testing.T) {
	db := &fakeSettingsDB{theme: pgtype.Text{String: "mission", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"theme":null}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeTheme(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("response theme = %q, want null after clearing the override", *got)
	}
	if db.theme.Valid {
		t.Fatalf("stored theme should be NULL after a null clear, got %+v", db.theme)
	}
}

func TestPutMySettingsRejectsUnknownTheme(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"theme":"neon"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown theme; body=%s", rec.Code, rec.Body.String())
	}
}

// A theme-only PUT must not clobber the stored model: absent default_model means
// "leave unchanged" (PATCH-like), which is what lets the two Settings controls
// save independently over the one endpoint.
func TestPutMySettingsThemeOnlyLeavesModelUntouched(t *testing.T) {
	db := &fakeSettingsDB{model: pgtype.Text{String: "opus", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"theme":"mission"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.model.Valid || db.model.String != "opus" {
		t.Fatalf("model must be untouched by a theme-only PUT, got %+v", db.model)
	}
	if got := decodeSettings(t, rec.Body.Bytes()); got == nil || *got != "opus" {
		t.Fatalf("response default_model = %v, want the untouched \"opus\"", got)
	}
}

// The mirror: a model-only PUT must not clobber the stored theme override.
func TestPutMySettingsModelOnlyLeavesThemeUntouched(t *testing.T) {
	db := &fakeSettingsDB{theme: pgtype.Text{String: "mission", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_model":"opus"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.theme.Valid || db.theme.String != "mission" {
		t.Fatalf("theme must be untouched by a model-only PUT, got %+v", db.theme)
	}
	if got := decodeTheme(t, rec.Body.Bytes()); got == nil || *got != "mission" {
		t.Fatalf("response theme = %v, want the untouched \"mission\"", got)
	}
}
