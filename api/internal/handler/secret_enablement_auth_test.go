package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestSecretEnablementRouterAuthAndDisabledMetadataLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	token := cliMintToken(t, pool, owner, "user")
	anthropicID, codexID, foreignID := uuid.New(), uuid.New(), uuid.New()
	for _, row := range []struct {
		id, user    uuid.UUID
		kind, label string
		isDefault   bool
	}{
		{anthropicID, owner, "anthropic_token", "anthropic", true},
		{codexID, owner, "openai_api_key", "codex", true},
		{foreignID, other, "anthropic_token", "foreign", true},
	} {
		cliMustExec(t, pool, `INSERT INTO user_secrets
			(id, user_id, kind, label, is_default, ciphertext, sealed_with)
			VALUES ($1, $2, $3, $4, $5, 'x', 'master')`,
			row.id, row.user, row.kind, row.label, row.isDefault)
	}

	enabledPath := "/api/me/secrets/anthropic_token/" + anthropicID.String() + "/enabled"
	dependentsPath := "/api/me/secrets/anthropic_token/" + anthropicID.String() + "/dependents"
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPatch, enabledPath, `{"enabled":false}`},
		{http.MethodGet, dependentsPath, ""},
	} {
		rec := bearerReqBody(router, tc.method, tc.path, token, tc.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Bearer %s %s = %d, want 401: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		foreignPath := "/api/me/secrets/anthropic_token/" + foreignID.String()
		if tc.method == http.MethodPatch {
			foreignPath += "/enabled"
		} else {
			foreignPath += "/dependents"
		}
		rec = cookieReq(t, router, tc.method, foreignPath, session, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("foreign %s %s = %d, want 404: %s", tc.method, foreignPath, rec.Code, rec.Body.String())
		}
	}

	decodeSecret := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Secret map[string]any `json:"secret"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Secret == nil {
			t.Fatalf("decode secret: %v: %s", err, rec.Body.String())
		}
		return body.Secret
	}
	disabled := decodeSecret(cookieReq(t, router, http.MethodPatch, enabledPath, session, `{"enabled":false}`))
	if disabled["enabled"] != false || disabled["disabled_at"] == nil {
		t.Fatalf("disable response = %v", disabled)
	}
	if rec := cookieReq(t, router, http.MethodGet, dependentsPath, session, ""); rec.Code != http.StatusOK {
		t.Fatalf("cookie GET dependents = %d: %s", rec.Code, rec.Body.String())
	}
	codexDisabled := decodeSecret(cookieReq(t, router, http.MethodPatch,
		"/api/me/secrets/openai_api_key/"+codexID.String()+"/enabled", session, `{"enabled":false}`))
	if codexDisabled["enabled"] != false || codexDisabled["disabled_at"] == nil {
		t.Fatalf("disable Codex response = %v", codexDisabled)
	}
	for _, tc := range []struct {
		path, body string
		wantTime   any
	}{
		{"/api/me/secrets/anthropic_token/" + anthropicID.String(), `{"label":"renamed","token":"new-value"}`, disabled["disabled_at"]},
		{"/api/me/secrets/anthropic_token/" + anthropicID.String() + "/auto-eligible", `{"auto_eligible":false}`, disabled["disabled_at"]},
		{"/api/me/secrets/openai_api_key/" + codexID.String(), `{"label":"renamed codex"}`, codexDisabled["disabled_at"]},
	} {
		got := decodeSecret(cookieReq(t, router, http.MethodPatch, tc.path, session, tc.body))
		if got["enabled"] != false || got["disabled_at"] != tc.wantTime {
			t.Errorf("PATCH %s disabled metadata = %v, want disabled_at %v", tc.path, got, tc.wantTime)
		}
	}
}
