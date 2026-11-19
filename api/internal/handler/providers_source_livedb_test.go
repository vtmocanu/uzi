package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Router-level coverage for PUT /api/me/providers/claude-source (PRD #4320 M4, plan decision P1:
// RequireUser, so a CLI Bearer works), driven through the REAL h.Routes() router. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func (f providerFixture) putSource(t *testing.T, bearer bool, body string) (int, string, string) {
	t.Helper()
	path := "/api/me/providers/claude-source"
	rec := cookieReq(t, f.router, http.MethodPut, path, f.jwt, body)
	if bearer {
		rec = bearerReqBody(f.router, http.MethodPut, path, f.uzc, body)
	}
	return rec.Code, providerReason(t, rec), rec.Body.String()
}

func (f providerFixture) sourceInSettings(t *testing.T) *string {
	t.Helper()
	rec := bearerReqBody(f.router, http.MethodGet, "/api/me/settings", f.uzc, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d %s", rec.Code, rec.Body.String())
	}
	var s struct {
		Settings struct {
			ClaudeProviderID *string `json:"claude_provider_id"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), `"claude_provider_id"`) {
		t.Fatalf("settings payload lacks claude_provider_id: %s", rec.Body.String())
	}
	return s.Settings.ClaudeProviderID
}

func (f providerFixture) setKey(t *testing.T, id string) {
	t.Helper()
	if rec := cookieReq(t, f.router, http.MethodPut, "/api/me/providers/"+id+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set key = %d %s", rec.Code, rec.Body.String())
	}
}

func TestClaudeSourceSetClearAndReadbackLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	p := f.create(t, validCreateBody)

	// A provider without a key is not usable: refused, nothing selected.
	code, reason, body := f.putSource(t, true, `{"provider_id":"`+p.ID+`"}`)
	if code != http.StatusConflict || reason != "provider_source_unusable" {
		t.Fatalf("keyless select = %d %s, want 409 provider_source_unusable", code, body)
	}
	if got := f.sourceInSettings(t); got != nil {
		t.Fatalf("a refused select left claude_provider_id = %s", *got)
	}

	f.setKey(t, p.ID)
	// Bearer (a CLI uzc_ token) and cookie are both accepted.
	if code, _, body = f.putSource(t, true, `{"provider_id":"`+p.ID+`"}`); code != http.StatusOK {
		t.Fatalf("bearer select = %d %s", code, body)
	}
	if got := f.sourceInSettings(t); got == nil || *got != p.ID {
		t.Fatalf("settings claude_provider_id = %v, want %s", got, p.ID)
	}
	if code, _, body = f.putSource(t, false, `{"provider_id":null}`); code != http.StatusOK {
		t.Fatalf("cookie clear = %d %s", code, body)
	}
	if got := f.sourceInSettings(t); got != nil {
		t.Fatalf("null did not clear: %s", *got)
	}
	if code, _, body = f.putSource(t, false, `{"provider_id":"`+p.ID+`"}`); code != http.StatusOK {
		t.Fatalf("cookie select = %d %s", code, body)
	}
}

func TestClaudeSourceRefusalsLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	foreign := f.other(t)
	theirs := foreign.create(t, validCreateBody)
	foreign.setKey(t, theirs.ID)

	// Foreign and unknown ids are 404 (no existence oracle), never a selection.
	for name, id := range map[string]string{"foreign": theirs.ID, "unknown": "11111111-1111-4111-8111-111111111111"} {
		if code, _, body := f.putSource(t, true, `{"provider_id":"`+id+`"}`); code != http.StatusNotFound {
			t.Errorf("%s select = %d %s, want 404", name, code, body)
		}
	}
	// Malformed and absent provider_id are 400.
	for _, b := range []string{`{}`, `{"provider_id":"nope"}`, `{"provider_id":7}`, `{"provider_id":null,"x":1}`} {
		if code, _, body := f.putSource(t, true, b); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", b, code, body)
		}
	}

	p := f.create(t, validCreateBody)
	f.setKey(t, p.ID)
	// A disabled key, a retired provider and a URL the operator removed are each refused.
	cliMustExec(t, f.pool, `UPDATE user_secrets SET disabled_at = now() WHERE user_id = $1 AND provider_id = $2`, f.user, p.ID)
	if code, reason, body := f.putSource(t, true, `{"provider_id":"`+p.ID+`"}`); code != http.StatusConflict || reason != "provider_source_unusable" || !strings.Contains(body, "disabled") {
		t.Errorf("disabled key = %d %s", code, body)
	}
	cliMustExec(t, f.pool, `UPDATE user_secrets SET disabled_at = NULL WHERE user_id = $1 AND provider_id = $2`, f.user, p.ID)
	f.h.cfg.ProviderAllowedBaseURLs = []string{"https://other.example.com/x"}
	if code, reason, body := f.putSource(t, true, `{"provider_id":"`+p.ID+`"}`); code != http.StatusConflict || reason != "provider_source_unusable" || !strings.Contains(body, "allowlist") {
		t.Errorf("removed URL = %d %s", code, body)
	}
	f.h.cfg.ProviderAllowedBaseURLs = []string{testGatewayURL}
	if rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers/"+p.ID+"/retire", f.uzc, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	if code, reason, body := f.putSource(t, true, `{"provider_id":"`+p.ID+`"}`); code != http.StatusConflict || reason != "provider_source_unusable" || !strings.Contains(body, "retired") {
		t.Errorf("retired = %d %s", code, body)
	}
	if got := f.sourceInSettings(t); got != nil {
		t.Errorf("refusals left claude_provider_id = %s", *got)
	}
}

// TestClaudeSourceBlocksDeleteAndRetireKeepsItLiveDB: the selected provider cannot be deleted
// (provider_in_use_as_source, the users_claude_provider_fkey RESTRICT), retiring leaves the source
// set rather than nulling it (the next run refuses provider_source_unusable).
func TestClaudeSourceBlocksDeleteAndRetireKeepsItLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	p := f.create(t, validCreateBody)
	f.setKey(t, p.ID)
	if code, _, body := f.putSource(t, true, `{"provider_id":"`+p.ID+`"}`); code != http.StatusOK {
		t.Fatalf("select = %d %s", code, body)
	}
	rec := cookieReq(t, f.router, http.MethodDelete, "/api/me/providers/"+p.ID, f.jwt, "")
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_in_use_as_source" {
		t.Fatalf("delete selected = %d %s, want 409 provider_in_use_as_source", rec.Code, rec.Body.String())
	}
	if rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers/"+p.ID+"/retire", f.uzc, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	if got := f.sourceInSettings(t); got == nil || *got != p.ID {
		t.Fatalf("retiring changed the source to %v, want it kept (never auto-nulled)", got)
	}
	// Clearing the source then lets the delete through.
	if code, _, body := f.putSource(t, true, `{"provider_id":null}`); code != http.StatusOK {
		t.Fatalf("clear = %d %s", code, body)
	}
	if rec := cookieReq(t, f.router, http.MethodDelete, "/api/me/providers/"+p.ID, f.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete after clear = %d %s", rec.Code, rec.Body.String())
	}
}
