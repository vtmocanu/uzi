package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// parseDomain mirrors what Register does before the allowlist check: lowercase,
// parse to an addr-spec, then take emailDomain of addr.Address. Used to assert
// domain extraction across display-name and quoted-local-part forms.
func parseDomain(t *testing.T, raw string) string {
	t.Helper()
	addr, err := mail.ParseAddress(strings.ToLower(raw))
	if err != nil {
		t.Fatalf("ParseAddress(%q): %v", raw, err)
	}
	return emailDomain(addr.Address)
}

// TestSessionPayloadCarriesUziLabel pins the PRD #764 contract on the session
// payload: the configured uzi_label rides this response so the issue view and the
// card Start affordance — which have no board payload — can read it. Only the
// uzi_label and autopilot_label of the label family are emitted; the retired
// eligibility keys are gone (a positive guard: the only label-family keys present
// are the two expected ones).
func TestSessionPayloadCarriesUziLabel(t *testing.T) {
	h := newSettingsHandler(
		store.AppSetting{Key: settings.KeyUziLabel, Value: "runnable"},
		store.AppSetting{Key: settings.KeyAutopilotLabel, Value: "hands-off"},
	)
	payload := h.sessionPayload(context.Background(), store.User{})

	if uzi, ok := payload["uzi_label"].(string); !ok || uzi != "runnable" {
		t.Errorf("uzi_label = %v (%T), want \"runnable\"", payload["uzi_label"], payload["uzi_label"])
	}
	if ap, ok := payload["autopilot_label"].(string); !ok || ap != "hands-off" {
		t.Errorf("autopilot_label = %v (%T), want \"hands-off\"", payload["autopilot_label"], payload["autopilot_label"])
	}

	// No other key in the payload ends in "_label" (positive guard that the retired
	// label-family keys are gone without naming them).
	for k := range payload {
		if k == "uzi_label" || k == "autopilot_label" {
			continue
		}
		if strings.HasSuffix(k, "_label") {
			t.Errorf("session payload emits an unexpected label-family key %q", k)
		}
	}
}

// TestSessionPayloadJudgeConsentFields pins the PRD #69 M4 consent channel on the
// session payload: judge_enforced_by_admin follows the Gate-2-wins semantics (the
// kill-switch dominates enforce_all), and effective_judge_model is resolved
// user-value-wins over the instance value, which itself falls back to opus.
func TestSessionPayloadJudgeConsentFields(t *testing.T) {
	// Kill-switch ON + enforce_all ON ⇒ enforced. Instance model "sonnet", user has
	// no per-user override ⇒ effective is the instance value.
	h := newSettingsHandler(
		store.AppSetting{Key: settings.KeyJudgeEnabled, Value: "true"},
		store.AppSetting{Key: settings.KeyJudgeEnforceAll, Value: "true"},
		store.AppSetting{Key: settings.KeyJudgeModel, Value: "sonnet"},
	)
	payload := h.sessionPayload(context.Background(), store.User{})
	if enforced, ok := payload["judge_enforced_by_admin"].(bool); !ok || !enforced {
		t.Errorf("judge_enforced_by_admin = %v (%T), want true", payload["judge_enforced_by_admin"], payload["judge_enforced_by_admin"])
	}
	if m := payload["effective_judge_model"]; m != "sonnet" {
		t.Errorf("effective_judge_model = %v, want sonnet (instance value)", m)
	}

	// Kill-switch OFF but enforce_all ON ⇒ NOT enforced (Gate-2-wins). A user override
	// wins for the effective model regardless of the enforced flag.
	h2 := newSettingsHandler(
		store.AppSetting{Key: settings.KeyJudgeEnabled, Value: "false"},
		store.AppSetting{Key: settings.KeyJudgeEnforceAll, Value: "true"},
		store.AppSetting{Key: settings.KeyJudgeModel, Value: "sonnet"},
	)
	user := store.User{JudgeModel: pgtype.Text{String: "haiku", Valid: true}}
	payload2 := h2.sessionPayload(context.Background(), user)
	if enforced, ok := payload2["judge_enforced_by_admin"].(bool); !ok || enforced {
		t.Errorf("judge_enforced_by_admin = %v, want false when kill-switch off", payload2["judge_enforced_by_admin"])
	}
	if m := payload2["effective_judge_model"]; m != "haiku" {
		t.Errorf("effective_judge_model = %v, want haiku (user override wins)", m)
	}

	// No judge settings at all ⇒ not enforced, effective falls back to the compiled-in
	// DefaultJudgeModel (opus) rather than an empty string.
	h3 := newSettingsHandler()
	payload3 := h3.sessionPayload(context.Background(), store.User{})
	if enforced, _ := payload3["judge_enforced_by_admin"].(bool); enforced {
		t.Errorf("judge_enforced_by_admin = true, want false with no settings")
	}
	if m := payload3["effective_judge_model"]; m != settings.DefaultJudgeModel {
		t.Errorf("effective_judge_model = %v, want %q (default)", m, settings.DefaultJudgeModel)
	}
}

// sessionAppearance is the decoded appearance surface of a session payload (PRD
// #1167): resolved values + raw nullable overrides + instance defaults, plus the
// deprecated theme trio, all read via a JSON round-trip so the *string overrides
// decode cleanly.
type sessionAppearance struct {
	Appearance struct {
		Mode       string `json:"mode"`
		LightTheme string `json:"light_theme"`
		DarkTheme  string `json:"dark_theme"`
		Typeface   string `json:"typeface"`
		Overrides  struct {
			Mode       *string `json:"mode"`
			LightTheme *string `json:"light_theme"`
			DarkTheme  *string `json:"dark_theme"`
			Typeface   *string `json:"typeface"`
		} `json:"overrides"`
		Defaults struct {
			Mode       string `json:"mode"`
			LightTheme string `json:"light_theme"`
			DarkTheme  string `json:"dark_theme"`
			Typeface   string `json:"typeface"`
		} `json:"defaults"`
	} `json:"appearance"`
	Theme         string  `json:"theme"`
	ThemeOverride *string `json:"theme_override"`
	DefaultTheme  string  `json:"default_theme"`
}

func decodeSessionAppearance(t *testing.T, payload map[string]any) sessionAppearance {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var got sessionAppearance
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode payload %s: %v", raw, err)
	}
	return got
}

// TestSessionPayloadAppearance pins the PRD #1167 appearance surface: each field
// resolves user-value-wins over the instance default, the raw overrides are the
// per-field nullable user values (an unset column ⇒ JSON null), and the deprecated
// trio is derived — theme is the painted-polarity slot (light slot under mode
// light), theme_override is the raw legacy override, default_theme is the instance
// dark default.
func TestSessionPayloadAppearance(t *testing.T) {
	h := newSettingsHandler(
		store.AppSetting{Key: settings.KeyDefaultAppearanceMode, Value: "system"},
		store.AppSetting{Key: settings.KeyDefaultLightTheme, Value: "shadow"},
		store.AppSetting{Key: settings.KeyDefaultDarkTheme, Value: "mission"},
		store.AppSetting{Key: settings.KeyDefaultTypeface, Value: "plex"},
	)
	user := store.User{
		AppearanceMode: pgtype.Text{String: "light", Valid: true},
		LightTheme:     pgtype.Text{String: "dawn", Valid: true},
		// DarkTheme unset ⇒ inherits the instance default "mission".
		Typeface: pgtype.Text{String: "system", Valid: true},
		Theme:    pgtype.Text{String: "ember", Valid: true},
	}
	got := decodeSessionAppearance(t, h.sessionPayload(context.Background(), user))

	// Resolved: user value wins per field; the unset dark slot inherits "mission".
	if got.Appearance.Mode != "light" || got.Appearance.LightTheme != "dawn" ||
		got.Appearance.DarkTheme != "mission" || got.Appearance.Typeface != "system" {
		t.Errorf("resolved appearance = %+v, want light/dawn/mission/system", got.Appearance)
	}
	// Overrides: the raw user values; the unset dark slot is JSON null.
	ov := got.Appearance.Overrides
	if ov.Mode == nil || *ov.Mode != "light" || ov.LightTheme == nil || *ov.LightTheme != "dawn" ||
		ov.Typeface == nil || *ov.Typeface != "system" {
		t.Errorf("overrides = %+v, want light/dawn/*/system", ov)
	}
	if ov.DarkTheme != nil {
		t.Errorf("dark_theme override = %q, want null (unset ⇒ inherit)", *ov.DarkTheme)
	}
	// Defaults: the instance values (dark = the legacy-chained DefaultDarkTheme).
	if got.Appearance.Defaults.Mode != "system" || got.Appearance.Defaults.LightTheme != "shadow" ||
		got.Appearance.Defaults.DarkTheme != "mission" || got.Appearance.Defaults.Typeface != "plex" {
		t.Errorf("defaults = %+v, want system/shadow/mission/plex", got.Appearance.Defaults)
	}
	// Deprecated trio: theme = painted light slot (mode light); theme_override = raw
	// legacy override; default_theme = the instance dark default.
	if got.Theme != "dawn" {
		t.Errorf("theme = %q, want dawn (painted light slot under mode light)", got.Theme)
	}
	if got.ThemeOverride == nil || *got.ThemeOverride != "ember" {
		t.Errorf("theme_override = %v, want ember (raw legacy override)", got.ThemeOverride)
	}
	if got.DefaultTheme != "mission" {
		t.Errorf("default_theme = %q, want mission (instance dark default)", got.DefaultTheme)
	}
}

// With no user overrides and no instance settings, the appearance resolves to the
// compiled-in fallbacks (mode dark, light hall, dark ember, typeface system) and
// the deprecated trio paints the dark slot (mode dark).
func TestSessionPayloadAppearanceFallbacks(t *testing.T) {
	h := newSettingsHandler()
	got := decodeSessionAppearance(t, h.sessionPayload(context.Background(), store.User{}))

	if got.Appearance.Mode != "dark" || got.Appearance.LightTheme != "hall" ||
		got.Appearance.DarkTheme != "ember" || got.Appearance.Typeface != "system" {
		t.Errorf("resolved fallback = %+v, want dark/hall/ember/system", got.Appearance)
	}
	ov := got.Appearance.Overrides
	if ov.Mode != nil || ov.LightTheme != nil || ov.DarkTheme != nil || ov.Typeface != nil {
		t.Errorf("overrides must all be null with nothing set, got %+v", ov)
	}
	if got.Appearance.Defaults.Mode != "dark" || got.Appearance.Defaults.LightTheme != "hall" ||
		got.Appearance.Defaults.DarkTheme != "ember" || got.Appearance.Defaults.Typeface != "system" {
		t.Errorf("default fallback = %+v, want dark/hall/ember/system", got.Appearance.Defaults)
	}
	if got.Theme != "ember" {
		t.Errorf("theme = %q, want ember (painted dark slot under mode dark)", got.Theme)
	}
	if got.ThemeOverride != nil {
		t.Errorf("theme_override = %q, want null with no legacy override", *got.ThemeOverride)
	}
	if got.DefaultTheme != "ember" {
		t.Errorf("default_theme = %q, want ember (compiled dark default)", got.DefaultTheme)
	}
}

func TestEmailDomainExtraction(t *testing.T) {
	cases := map[string]string{
		"alice@example.com":         "example.com",
		"Alice <alice@example.com>": "example.com", // display-name form
		"  bob@Example.COM  ":       "example.com", // padded + mixed case
		`"weird@local"@example.com`: "example.com", // quoted local part containing '@'
		"carol@sub.example.com":     "sub.example.com",
	}
	for raw, want := range cases {
		if got := parseDomain(t, raw); got != want {
			t.Errorf("domain of %q = %q, want %q", raw, got, want)
		}
	}
}

func TestEmailDomainAllowed(t *testing.T) {
	example := []string{"example.com"}
	multi := []string{"example.com", "example.org"}
	cases := []struct {
		addr    string
		allowed []string
		want    bool
	}{
		{"alice@example.com", nil, true},          // empty allowlist ⇒ all allowed
		{"alice@gmail.com", nil, true},            // empty allowlist ⇒ all allowed
		{"alice@example.com", example, true},      // exact
		{"alice@gmail.com", example, false},       // not on list
		{"alice@sub.example.com", example, false}, // no subdomain wildcard
		{"alice@example.org", multi, true},        // second entry
		{"alice@gmail.com", multi, false},         // neither
	}
	for _, tc := range cases {
		if got := emailDomainAllowed(tc.addr, tc.allowed); got != tc.want {
			t.Errorf("emailDomainAllowed(%q, %v) = %v, want %v", tc.addr, tc.allowed, got, tc.want)
		}
	}
}

// postRegister drives the Register handler with the given config and JSON body.
// The policy checks (disabled, domain) and the password-length check all return
// before any DB access, so a nil pool is never reached on these paths.
func postRegister(cfg config.Config, body string) *httptest.ResponseRecorder {
	h := &Handler{cfg: cfg}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Register(rec, req)
	return rec
}

func TestRegisterDisabledReturns403(t *testing.T) {
	rec := postRegister(config.Config{RegistrationEnabled: false},
		`{"email":"alice@example.com","password":"a-long-enough-password"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "registration is disabled") {
		t.Fatalf("body = %q, want the disabled message", rec.Body.String())
	}
}

func TestRegisterDomainRejectedReturns403(t *testing.T) {
	cfg := config.Config{RegistrationEnabled: true, PasswordLoginEnabled: true, AllowedEmailDomains: []string{"example.com"}}
	rec := postRegister(cfg, `{"email":"someone@gmail.com","password":"a-long-enough-password"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "example.com") {
		t.Fatalf("body = %q, want the allowed-domain named", rec.Body.String())
	}
}

// TestRegisterDomainAllowedPassesPolicy proves an allowed domain is NOT rejected:
// with a deliberately short password the handler falls through to the 400
// password check (not 403), so the domain gate must have passed. Case-insensitive
// matching is covered by the mixed-case address.
func TestRegisterDomainAllowedPassesPolicy(t *testing.T) {
	cfg := config.Config{RegistrationEnabled: true, PasswordLoginEnabled: true, AllowedEmailDomains: []string{"example.com"}}
	rec := postRegister(cfg, `{"email":"Alice@EXAMPLE.com","password":"short"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (past the domain gate, failing on password)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "restricted") {
		t.Fatalf("body = %q, an allowed domain must not be rejected", rec.Body.String())
	}
}

func TestRegisterEmptyAllowlistAllowsAll(t *testing.T) {
	cfg := config.Config{RegistrationEnabled: true, PasswordLoginEnabled: true} // nil AllowedEmailDomains
	rec := postRegister(cfg, `{"email":"anyone@gmail.com","password":"short"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (any domain allowed, failing on password)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "restricted") {
		t.Fatalf("body = %q, an empty allowlist must allow every domain", rec.Body.String())
	}
}

// TestLoginDisabledWhenPasswordLoginOff: with password login off, POST /login is a
// 403 even with well-formed, plausibly-valid credentials — the gate fires before the
// body and the DB, so there is no password backdoor in SSO-only mode (fact-check R1).
func TestLoginDisabledWhenPasswordLoginOff(t *testing.T) {
	h := &Handler{cfg: config.Config{PasswordLoginEnabled: false}}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"email":"admin@example.com","password":"a-plausible-password"}`))
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 with password login disabled", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "password login is disabled") {
		t.Errorf("body = %q, want the disabled reason", rec.Body.String())
	}
}

// TestLoginNotGatedWhenPasswordLoginOn: with the flag on, the gate does not fire —
// a malformed body reaches the decode step and 400s (a 403 here would mean the gate
// wrongly tripped). Proves the normal path is unchanged without needing a DB.
func TestLoginNotGatedWhenPasswordLoginOn(t *testing.T) {
	h := &Handler{cfg: config.Config{PasswordLoginEnabled: true}}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (gate off, failing on the malformed body)", rec.Code)
	}
}

func TestAuthConfigShape(t *testing.T) {
	t.Run("enabled with domains", func(t *testing.T) {
		h := &Handler{cfg: config.Config{RegistrationEnabled: true, AllowedEmailDomains: []string{"example.com"}}}
		rec := httptest.NewRecorder()
		h.AuthConfig(rec, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var got struct {
			RegistrationEnabled bool     `json:"registration_enabled"`
			AllowedEmailDomains []string `json:"allowed_email_domains"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
		}
		if !got.RegistrationEnabled {
			t.Error("registration_enabled should be true")
		}
		if len(got.AllowedEmailDomains) != 1 || got.AllowedEmailDomains[0] != "example.com" {
			t.Errorf("allowed_email_domains = %v, want [example.com]", got.AllowedEmailDomains)
		}
	})

	t.Run("disabled emits an empty JSON array, never null", func(t *testing.T) {
		h := &Handler{cfg: config.Config{RegistrationEnabled: false}} // nil AllowedEmailDomains
		rec := httptest.NewRecorder()
		h.AuthConfig(rec, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))
		body := rec.Body.String()
		if !strings.Contains(body, `"registration_enabled":false`) {
			t.Errorf("body = %q, want registration_enabled false", body)
		}
		if !strings.Contains(body, `"allowed_email_domains":[]`) {
			t.Errorf("body = %q, want an empty array (not null) for the domains", body)
		}
	})

	// PRD #45, Decision 9: the OIDC surface fields. Default (no OIDC) is dormant with
	// password login on; a configured OIDC-only deployment flips all three.
	t.Run("oidc dormant by default", func(t *testing.T) {
		h := &Handler{cfg: config.Config{RegistrationEnabled: true, PasswordLoginEnabled: true}}
		rec := httptest.NewRecorder()
		h.AuthConfig(rec, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))
		var got struct {
			OIDCEnabled          bool   `json:"oidc_enabled"`
			OIDCProviderName     string `json:"oidc_provider_name"`
			PasswordLoginEnabled bool   `json:"password_login_enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
		}
		if got.OIDCEnabled {
			t.Error("oidc_enabled should be false when unconfigured")
		}
		if !got.PasswordLoginEnabled {
			t.Error("password_login_enabled should default true")
		}
	})

	t.Run("oidc configured, password login off", func(t *testing.T) {
		h := &Handler{cfg: config.Config{
			OIDCIssuerURL:        "https://idp.example.com",
			OIDCProviderName:     "Keycloak",
			PasswordLoginEnabled: false,
		}}
		rec := httptest.NewRecorder()
		h.AuthConfig(rec, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))
		var got struct {
			OIDCEnabled          bool   `json:"oidc_enabled"`
			OIDCProviderName     string `json:"oidc_provider_name"`
			PasswordLoginEnabled bool   `json:"password_login_enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
		}
		if !got.OIDCEnabled {
			t.Error("oidc_enabled should be true when the issuer is set")
		}
		if got.OIDCProviderName != "Keycloak" {
			t.Errorf("oidc_provider_name = %q, want Keycloak", got.OIDCProviderName)
		}
		if got.PasswordLoginEnabled {
			t.Error("password_login_enabled should be false")
		}
	})
}
