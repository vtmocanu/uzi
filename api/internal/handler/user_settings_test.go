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

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeSettingsDB is a store.DBTX holding one user's default_model, default_effort,
// judge_model, summary_model, theme, and sidebar token set, so the
// GetMySettings/PutMySettings handlers run end to end (decode -> validate -> store
// -> respond) without a real database. The
// SetUserDefaultModel/SetUserDefaultEffort/SetUserJudgeModel/SetUserSummaryModel/SetUserTheme
// UPDATEs QueryRow a single Text RETURNING column (discarded by the handler) and
// SetUserSidebarTokens a uuid[] one; GetUserSettings QueryRows six (default_model,
// default_effort, judge_model, summary_model, theme, sidebar_token_ids) —
// summary_model rides that one-row read, so the settings handler makes no separate
// GetUserSummaryModel call. The UPDATE paths record the written value so the
// round-trip is observable.
type fakeSettingsDB struct {
	model      pgtype.Text
	effort     pgtype.Text
	judge      pgtype.Text
	summary    pgtype.Text
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
	case strings.Contains(sql, "UPDATE users SET default_effort") && len(args) >= 1:
		if e, ok := args[0].(pgtype.Text); ok {
			f.effort = e // SetUserDefaultEffort: $1 = default_effort
		}
	case strings.Contains(sql, "UPDATE users SET judge_model") && len(args) >= 1:
		if j, ok := args[0].(pgtype.Text); ok {
			f.judge = j // SetUserJudgeModel: $1 = judge_model
		}
	case strings.Contains(sql, "UPDATE users SET summary_model") && len(args) >= 1:
		if s, ok := args[0].(pgtype.Text); ok {
			f.summary = s // SetUserSummaryModel: $1 = summary_model
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
	return fakeSettingsRow{
		model:      f.model,
		effort:     f.effort,
		judge:      f.judge,
		summary:    f.summary,
		theme:      f.theme,
		sidebarIDs: f.sidebarIDs,
	}
}

type fakeSettingsRow struct {
	model      pgtype.Text
	effort     pgtype.Text
	judge      pgtype.Text
	summary    pgtype.Text
	theme      pgtype.Text
	sidebarIDs []uuid.UUID
}

func (r fakeSettingsRow) Scan(dest ...any) error {
	switch len(dest) {
	case 1:
		// The only single-column reads left are write-path RETURNINGs (SetUser*),
		// which the handler discards, so any Text suffices.
		if p, ok := dest[0].(*pgtype.Text); ok {
			*p = r.model
		}
	case 6:
		// GetUserSettings: SELECT default_model, default_effort, judge_model,
		// summary_model, theme, sidebar_token_ids.
		if p, ok := dest[0].(*pgtype.Text); ok {
			*p = r.model
		}
		if p, ok := dest[1].(*pgtype.Text); ok {
			*p = r.effort
		}
		if p, ok := dest[2].(*pgtype.Text); ok {
			*p = r.judge
		}
		if p, ok := dest[3].(*pgtype.Text); ok {
			*p = r.summary
		}
		if p, ok := dest[4].(*pgtype.Text); ok {
			*p = r.theme
		}
		if p, ok := dest[5].(*[]uuid.UUID); ok {
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

// decodeJudgeModel pulls the judge_model field out of a /me/settings response.
func decodeJudgeModel(t *testing.T, body []byte) *string {
	t.Helper()
	var resp struct {
		Settings struct {
			JudgeModel *string `json:"judge_model"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings.JudgeModel
}

// PRD #69 M2: the per-user judge_model mirrors default_model — set, clear (blank
// ⇒ NULL/inherit), reject an invalid model, and GET reflects the stored value.

func TestGetMySettingsReturnsStoredJudgeModel(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{judge: pgtype.Text{String: "opus", Valid: true}})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJudgeModel(t, rec.Body.Bytes()); got == nil || *got != "opus" {
		t.Fatalf("judge_model = %v, want \"opus\"", got)
	}
}

func TestGetMySettingsNullJudgeModelSerializesAsNull(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})} // judge zero ⇒ NULL
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeJudgeModel(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("judge_model = %q, want null (inherit)", *got)
	}
}

func TestPutMySettingsJudgeModelRoundTrip(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"judge_model":"opus"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJudgeModel(t, rec.Body.Bytes()); got == nil || *got != "opus" {
		t.Fatalf("response judge_model = %v, want \"opus\"", got)
	}
	if !db.judge.Valid || db.judge.String != "opus" {
		t.Fatalf("stored judge_model = %+v, want opus", db.judge)
	}
}

func TestPutMySettingsBlankJudgeModelClearsToInherit(t *testing.T) {
	db := &fakeSettingsDB{judge: pgtype.Text{String: "opus", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"judge_model":""}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJudgeModel(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("response judge_model = %q, want null after clearing", *got)
	}
	if db.judge.Valid {
		t.Fatalf("stored judge_model should be NULL after a blank clear, got %+v", db.judge)
	}
}

func TestPutMySettingsRejectsInvalidJudgeModel(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"judge_model":"claude 3"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a model with interior whitespace; body=%s", rec.Code, rec.Body.String())
	}
}

// A judge-model-only PUT must not clobber the stored default_model: the two model
// controls save independently over the one endpoint (PATCH-like semantics).
func TestPutMySettingsJudgeModelOnlyLeavesDefaultModelUntouched(t *testing.T) {
	db := &fakeSettingsDB{model: pgtype.Text{String: "sonnet", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"judge_model":"opus"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.model.Valid || db.model.String != "sonnet" {
		t.Fatalf("default_model must be untouched by a judge-model-only PUT, got %+v", db.model)
	}
	if got := decodeSettings(t, rec.Body.Bytes()); got == nil || *got != "sonnet" {
		t.Fatalf("response default_model = %v, want the untouched \"sonnet\"", got)
	}
}

// decodeSummaryModel pulls the summary_model field out of a /me/settings response.
func decodeSummaryModel(t *testing.T, body []byte) *string {
	t.Helper()
	var resp struct {
		Settings struct {
			SummaryModel *string `json:"summary_model"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings.SummaryModel
}

// PRD #362 M2: the per-user summary_model mirrors judge_model — set, clear (blank
// ⇒ NULL/inherit), reject an invalid model, and GET reflects the stored value. This
// is the write path that gives SetUserSummaryModel a production caller.

func TestGetMySettingsReturnsStoredSummaryModel(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{summary: pgtype.Text{String: "haiku", Valid: true}})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeSummaryModel(t, rec.Body.Bytes()); got == nil || *got != "haiku" {
		t.Fatalf("summary_model = %v, want \"haiku\"", got)
	}
}

func TestGetMySettingsNullSummaryModelSerializesAsNull(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})} // summary zero ⇒ NULL
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeSummaryModel(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("summary_model = %q, want null (inherit)", *got)
	}
}

func TestPutMySettingsSummaryModelRoundTrip(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"summary_model":"haiku"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeSummaryModel(t, rec.Body.Bytes()); got == nil || *got != "haiku" {
		t.Fatalf("response summary_model = %v, want \"haiku\"", got)
	}
	if !db.summary.Valid || db.summary.String != "haiku" {
		t.Fatalf("stored summary_model = %+v, want haiku", db.summary)
	}
}

func TestPutMySettingsBlankSummaryModelClearsToInherit(t *testing.T) {
	db := &fakeSettingsDB{summary: pgtype.Text{String: "haiku", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"summary_model":""}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeSummaryModel(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("response summary_model = %q, want null after clearing", *got)
	}
	if db.summary.Valid {
		t.Fatalf("stored summary_model should be NULL after a blank clear, got %+v", db.summary)
	}
}

func TestPutMySettingsRejectsInvalidSummaryModel(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"summary_model":"claude 3"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a model with interior whitespace; body=%s", rec.Code, rec.Body.String())
	}
}

// A summary-model-only PUT must not clobber the stored judge_model: the model
// controls save independently over the one endpoint (PATCH-like semantics).
func TestPutMySettingsSummaryModelOnlyLeavesJudgeModelUntouched(t *testing.T) {
	db := &fakeSettingsDB{judge: pgtype.Text{String: "opus", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"summary_model":"haiku"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.judge.Valid || db.judge.String != "opus" {
		t.Fatalf("judge_model must be untouched by a summary-model-only PUT, got %+v", db.judge)
	}
	if got := decodeJudgeModel(t, rec.Body.Bytes()); got == nil || *got != "opus" {
		t.Fatalf("response judge_model = %v, want the untouched \"opus\"", got)
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

// decodeEffort pulls the default_effort field out of a /me/settings response.
func decodeEffort(t *testing.T, body []byte) *string {
	t.Helper()
	var resp struct {
		Settings struct {
			DefaultEffort *string `json:"default_effort"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings.DefaultEffort
}

// PRD #617 M2: the per-user default_effort mirrors default_model — set, clear
// (blank ⇒ NULL/inherit, and literal null ⇒ NULL/inherit), reject an off-enum
// value, and GET reflects the stored value. Validation goes through the
// closed-enum validateEffort, so only the five SDK levels are accepted.

func TestPutMySettingsEffortRoundTrip(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_effort":"low"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeEffort(t, rec.Body.Bytes()); got == nil || *got != "low" {
		t.Fatalf("response default_effort = %v, want \"low\"", got)
	}
	if !db.effort.Valid || db.effort.String != "low" {
		t.Fatalf("stored default_effort = %+v, want low", db.effort)
	}
	// A follow-up GET reflects the stored value.
	getRec := httptest.NewRecorder()
	h.GetMySettings(getRec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", getRec.Code, getRec.Body.String())
	}
	if got := decodeEffort(t, getRec.Body.Bytes()); got == nil || *got != "low" {
		t.Fatalf("GET default_effort = %v, want \"low\"", got)
	}
}

// Every SDK level (low, medium, high, xhigh, max) is accepted and round-trips.
func TestPutMySettingsEffortAcceptsEveryLevel(t *testing.T) {
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		db := &fakeSettingsDB{}
		h := &Handler{q: store.New(db)}
		rec := httptest.NewRecorder()
		body := []byte(`{"default_effort":"` + level + `"}`)
		req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader(body)))
		h.PutMySettings(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("level %q: status = %d, want 200; body=%s", level, rec.Code, rec.Body.String())
		}
		if got := decodeEffort(t, rec.Body.Bytes()); got == nil || *got != level {
			t.Fatalf("level %q: response default_effort = %v, want %q", level, got, level)
		}
		if !db.effort.Valid || db.effort.String != level {
			t.Fatalf("level %q: stored default_effort = %+v", level, db.effort)
		}
	}
}

func TestPutMySettingsBlankEffortClearsToInherit(t *testing.T) {
	db := &fakeSettingsDB{effort: pgtype.Text{String: "high", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_effort":""}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeEffort(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("response default_effort = %q, want null after a blank clear", *got)
	}
	if db.effort.Valid {
		t.Fatalf("stored default_effort should be NULL after a blank clear, got %+v", db.effort)
	}
}

// A literal null clears the override to inherit, distinct from an absent field.
func TestPutMySettingsNullEffortClearsToInherit(t *testing.T) {
	db := &fakeSettingsDB{effort: pgtype.Text{String: "high", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_effort":null}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeEffort(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("response default_effort = %q, want null after a null clear", *got)
	}
	if db.effort.Valid {
		t.Fatalf("stored default_effort should be NULL after a null clear, got %+v", db.effort)
	}
}

// An off-enum value is a 400 and must NEVER be stored.
func TestPutMySettingsRejectsInvalidEffort(t *testing.T) {
	db := &fakeSettingsDB{effort: pgtype.Text{String: "high", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_effort":"turbo"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an off-enum effort; body=%s", rec.Code, rec.Body.String())
	}
	if !db.effort.Valid || db.effort.String != "high" {
		t.Fatalf("stored default_effort must be untouched by a rejected value, got %+v", db.effort)
	}
}

// A model-only PUT must not clobber the stored default_effort: the controls save
// independently over the one endpoint (PATCH-like semantics).
func TestPutMySettingsModelOnlyLeavesEffortUntouched(t *testing.T) {
	db := &fakeSettingsDB{effort: pgtype.Text{String: "high", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_model":"opus"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.effort.Valid || db.effort.String != "high" {
		t.Fatalf("default_effort must be untouched by a model-only PUT, got %+v", db.effort)
	}
	if got := decodeEffort(t, rec.Body.Bytes()); got == nil || *got != "high" {
		t.Fatalf("response default_effort = %v, want the untouched \"high\"", got)
	}
}
