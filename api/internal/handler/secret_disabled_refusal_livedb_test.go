package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// secret_disabled_refusal_livedb_test.go pins PRD #1732 D5/D12 for the writes that would
// PROMOTE a disabled credential: making it the default (Anthropic, Codex, and the legacy
// default-slot PUT) and opting it into the auto-select pool are each a 409 whose message is
// workersvc.ErrCredentialDisabled's "credential is disabled; enable it in Settings", the
// same advice a run or schedule naming a disabled credential gets. They must not answer
// "choose an enabled replacement", which is advice for disabling a default, not for this.
func TestSecretDisabledPromotionRefusalsLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	ctx := t.Context()
	want := workersvc.ErrCredentialDisabled.Error()
	check := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: code %d, want 409 (body %s)", name, rec.Code, rec.Body.String())
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode %s: %v", name, rec.Body.String(), err)
		}
		if body.Error != want {
			t.Fatalf("%s: error = %q, want %q", name, body.Error, want)
		}
	}

	user := mkSecretUser(t, pool)
	insertEnablementSecret(ctx, t, pool, user, "anthropic_token", "main", true)
	off := insertEnablementSecret(ctx, t, pool, user, "anthropic_token", "off", false).String()
	mustExecT(ctx, t, pool, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, off)

	rec := httptest.NewRecorder()
	h.PatchAnthropicToken(rec, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+off,
		`{"default":true}`, user, map[string]string{"id": off}))
	check("anthropic make-default", rec)

	rec = httptest.NewRecorder()
	h.PatchAnthropicTokenAutoEligible(rec, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+off+"/auto-eligible",
		`{"auto_eligible":true}`, user, map[string]string{"id": off}))
	check("anthropic pool opt-in", rec)

	insertEnablementSecret(ctx, t, pool, user, "codex_auth", "codex-main", true)
	codexOff := insertEnablementSecret(ctx, t, pool, user, "codex_auth", "codex-off", false).String()
	mustExecT(ctx, t, pool, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, codexOff)
	rec = httptest.NewRecorder()
	h.PatchCodexAuth(rec, userReq(http.MethodPatch, "/api/me/secrets/codex_auth/"+codexOff,
		`{"default":true}`, user, map[string]string{"id": codexOff}))
	check("codex make-default", rec)

	// The legacy PUT rotates the default slot; a disabled row holding that slot (seeded
	// directly, the handlers never leave one) is never rotated.
	legacy := mkSecretUser(t, pool)
	legacyDef := insertEnablementSecret(ctx, t, pool, legacy, "anthropic_token", "default", true)
	mustExecT(ctx, t, pool, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, legacyDef)
	rec = httptest.NewRecorder()
	h.PutAnthropicToken(rec, userReq(http.MethodPut, "/api/me/secrets/anthropic_token",
		`{"token":"tok-legacy-value"}`, legacy, nil))
	check("legacy PUT onto a disabled default", rec)
}
