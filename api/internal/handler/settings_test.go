package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// settingsStore is a minimal settings.Store: it serves a fixed row set so the
// handler's cache-backed reads (GetSettings, the cross-key precheck in
// UpdateSettings) resolve without a database.
type settingsStore struct {
	rows []store.AppSetting
}

func (s *settingsStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	return s.rows, nil
}

func newSettingsHandler(rows ...store.AppSetting) *Handler {
	return &Handler{settings: settings.New(&settingsStore{rows: rows}, time.Minute)}
}

func adminUser() store.User {
	return store.User{ID: uuid.New(), Email: "admin@uzi.local", IsAdmin: true, IsActive: true}
}

// putSettings builds a PUT /admin/settings request carrying body, authed as the
// given user (nil user = unauthenticated).
func putSettings(user *store.User, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
	if user != nil {
		req = req.WithContext(mw.ContextWithUser(req.Context(), *user))
	}
	return req
}

func TestGetSettingsReturnsKnownKeysWithDefaults(t *testing.T) {
	h := newSettingsHandler(store.AppSetting{Key: settings.KeyUziLabel, Value: "Feature"})
	rec := httptest.NewRecorder()
	h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Settings map[string]string `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Settings[settings.KeyUziLabel] != "Feature" {
		t.Errorf("uzi_label = %q, want Feature", resp.Settings[settings.KeyUziLabel])
	}
	// The unset key reads as its compiled-in default, not an absent field.
	if resp.Settings[settings.KeyAutopilotLabel] != settings.DefaultAutopilotLabel {
		t.Errorf("autopilot_label = %q, want default", resp.Settings[settings.KeyAutopilotLabel])
	}
}

// The admin GET surface auto-surfaces new Defaults keys (PRD #69): judge_enforce_all
// reads as its compiled-in default when unset, and a stored bool round-trips
// verbatim through the admin view. This is the read half of the enforce-all setting;
// the write half needs a live pool and lives in the LiveDB suite.
func TestGetSettingsSurfacesJudgeEnforceAll(t *testing.T) {
	// Unset → compiled-in default "false".
	h := newSettingsHandler()
	rec := httptest.NewRecorder()
	h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	var resp struct {
		Settings map[string]string `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Settings[settings.KeyJudgeEnforceAll]; got != settings.DefaultJudgeEnforceAll {
		t.Errorf("judge_enforce_all default = %q, want %q", got, settings.DefaultJudgeEnforceAll)
	}

	// A stored "true" round-trips through the admin view.
	h = newSettingsHandler(store.AppSetting{Key: settings.KeyJudgeEnforceAll, Value: "true"})
	rec = httptest.NewRecorder()
	h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Settings[settings.KeyJudgeEnforceAll]; got != "true" {
		t.Errorf("judge_enforce_all stored = %q, want true", got)
	}
}

// The admin GET surface auto-surfaces the PRD #84 capability-aware scheduling
// kill-switch: unset it reads as its compiled-in default ("true"), and a stored bool
// round-trips verbatim through the admin view. Read half; the write half round-trips
// through Validate's bool case (settings package test).
func TestGetSettingsSurfacesCapabilityAwareScheduling(t *testing.T) {
	// Unset → compiled-in default "true".
	h := newSettingsHandler()
	rec := httptest.NewRecorder()
	h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	var resp struct {
		Settings map[string]string `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Settings[settings.KeyCapabilityAwareScheduling]; got != settings.DefaultCapabilityAwareScheduling {
		t.Errorf("capability_aware_scheduling default = %q, want %q", got, settings.DefaultCapabilityAwareScheduling)
	}

	// A stored "false" round-trips through the admin view.
	h = newSettingsHandler(store.AppSetting{Key: settings.KeyCapabilityAwareScheduling, Value: "false"})
	rec = httptest.NewRecorder()
	h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Settings[settings.KeyCapabilityAwareScheduling]; got != "false" {
		t.Errorf("capability_aware_scheduling stored = %q, want false", got)
	}
}

func TestUpdateSettingsRejectsUnauthenticated(t *testing.T) {
	h := newSettingsHandler()
	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(nil, `{"settings":{"uzi_label":"uzi"}}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The validation-rejection paths all return before the transaction, so they run
// against the cache-backed store with no pool.
func TestUpdateSettingsValidationRejections(t *testing.T) {
	admin := adminUser()
	cases := map[string]string{
		"empty body":       `{"settings":{}}`,
		"unknown key":      `{"settings":{"bogus":"x"}}`,
		"empty value":      `{"settings":{"uzi_label":""}}`,
		"whitespace value": `{"settings":{"uzi_label":"   "}}`,
		"comma value":      `{"settings":{"uzi_label":"a,b"}}`,
		"too long value":   `{"settings":{"uzi_label":"` + strings.Repeat("x", 65) + `"}}`,
		// Current autopilot_label is its default "autopilot"; setting uzi_label to
		// the same value must trip the cross-key rule (PRD #764).
		"equal labels": `{"settings":{"uzi_label":"autopilot"}}`,
		// PRD #69: judge_enforce_all is a strict bool. "yes" MUST be rejected — it is
		// the documented pitfall: were the key to fall through to ValidateLabel it would
		// accept "yes" and then read as false, silently disabling enforcement.
		"non-bool judge_enforce_all": `{"settings":{"judge_enforce_all":"yes"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSettingsHandler()
			rec := httptest.NewRecorder()
			h.UpdateSettings(rec, putSettings(&admin, body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The cross-key rule is validated against the merged post-update state, so a
// single-key PUT is still checked against the stored value of the other key.
func TestUpdateSettingsCrossKeyUsesMergedState(t *testing.T) {
	admin := adminUser()
	// Only autopilot_label sent, set equal to the stored uzi_label default "uzi":
	// the cross-key rule must see the stored uzi_label and reject (PRD #764).
	h := newSettingsHandler()
	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"autopilot_label":"uzi"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for autopilot_label==stored uzi_label", rec.Code)
	}
}
