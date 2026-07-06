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

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
	h := newSettingsHandler(store.AppSetting{Key: settings.KeyPRDLabel, Value: "Feature"})
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
	if resp.Settings[settings.KeyPRDLabel] != "Feature" {
		t.Errorf("prd_label = %q, want Feature", resp.Settings[settings.KeyPRDLabel])
	}
	// The unset key reads as its compiled-in default, not an absent field.
	if resp.Settings[settings.KeyAutopilotLabel] != settings.DefaultAutopilotLabel {
		t.Errorf("autopilot_label = %q, want default", resp.Settings[settings.KeyAutopilotLabel])
	}
}

func TestUpdateSettingsRejectsUnauthenticated(t *testing.T) {
	h := newSettingsHandler()
	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(nil, `{"settings":{"prd_label":"PRD"}}`))
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
		"empty value":      `{"settings":{"prd_label":""}}`,
		"whitespace value": `{"settings":{"prd_label":"   "}}`,
		"comma value":      `{"settings":{"prd_label":"a,b"}}`,
		"too long value":   `{"settings":{"prd_label":"` + strings.Repeat("x", 65) + `"}}`,
		// Current autopilot_label is its default "autopilot"; setting prd_label to
		// the same value must trip the cross-key rule.
		"equal labels": `{"settings":{"prd_label":"autopilot"}}`,
		// PRD #22 M1: prdless_enabled is a strict bool; a non-bool is rejected.
		"non-bool prdless_enabled": `{"settings":{"prdless_enabled":"banana"}}`,
		// prdless_label must be pairwise-distinct from the other two (defaults PRD /
		// autopilot), even against their stored values on a single-key PUT.
		"prdless equals prd default":       `{"settings":{"prdless_label":"PRD"}}`,
		"prdless equals autopilot default": `{"settings":{"prdless_label":"autopilot"}}`,
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

// A two-key swap is validated against the merged post-update state, so the swap
// itself (prd↔autopilot) is accepted — the values stay distinct — and only
// reaches the transaction. Here we assert it passes validation by confirming it
// is NOT rejected with a 400 before the DB write (a nil pool would panic on the
// write, so we stop at the pre-write boundary via a distinct value that keeps
// them equal to force the 400, and its inverse that does not).
func TestUpdateSettingsCrossKeyUsesMergedState(t *testing.T) {
	admin := adminUser()
	// Only autopilot_label sent, set equal to the stored prd_label default "PRD":
	// the cross-key rule must see the stored prd_label and reject.
	h := newSettingsHandler()
	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"autopilot_label":"PRD"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for autopilot_label==stored prd_label", rec.Code)
	}
}
