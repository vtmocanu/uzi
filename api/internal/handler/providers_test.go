package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func providerJSONRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/me/providers", strings.NewReader(body))
}

func TestDecodeProviderBodyRefusals(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}
	cases := []struct {
		name, body string
		refusals   [][]providerBodyRefusal
		wantOK     bool
		wantCode   int
		wantSubstr string
	}{
		{"plain", `{"name":"x"}`, nil, true, 0, ""},
		{"key refused", `{"name":"x","key":"dummy-gateway-key-0001"}`, [][]providerBodyRefusal{providerKeyFieldRefusals}, false, 400, "PUT /api/me/providers/{id}/key"},
		{"api_key refused", `{"api_key":"dummy-gateway-key-0001"}`, [][]providerBodyRefusal{providerKeyFieldRefusals}, false, 400, "key"},
		{"base_url refused on patch", `{"name":"x","base_url":"https://gw.example.com/anthropic"}`, [][]providerBodyRefusal{providerEndpointFieldRefusals}, false, 400, "/endpoint"},
		{"auth_style refused on patch", `{"auth_style":"bearer"}`, [][]providerBodyRefusal{providerEndpointFieldRefusals}, false, 400, "/endpoint"},
		{"unknown field", `{"name":"x","extra":1}`, nil, false, 400, "invalid request body"},
		{"not an object", `[1]`, nil, false, 400, "invalid request body"},
		{"too large", `{"name":"` + strings.Repeat("a", maxProviderBodyBytes) + `"}`, nil, false, 413, "too large"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		var dst body
		ok := decodeProviderBody(rec, providerJSONRequest(c.body), &dst, c.refusals...)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v (%d %s)", c.name, ok, c.wantOK, rec.Code, rec.Body.String())
			continue
		}
		if !ok && (rec.Code != c.wantCode || !strings.Contains(rec.Body.String(), c.wantSubstr)) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, rec.Code, rec.Body.String(), c.wantCode, c.wantSubstr)
		}
		if !ok && strings.Contains(rec.Body.String(), "dummy-gateway-key-0001") {
			t.Errorf("%s: the refusal echoes the key", c.name)
		}
	}
}

func TestProviderDTOHidesKeyAndMapsModelMap(t *testing.T) {
	row := providerRow{
		Name: "corp", Engine: "claude", CurrentRevision: 3, Protocol: "anthropic_messages",
		BaseUrl: "https://gw.example.com/anthropic", AuthStyle: "x_api_key",
		ModelMap: []byte(`{"haiku":{"model":"h","context_window":8192},"fable":{"model":"f"}}`),
	}
	dto := providerDTO(row)
	if dto.HasKey || dto.KeyEnabled || dto.Probe != nil || dto.Retired {
		t.Errorf("keyless dto = %+v", dto)
	}
	if dto.ModelMap.Haiku.Model != "h" || dto.ModelMap.Haiku.ContextWindow == nil || *dto.ModelMap.Haiku.ContextWindow != 8192 ||
		dto.ModelMap.Fable == nil || dto.ModelMap.Fable.Model != "f" || dto.ModelMap.Opus != nil || dto.ModelMap.Sonnet != nil {
		t.Errorf("model map = %+v", dto.ModelMap)
	}
	row.KeySecretID = pgtype.UUID{Valid: true}
	row.KeyDisabledAt = pgtype.Timestamptz{Valid: true}
	row.RetiredAt = pgtype.Timestamptz{Valid: true}
	row.ProbeAuthResult = pgtype.Text{String: "ok", Valid: true}
	dto = providerDTO(row)
	if !dto.HasKey || dto.KeyEnabled || !dto.Retired || dto.Probe == nil || dto.Probe.AuthResult != "ok" {
		t.Errorf("keyed, disabled, retired dto = %+v", dto)
	}
}

func TestWriteProviderErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		reason string
	}{
		{errProviderNotFound, 404, ""},
		{errProviderRetired, 409, "provider_retired"},
		{errProviderCap, 409, "provider_limit_reached"},
		{errProviderNoKey, 404, ""},
		{&pgconn.PgError{Code: "23505", ConstraintName: "model_providers_owner_name_lower_key"}, 409, "provider_name_taken"},
		{errProviderBaseURLGone, 400, "provider_base_url_not_allowed"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if !writeProviderError(rec, c.err) || rec.Code != c.status || (c.reason != "" && !strings.Contains(rec.Body.String(), `"reason":"`+c.reason+`"`)) {
			t.Errorf("%v -> %d %s, want %d %s", c.err, rec.Code, rec.Body.String(), c.status, c.reason)
		}
	}
	rec := httptest.NewRecorder()
	if writeProviderError(rec, errors.New("boom")) || rec.Body.Len() != 0 {
		t.Error("an unknown error must be left to the caller")
	}
	fk := &pgconn.PgError{Code: "23503", ConstraintName: "users_claude_provider_fkey"}
	if !isPGForeignKeyViolation(fk, "users_claude_provider_fkey") || isPGForeignKeyViolation(fk, "other_fkey") || isPGForeignKeyViolation(errors.New("x"), "users_claude_provider_fkey") {
		t.Error("isPGForeignKeyViolation misclassifies")
	}
}

func TestProviderListDisabledWithoutAllowlist(t *testing.T) {
	h := &Handler{}
	if h.providersEnabled() {
		t.Error("providers enabled with no allowlist")
	}
	rec := httptest.NewRecorder()
	h.allowedProviderBaseURL(rec, "https://gw.example.com/anthropic")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"reason":"provider_base_url_not_allowed"`) {
		t.Errorf("feature-off URL check = %d %s", rec.Code, rec.Body.String())
	}
}

// Compile-time pin: the shared row alias must stay convertible from the list query, so the list and
// the single-row read build identical DTOs.
var _ = providerRow(store.ListVisibleProvidersRow{})
