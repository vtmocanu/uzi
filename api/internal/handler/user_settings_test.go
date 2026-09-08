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
// SetUserSidebarTokens a uuid[] one; GetUserSettings QueryRows eleven (default_model,
// default_effort, judge_model, summary_model, theme, sidebar_token_ids,
// mr_rework_enabled, appearance_mode, light_theme, dark_theme, typeface — the four
// appearance columns joined the read in PRD #1167 M1) — summary_model rides that
// one-row read, so the settings handler makes no separate GetUserSummaryModel call.
// The UPDATE paths record the written value so the round-trip is observable.
type fakeSettingsDB struct {
	model      pgtype.Text
	effort     pgtype.Text
	judge      pgtype.Text
	summary    pgtype.Text
	theme      pgtype.Text
	mrRework   pgtype.Bool
	sidebarIDs []uuid.UUID
	apprMode   pgtype.Text
	lightTheme pgtype.Text
	darkTheme  pgtype.Text
	typeface   pgtype.Text
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
	case strings.Contains(sql, "UPDATE users SET mr_rework_enabled") && len(args) >= 1:
		if b, ok := args[0].(pgtype.Bool); ok {
			f.mrRework = b // SetUserMrReworkEnabled: $1 = mr_rework_enabled
		}
	case strings.Contains(sql, "UPDATE users SET sidebar_token_ids") && len(args) >= 1:
		if ids, ok := args[0].([]uuid.UUID); ok {
			f.sidebarIDs = ids // SetUserSidebarTokens: $1 = sidebar_token_ids
		}
	case strings.Contains(sql, "SET appearance_mode") && len(args) >= 4:
		// SetUserAppearance writes all four columns at once (PRD #1167 M1):
		// $1=appearance_mode, $2=light_theme, $3=dark_theme, $4=typeface, $5=id.
		if m, ok := args[0].(pgtype.Text); ok {
			f.apprMode = m
		}
		if l, ok := args[1].(pgtype.Text); ok {
			f.lightTheme = l
		}
		if d, ok := args[2].(pgtype.Text); ok {
			f.darkTheme = d
		}
		if tf, ok := args[3].(pgtype.Text); ok {
			f.typeface = tf
		}
	}
	return fakeSettingsRow{
		model:      f.model,
		effort:     f.effort,
		judge:      f.judge,
		summary:    f.summary,
		theme:      f.theme,
		mrRework:   f.mrRework,
		sidebarIDs: f.sidebarIDs,
		apprMode:   f.apprMode,
		lightTheme: f.lightTheme,
		darkTheme:  f.darkTheme,
		typeface:   f.typeface,
	}
}

type fakeSettingsRow struct {
	model      pgtype.Text
	effort     pgtype.Text
	judge      pgtype.Text
	summary    pgtype.Text
	theme      pgtype.Text
	mrRework   pgtype.Bool
	sidebarIDs []uuid.UUID
	apprMode   pgtype.Text
	lightTheme pgtype.Text
	darkTheme  pgtype.Text
	typeface   pgtype.Text
}

func (r fakeSettingsRow) Scan(dest ...any) error {
	switch len(dest) {
	case 1:
		// Single-column reads are write-path RETURNINGs (SetUser*), which the handler
		// discards. Most return a Text column; SetUserMrReworkEnabled returns a Bool.
		if p, ok := dest[0].(*pgtype.Text); ok {
			*p = r.model
		}
		if p, ok := dest[0].(*pgtype.Bool); ok {
			*p = r.mrRework
		}
	case 4:
		// SetUserAppearance RETURNING appearance_mode, light_theme, dark_theme,
		// typeface (PRD #1167 M1). Discarded by the handler, captured here so the
		// write is observable on a follow-up GetUserSettings.
		if p, ok := dest[0].(*pgtype.Text); ok {
			*p = r.apprMode
		}
		if p, ok := dest[1].(*pgtype.Text); ok {
			*p = r.lightTheme
		}
		if p, ok := dest[2].(*pgtype.Text); ok {
			*p = r.darkTheme
		}
		if p, ok := dest[3].(*pgtype.Text); ok {
			*p = r.typeface
		}
	case 11:
		// GetUserSettings: SELECT default_model, default_effort, judge_model,
		// summary_model, theme, sidebar_token_ids, mr_rework_enabled,
		// appearance_mode, light_theme, dark_theme, typeface (the last four are
		// PRD #1167 M1's appearance columns riding the same one-row read).
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
		if p, ok := dest[6].(*pgtype.Bool); ok {
			*p = r.mrRework
		}
		if p, ok := dest[7].(*pgtype.Text); ok {
			*p = r.apprMode
		}
		if p, ok := dest[8].(*pgtype.Text); ok {
			*p = r.lightTheme
		}
		if p, ok := dest[9].(*pgtype.Text); ok {
			*p = r.darkTheme
		}
		if p, ok := dest[10].(*pgtype.Text); ok {
			*p = r.typeface
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

// decodeMrRework pulls the mr_rework_enabled field out of a /me/settings response.
func decodeMrRework(t *testing.T, body []byte) *bool {
	t.Helper()
	var resp struct {
		Settings struct {
			MrReworkEnabled *bool `json:"mr_rework_enabled"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings.MrReworkEnabled
}

// A NULL mr_rework_enabled column (the default-ON, opted-in state) serializes as
// JSON null so the client distinguishes "unset" from an explicit false (PRD #700 M5).
func TestGetMySettingsNullMrReworkSerializesAsNull(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})} // mrRework zero ⇒ NULL
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeMrRework(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("mr_rework_enabled = %v, want null (unset = default-ON)", *got)
	}
}

// A stored explicit false (the opt-OUT) is surfaced verbatim.
func TestGetMySettingsReturnsStoredMrRework(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{mrRework: pgtype.Bool{Bool: false, Valid: true}})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeMrRework(t, rec.Body.Bytes())
	if got == nil || *got != false {
		t.Fatalf("mr_rework_enabled = %v, want explicit false (opted out)", got)
	}
}

// PUT of an explicit false stores the opt-out and round-trips it back.
func TestPutMySettingsMrReworkRoundTrip(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"mr_rework_enabled":false}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeMrRework(t, rec.Body.Bytes()); got == nil || *got != false {
		t.Fatalf("response mr_rework_enabled = %v, want false", got)
	}
	if !db.mrRework.Valid || db.mrRework.Bool != false {
		t.Fatalf("stored mr_rework_enabled = %+v, want a set false (opt-out)", db.mrRework)
	}
}

// PUT of a present-null clears the opt-out back to NULL = the default-ON state.
func TestPutMySettingsNullMrReworkClearsToDefault(t *testing.T) {
	db := &fakeSettingsDB{mrRework: pgtype.Bool{Bool: false, Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"mr_rework_enabled":null}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeMrRework(t, rec.Body.Bytes()); got != nil {
		t.Fatalf("response mr_rework_enabled = %v, want null after clearing to default-ON", *got)
	}
	if db.mrRework.Valid {
		t.Fatalf("stored mr_rework_enabled should be NULL after a null clear, got %+v", db.mrRework)
	}
}

// A non-bool mr_rework_enabled body is a 400 and does not touch the stored value.
func TestPutMySettingsRejectsNonBoolMrRework(t *testing.T) {
	db := &fakeSettingsDB{mrRework: pgtype.Bool{Bool: false, Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"mr_rework_enabled":"yes"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a non-bool mr_rework_enabled; body=%s", rec.Code, rec.Body.String())
	}
	if !db.mrRework.Valid || db.mrRework.Bool != false {
		t.Fatalf("stored mr_rework_enabled must be untouched by a rejected value, got %+v", db.mrRework)
	}
}

// An mr_rework-only PUT must not clobber the stored default_model (PATCH-like).
func TestPutMySettingsMrReworkOnlyLeavesModelUntouched(t *testing.T) {
	db := &fakeSettingsDB{model: pgtype.Text{String: "opus", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"mr_rework_enabled":false}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.model.Valid || db.model.String != "opus" {
		t.Fatalf("default_model must be untouched by an mr_rework-only PUT, got %+v", db.model)
	}
}

// --- Appearance (PRD #1167 M2): appearance_mode / light_theme / dark_theme / typeface ---

// decodeAppearance pulls the four raw appearance override fields out of a
// /me/settings response (each a nullable *string mirroring the stored column).
func decodeAppearance(t *testing.T, body []byte) (mode, light, dark, typeface *string) {
	t.Helper()
	var resp struct {
		Settings struct {
			AppearanceMode *string `json:"appearance_mode"`
			LightTheme     *string `json:"light_theme"`
			DarkTheme      *string `json:"dark_theme"`
			Typeface       *string `json:"typeface"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings.AppearanceMode, resp.Settings.LightTheme, resp.Settings.DarkTheme, resp.Settings.Typeface
}

// GET reflects the four stored raw override columns (each NULL ⇒ JSON null).
func TestGetMySettingsReturnsStoredAppearance(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{
		apprMode:   pgtype.Text{String: "light", Valid: true},
		lightTheme: pgtype.Text{String: "dawn", Valid: true},
		darkTheme:  pgtype.Text{String: "mission", Valid: true},
		typeface:   pgtype.Text{String: "plex", Valid: true},
	})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	mode, light, dark, tf := decodeAppearance(t, rec.Body.Bytes())
	if mode == nil || *mode != "light" || light == nil || *light != "dawn" ||
		dark == nil || *dark != "mission" || tf == nil || *tf != "plex" {
		t.Fatalf("appearance = mode=%v light=%v dark=%v typeface=%v, want light/dawn/mission/plex", mode, light, dark, tf)
	}
}

// An unset appearance column serializes as JSON null (inherit the instance default).
func TestGetMySettingsNullAppearanceSerializesAsNull(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	mode, light, dark, tf := decodeAppearance(t, rec.Body.Bytes())
	if mode != nil || light != nil || dark != nil || tf != nil {
		t.Fatalf("unset appearance must be null, got mode=%v light=%v dark=%v typeface=%v", mode, light, dark, tf)
	}
}

// Each field: a valid value round-trips through the store and back out on GET.
func TestPutMySettingsAppearanceRoundTrip(t *testing.T) {
	cases := []struct {
		field string
		value string
	}{
		{"appearance_mode", "light"},
		{"light_theme", "dawn"},
		{"dark_theme", "mission"},
		{"typeface", "plex"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			db := &fakeSettingsDB{}
			h := &Handler{q: store.New(db)}
			rec := httptest.NewRecorder()
			body := []byte(`{"` + tc.field + `":"` + tc.value + `"}`)
			h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader(body))))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200; body=%s", tc.field, rec.Code, rec.Body.String())
			}

			// A follow-up GET reflects the stored value.
			getRec := httptest.NewRecorder()
			h.GetMySettings(getRec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
			mode, light, dark, tf := decodeAppearance(t, getRec.Body.Bytes())
			got := map[string]*string{"appearance_mode": mode, "light_theme": light, "dark_theme": dark, "typeface": tf}[tc.field]
			if got == nil || *got != tc.value {
				t.Fatalf("%s round-trip = %v, want %q", tc.field, got, tc.value)
			}
		})
	}
}

// A present-null clears an appearance column back to NULL (inherit the default).
func TestPutMySettingsNullAppearanceClearsToInherit(t *testing.T) {
	db := &fakeSettingsDB{lightTheme: pgtype.Text{String: "dawn", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"light_theme":null}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.lightTheme.Valid {
		t.Fatalf("stored light_theme should be NULL after a null clear, got %+v", db.lightTheme)
	}
	if _, light, _, _ := decodeAppearance(t, rec.Body.Bytes()); light != nil {
		t.Fatalf("response light_theme = %q, want null after clearing", *light)
	}
}

// An absent appearance field leaves its stored column untouched (PATCH-like).
func TestPutMySettingsAppearanceAbsentLeavesColumnUntouched(t *testing.T) {
	db := &fakeSettingsDB{
		apprMode:   pgtype.Text{String: "dark", Valid: true},
		lightTheme: pgtype.Text{String: "dawn", Valid: true},
		darkTheme:  pgtype.Text{String: "mission", Valid: true},
		typeface:   pgtype.Text{String: "plex", Valid: true},
	}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	// Only typeface is sent; the other three columns must survive the merge.
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"typeface":"system"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.apprMode.Valid || db.apprMode.String != "dark" {
		t.Fatalf("appearance_mode must survive a typeface-only PUT, got %+v", db.apprMode)
	}
	if !db.lightTheme.Valid || db.lightTheme.String != "dawn" {
		t.Fatalf("light_theme must survive a typeface-only PUT, got %+v", db.lightTheme)
	}
	if !db.darkTheme.Valid || db.darkTheme.String != "mission" {
		t.Fatalf("dark_theme must survive a typeface-only PUT, got %+v", db.darkTheme)
	}
	if !db.typeface.Valid || db.typeface.String != "system" {
		t.Fatalf("typeface should be updated, got %+v", db.typeface)
	}
}

// Each invalid/polarity-wrong value is a 400 that names the field and never writes.
func TestPutMySettingsRejectsInvalidAppearance(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad mode", `{"appearance_mode":"bright"}`, "appearance_mode"},
		{"light slot given a dark theme", `{"light_theme":"ember"}`, "light_theme"},
		{"dark slot given a light theme", `{"dark_theme":"dawn"}`, "dark_theme"},
		{"unknown light theme", `{"light_theme":"neon"}`, "light_theme"},
		{"bad typeface", `{"typeface":"comic"}`, "typeface"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeSettingsDB{apprMode: pgtype.Text{String: "dark", Valid: true}}
			h := &Handler{q: store.New(db)}
			rec := httptest.NewRecorder()
			req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(tc.body))))
			h.PutMySettings(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("400 body %q should name the field %q", rec.Body.String(), tc.want)
			}
			// A rejected value must not touch any stored appearance column.
			if !db.apprMode.Valid || db.apprMode.String != "dark" {
				t.Fatalf("appearance_mode must be untouched by a rejected value, got %+v", db.apprMode)
			}
		})
	}
}

// The light_theme wrong-polarity message is the theme.ValidateFor prose, prefixed
// with the field so a client can tell "wrong slot" from "unknown id".
func TestPutMySettingsLightThemePolarityMessage(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"light_theme":"ember"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body %s: %v", rec.Body.String(), err)
	}
	if errBody.Error != `light_theme: theme "ember" is not a light theme` {
		t.Fatalf("error = %q, want the prefixed polarity message", errBody.Error)
	}
}

// Legacy theme mapping: a DARK theme folds into the dark slot AND sets mode=dark,
// while still writing users.theme for the deprecated trio's consistency.
func TestPutMySettingsLegacyThemeMapsDarkSlot(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"theme":"mission"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// users.theme still written (deprecated trio consistency).
	if !db.theme.Valid || db.theme.String != "mission" {
		t.Fatalf("legacy users.theme = %+v, want mission", db.theme)
	}
	// Folded into the dark slot + mode=dark.
	if !db.darkTheme.Valid || db.darkTheme.String != "mission" {
		t.Fatalf("dark_theme = %+v, want mission (folded from legacy theme)", db.darkTheme)
	}
	if !db.apprMode.Valid || db.apprMode.String != "dark" {
		t.Fatalf("appearance_mode = %+v, want dark (folded from legacy theme)", db.apprMode)
	}
	// GET reflects the fold.
	mode, _, dark, _ := decodeAppearance(t, rec.Body.Bytes())
	if mode == nil || *mode != "dark" || dark == nil || *dark != "mission" {
		t.Fatalf("GET after legacy dark theme: mode=%v dark=%v, want dark/mission", mode, dark)
	}
}

// Legacy theme mapping: a LIGHT theme folds into the light slot AND sets mode=light.
func TestPutMySettingsLegacyThemeMapsLightSlot(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"theme":"dawn"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.theme.Valid || db.theme.String != "dawn" {
		t.Fatalf("legacy users.theme = %+v, want dawn", db.theme)
	}
	if !db.lightTheme.Valid || db.lightTheme.String != "dawn" {
		t.Fatalf("light_theme = %+v, want dawn (folded from legacy theme)", db.lightTheme)
	}
	if !db.apprMode.Valid || db.apprMode.String != "light" {
		t.Fatalf("appearance_mode = %+v, want light (folded from legacy theme)", db.apprMode)
	}
}

// A null legacy theme clears users.theme and must NOT touch the appearance columns.
func TestPutMySettingsNullLegacyThemeLeavesAppearanceUntouched(t *testing.T) {
	db := &fakeSettingsDB{
		theme:    pgtype.Text{String: "mission", Valid: true},
		apprMode: pgtype.Text{String: "dark", Valid: true},
		darkTheme: pgtype.Text{
			String: "mission", Valid: true,
		},
	}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"theme":null}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.theme.Valid {
		t.Fatalf("users.theme should be cleared, got %+v", db.theme)
	}
	if !db.apprMode.Valid || db.apprMode.String != "dark" {
		t.Fatalf("appearance_mode must be untouched by a null legacy theme, got %+v", db.apprMode)
	}
	if !db.darkTheme.Valid || db.darkTheme.String != "mission" {
		t.Fatalf("dark_theme must be untouched by a null legacy theme, got %+v", db.darkTheme)
	}
}

// Explicit new fields win over the legacy theme mapping when both are sent.
func TestPutMySettingsExplicitAppearanceWinsOverLegacyTheme(t *testing.T) {
	db := &fakeSettingsDB{}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	// Legacy mission → dark slot + mode=dark; explicit mode=light and light=dawn win.
	body := []byte(`{"theme":"mission","appearance_mode":"light","light_theme":"dawn"}`)
	h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader(body))))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.apprMode.Valid || db.apprMode.String != "light" {
		t.Fatalf("appearance_mode = %+v, want light (explicit wins over legacy dark)", db.apprMode)
	}
	if !db.lightTheme.Valid || db.lightTheme.String != "dawn" {
		t.Fatalf("light_theme = %+v, want dawn (explicit)", db.lightTheme)
	}
	// The legacy fold's dark slot still lands (not overridden by an explicit dark_theme).
	if !db.darkTheme.Valid || db.darkTheme.String != "mission" {
		t.Fatalf("dark_theme = %+v, want mission (legacy fold, no explicit override)", db.darkTheme)
	}
}

// Read-merge: an appearance_mode-only PUT preserves the other three columns.
func TestPutMySettingsAppearanceModeOnlyLeavesSlotsUntouched(t *testing.T) {
	db := &fakeSettingsDB{
		apprMode:   pgtype.Text{String: "dark", Valid: true},
		lightTheme: pgtype.Text{String: "dawn", Valid: true},
		darkTheme:  pgtype.Text{String: "mission", Valid: true},
		typeface:   pgtype.Text{String: "plex", Valid: true},
	}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"appearance_mode":"light"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.apprMode.Valid || db.apprMode.String != "light" {
		t.Fatalf("appearance_mode = %+v, want light", db.apprMode)
	}
	if !db.lightTheme.Valid || db.lightTheme.String != "dawn" {
		t.Fatalf("light_theme must survive a mode-only PUT, got %+v", db.lightTheme)
	}
	if !db.darkTheme.Valid || db.darkTheme.String != "mission" {
		t.Fatalf("dark_theme must survive a mode-only PUT, got %+v", db.darkTheme)
	}
	if !db.typeface.Valid || db.typeface.String != "plex" {
		t.Fatalf("typeface must survive a mode-only PUT, got %+v", db.typeface)
	}
}

// A PUT with no appearance/theme field must not issue an appearance write at all,
// so a stored appearance is left completely untouched (no read-merge no-op).
func TestPutMySettingsModelOnlyLeavesAppearanceUntouched(t *testing.T) {
	db := &fakeSettingsDB{apprMode: pgtype.Text{String: "light", Valid: true}}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(`{"default_model":"opus"}`))))
	h.PutMySettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.apprMode.Valid || db.apprMode.String != "light" {
		t.Fatalf("appearance_mode must be untouched by a model-only PUT, got %+v", db.apprMode)
	}
}
