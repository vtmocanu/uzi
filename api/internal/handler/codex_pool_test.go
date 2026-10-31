package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3719 unit coverage that needs no database: the refusals that fire before any store access.

func TestCodexPoolHandlersRequireAuth(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.SetCodexAccountMode(rec, httptest.NewRequest(http.MethodPut, "/api/me/codex-account-mode", strings.NewReader(`{"codex_account_mode":"auto"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("SetCodexAccountMode unauthenticated = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.PatchCodexAuthAutoEligible(rec, httptest.NewRequest(http.MethodPatch, "/api/me/secrets/codex_auth/"+uuid.NewString()+"/auto-eligible", strings.NewReader(`{"auto_eligible":true}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("PatchCodexAuthAutoEligible unauthenticated = %d, want 401", rec.Code)
	}
}

func TestSetCodexAccountModeRejectsBadBodyBeforeStore(t *testing.T) {
	h := &Handler{} // a nil store would panic if a bad body reached it
	for _, body := range []string{`{}`, `{"codex_account_mode":"pool"}`, `{"codex_account_mode":""}`, `{"codex_account_mode":null}`, `nope`} {
		rec := httptest.NewRecorder()
		h.SetCodexAccountMode(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/codex-account-mode", strings.NewReader(body))))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
	}
}

func TestGetMySettingsCarriesCodexAccountMode(t *testing.T) {
	h := &Handler{q: store.New(&fakeSettingsDB{})}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"codex_account_mode":"default"`) {
		t.Fatalf("status %d body %s, want 200 carrying codex_account_mode default", rec.Code, rec.Body)
	}
}
