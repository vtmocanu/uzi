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

// The admin GET surface auto-surfaces the four PRD #1167 appearance default keys:
// each unset reads as its compiled-in default (default_dark_theme's default is ""
// because its legacy fallback chain lives in the accessor, not the stored default),
// so all four are present in the settings map. This is the read half; the write
// half (a valid value ⇒ 200) needs a live pool and lives in the LiveDB suite.
func TestGetSettingsSurfacesAppearanceDefaults(t *testing.T) {
	h := newSettingsHandler()
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
	for _, key := range []string{
		settings.KeyDefaultAppearanceMode, settings.KeyDefaultLightTheme,
		settings.KeyDefaultDarkTheme, settings.KeyDefaultTypeface,
	} {
		got, ok := resp.Settings[key]
		if !ok {
			t.Errorf("admin settings map missing key %q", key)
			continue
		}
		if want := settings.Defaults[key]; got != want {
			t.Errorf("%s = %q, want compiled default %q", key, got, want)
		}
	}
}

// A valid light-slot and dark-slot default id are accepted by the same
// settings.Validate the admin PUT delegates to (the write's 200 path needs a live
// pool; this pins the acceptance half the polarity-mismatch tests below reject).
func TestAppearanceDefaultKeysValidate(t *testing.T) {
	if err := settings.Validate(settings.KeyDefaultLightTheme, "dawn"); err != nil {
		t.Errorf("default_light_theme=dawn (a light id) should be accepted, got %v", err)
	}
	if err := settings.Validate(settings.KeyDefaultDarkTheme, "ember"); err != nil {
		t.Errorf("default_dark_theme=ember (a dark id) should be accepted, got %v", err)
	}
	if err := settings.Validate(settings.KeyDefaultAppearanceMode, "system"); err != nil {
		t.Errorf("default_appearance_mode=system should be accepted, got %v", err)
	}
	if err := settings.Validate(settings.KeyDefaultTypeface, "plex"); err != nil {
		t.Errorf("default_typeface=plex should be accepted, got %v", err)
	}
}

// The admin PUT rejects a polarity-mismatched appearance default with a 400 before
// any write: a dark id in the light slot and a light id in the dark slot both fail
// the per-key gate (settings.Validate ⇒ theme.ValidateFor), naming the key.
func TestUpdateSettingsRejectsAppearancePolarityMismatch(t *testing.T) {
	admin := adminUser()
	cases := map[string]struct {
		body string
		key  string
	}{
		"dark id in the light slot": {`{"settings":{"default_light_theme":"ember"}}`, "default_light_theme"},
		"light id in the dark slot": {`{"settings":{"default_dark_theme":"dawn"}}`, "default_dark_theme"},
		"unknown mode":              {`{"settings":{"default_appearance_mode":"bright"}}`, "default_appearance_mode"},
		"unknown typeface":          {`{"settings":{"default_typeface":"comic"}}`, "default_typeface"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSettingsHandler()
			rec := httptest.NewRecorder()
			h.UpdateSettings(rec, putSettings(&admin, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.key) {
				t.Fatalf("400 body %q should name the key %q", rec.Body.String(), tc.key)
			}
		})
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
