package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCodexProviderRoutesLiveDB(t *testing.T) {
	var hits atomic.Int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+testProviderKey {
			t.Errorf("gateway request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["model"]) != `"gateway-default"` {
			t.Error("probe model/body")
		}
		w.Header().Set("Content-Type", "application/json")
		if body["tools"] != nil {
			_, _ = w.Write([]byte(`{"object":"response","output":[{"type":"function_call","name":"uzi_probe","arguments":"{\"ok\":true}"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"object":"response","output":[]}`))
		}
	}))
	t.Cleanup(gw.Close)
	base := gw.URL + "/v1"
	f := newProviderFixture(t, base, testGatewayURL)
	f.h.cfg.ProviderAllowLoopback = true
	const expectedMap = `{"default":{"model":"gateway-default","context_window":200000},"gpt-6-astra":{"model":"gateway-astra","context_window":210000},"gpt-5.6-sol":{"model":"gateway-56-sol","context_window":220000},"gpt-6-sol":{"model":"gateway-sol","context_window":300000},"gpt-6.1-sol":{"model":"gateway-61-sol","context_window":310000},"gpt-6.2-sol":{"model":"gateway-62-sol","context_window":330000},"gpt-6.2-astra":{"model":"gateway-62-astra","context_window":340000},"gpt-6-luna":{"model":"gateway-luna","context_window":320000}}`
	assertMap := func(value any) {
		t.Helper()
		actual, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		if err := json.Unmarshal(actual, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(expectedMap), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("eight-slot model map roundtrip: got %s want %s", actual, expectedMap)
		}
	}
	p := f.create(t, `{"name":"codex","engine":"codex","protocol":"openai_responses","base_url":"`+base+`","auth_style":"bearer","model_map":`+expectedMap+`}`)
	assertMap(p.ModelMap)
	id := "/api/me/providers/" + p.ID
	rec := cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"dummy-gateway-key-0001"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("key: %d %s", rec.Code, rec.Body.String())
	}
	rec = bearerReqBody(f.router, http.MethodGet, id, f.uzc, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	var result providerEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	assertMap(result.Provider.ModelMap)
	raw, _ := json.Marshal(result.Provider.ModelMap)
	rec = bearerReqBody(f.router, http.MethodPatch, id, f.uzc, `{"model_map":`+string(raw)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	assertMap(result.Provider.ModelMap)
	rec = bearerReqBody(f.router, http.MethodGet, id, f.uzc, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get edited map: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	assertMap(result.Provider.ModelMap)
	rec = cookieReq(t, f.router, http.MethodPatch, id+"/endpoint", f.jwt, `{"auth_style":"x_api_key"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("auth: %d %s", rec.Code, rec.Body.String())
	}
	other := f.other(t)
	if r := bearerReqBody(other.router, http.MethodPost, id+"/test", other.uzc, ""); r.Code != http.StatusNotFound {
		t.Fatalf("foreign probe: %d %s", r.Code, r.Body.String())
	}
	if r := bearerReqBody(f.router, http.MethodPut, id+"/key", f.uzc, `{"key":"replacement-dummy-key"}`); r.Code != http.StatusUnauthorized {
		t.Fatalf("bearer key: %d %s", r.Code, r.Body.String())
	}
	rec = bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"auth_result":"ok"`) || !strings.Contains(rec.Body.String(), `"tool_result":"passed"`) || hits.Load() != 2 {
		t.Fatalf("probe: %d %s hits=%d", rec.Code, rec.Body.String(), hits.Load())
	}
	// A valid default must not hide an unsupported alias in the stored profile.
	var keyBefore, keyAfter []byte
	var revBefore, revAfter int64
	readKey := func(ciphertext *[]byte, revision *int64) {
		t.Helper()
		if err := f.pool.QueryRow(context.Background(), `SELECT ciphertext, enablement_rev FROM user_secrets WHERE user_id = $1 AND provider_id = $2`, f.user, p.ID).Scan(ciphertext, revision); err != nil {
			t.Fatal(err)
		}
	}
	readKey(&keyBefore, &revBefore)
	cliMustExec(t, f.pool, `UPDATE model_provider_revisions SET model_map = $2 WHERE provider_id = $1`, p.ID, `{"default":{"model":"gateway-default","context_window":200000},"haiku":{"model":"unsupported-claude-alias"}}`)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, id + "/test", ""},
		{http.MethodPut, id + "/key", `{"key":"replacement-dummy-key"}`},
	} {
		rec = cookieReq(t, f.router, route.method, route.path, f.jwt, route.body)
		if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != `{"error":"the provider profile is invalid"}` {
			t.Errorf("invalid profile %s: %d %s", route.path, rec.Code, rec.Body.String())
		}
	}
	readKey(&keyAfter, &revAfter)
	if hits.Load() != 2 || !bytes.Equal(keyBefore, keyAfter) || revBefore != revAfter {
		t.Errorf("invalid profile used key: hits=%d revision=%d -> %d ciphertext changed=%v", hits.Load(), revBefore, revAfter, !bytes.Equal(keyBefore, keyAfter))
	}
	cliMustExec(t, f.pool, `UPDATE model_provider_revisions SET model_map = $2 WHERE provider_id = $1`, p.ID, string(raw))
	claude := f.create(t, validCreateBody)
	f.setKey(t, claude.ID)
	// Wrong kind must fail before networking even though the provider still owns the row.
	cliMustExec(t, f.pool, `UPDATE user_secrets SET kind = 'anthropic_compatible_key' WHERE user_id = $1 AND provider_id = $2`, f.user, p.ID)
	rec = bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, "")
	if rec.Code != http.StatusNotFound || hits.Load() != 2 {
		t.Fatalf("wrong kind: %d %s hits=%d", rec.Code, rec.Body.String(), hits.Load())
	}
	readKey(&keyBefore, &revBefore)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPut, id + "/key", `{"key":"replacement-dummy-key"}`},
		{http.MethodPatch, id + "/key/enabled", `{"enabled":true}`},
	} {
		rec = cookieReq(t, f.router, route.method, route.path, f.jwt, route.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("wrong kind %s %s: %d %s", route.method, route.path, rec.Code, rec.Body.String())
		}
	}
	readKey(&keyAfter, &revAfter)
	if hits.Load() != 2 || !bytes.Equal(keyBefore, keyAfter) || revBefore != revAfter {
		t.Errorf("wrong kind mutated key: hits=%d revision=%d -> %d ciphertext changed=%v", hits.Load(), revBefore, revAfter, !bytes.Equal(keyBefore, keyAfter))
	}
	cliMustExec(t, f.pool, `UPDATE user_secrets SET kind = 'openai_compatible_key' WHERE user_id = $1 AND provider_id = $2`, f.user, p.ID)
	// A valid ciphertext sealed for the Claude kind cannot authenticate as a Codex key.
	cliMustExec(t, f.pool, `UPDATE user_secrets SET ciphertext = (SELECT ciphertext FROM user_secrets WHERE user_id = $1 AND provider_id = $2) WHERE user_id = $1 AND provider_id = $3`, f.user, claude.ID, p.ID)
	rec = bearerReqBody(f.router, http.MethodPost, id+"/test", f.uzc, "")
	if rec.Code != http.StatusInternalServerError || hits.Load() != 2 {
		t.Fatalf("wrong AAD: %d %s hits=%d", rec.Code, rec.Body.String(), hits.Load())
	}
	f.setKey(t, p.ID)

	if code, _, body := f.putSource(t, true, `{"provider_id":"`+claude.ID+`"}`); code != http.StatusOK {
		t.Fatalf("claude select: %d %s", code, body)
	}
	for _, engine := range []string{"claude", "codex"} {
		rec = bearerReqBody(f.router, http.MethodPut, "/api/me/providers/"+engine+"-source", f.uzc, `{"provider_id":"`+p.ID+`"}`)
		want := http.StatusOK
		if engine == "claude" {
			want = http.StatusNotFound
		}
		if rec.Code != want {
			t.Fatalf("select %s: %d %s", engine, rec.Code, rec.Body.String())
		}
	}
	if got := f.sourceInSettings(t); got == nil || *got != claude.ID {
		t.Fatalf("codex selection changed claude source: %v", got)
	}
	settings := bearerReqBody(f.router, http.MethodGet, "/api/me/settings", f.uzc, "")
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), `"codex_provider_id":"`+p.ID+`"`) {
		t.Fatalf("codex settings: %d %s", settings.Code, settings.Body.String())
	}
	rec = cookieReq(t, f.router, http.MethodDelete, id, f.jwt, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete selected: %d %s", rec.Code, rec.Body.String())
	}
	rec = bearerReqBody(f.router, http.MethodPut, "/api/me/providers/codex-source", f.uzc, `{"provider_id":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	settings = bearerReqBody(f.router, http.MethodGet, "/api/me/settings", f.uzc, "")
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), `"codex_provider_id":null`) {
		t.Fatalf("cleared codex settings: %d %s", settings.Code, settings.Body.String())
	}
	if got := f.sourceInSettings(t); got == nil || *got != claude.ID {
		t.Fatalf("codex clear changed claude source: %v", got)
	}
}
