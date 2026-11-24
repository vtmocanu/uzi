package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// Router-level coverage for the /api/me/providers routes (PRD #4320 M1), driven through the REAL
// h.Routes() router so a mis-mounted auth group is caught (issue #428's regression class). Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

const (
	testGatewayURL = "https://gw.example.com/anthropic"
	// A dummy key: 20 printable ASCII characters, nothing provider-token-shaped.
	testProviderKey = "dummy-gateway-key-0001"
)

type providerFixture struct {
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool
	user   uuid.UUID
	jwt    string
	uzc    string
}

func newProviderFixture(t *testing.T, allow ...string) providerFixture {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.cfg.ProviderAllowedBaseURLs = allow
	user := cliSeedUser(t, pool, false)
	return providerFixture{h: h, router: router, pool: pool, user: user,
		jwt: cliMintJWT(t, pool, user), uzc: cliMintToken(t, pool, user, clitoken.ScopeUser)}
}

func (f providerFixture) other(t *testing.T) providerFixture {
	t.Helper()
	u := cliSeedUser(t, f.pool, false)
	return providerFixture{h: f.h, router: f.router, pool: f.pool, user: u,
		jwt: cliMintJWT(t, f.pool, u), uzc: cliMintToken(t, f.pool, u, clitoken.ScopeUser)}
}

const validCreateBody = `{"name":"corp-gw","base_url":"` + testGatewayURL + `","auth_style":"bearer",` +
	`"model_map":{"opus":{"model":"corp-opus"},"sonnet":{"model":"corp-sonnet"},"haiku":{"model":"corp-haiku","context_window":200000}}}`

type providerEnvelope struct {
	Provider apitypes.ProviderDTO `json:"provider"`
}

// create posts validCreateBody over Bearer and returns the new provider.
func (f providerFixture) create(t *testing.T, body string) apitypes.ProviderDTO {
	t.Helper()
	rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers", f.uzc, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var env providerEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env.Provider
}

func providerReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Reason
}

func TestProviderCreateAndListLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	p := f.create(t, validCreateBody)
	if p.Name != "corp-gw" || p.Engine != "claude" || p.Protocol != "anthropic_messages" || p.BaseURL != testGatewayURL ||
		p.AuthStyle != "bearer" || p.CurrentRevision != 1 || p.Retired || p.HasKey || p.KeyEnabled || p.Probe != nil {
		t.Fatalf("created = %+v", p)
	}
	if p.ModelMap.Opus == nil || p.ModelMap.Opus.Model != "corp-opus" || p.ModelMap.Haiku.Model != "corp-haiku" ||
		p.ModelMap.Haiku.ContextWindow == nil || *p.ModelMap.Haiku.ContextWindow != 200000 || p.ModelMap.Fable != nil {
		t.Fatalf("model map = %+v", p.ModelMap)
	}
	// A trailing slash and upper-case host store the operator's allowlist entry verbatim.
	q := f.create(t, strings.Replace(strings.Replace(validCreateBody, "corp-gw", "second", 1), testGatewayURL, "HTTPS://GW.example.com/anthropic/", 1))
	if q.BaseURL != testGatewayURL {
		t.Errorf("stored base_url = %q, want the allowlist entry %q", q.BaseURL, testGatewayURL)
	}

	rec := bearerReqBody(f.router, http.MethodGet, "/api/me/providers", f.uzc, "")
	var list apitypes.ProviderListDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || !list.Enabled || len(list.Providers) != 2 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ciphertext") || strings.Contains(rec.Body.String(), testProviderKey) {
		t.Errorf("list leaks key material: %s", rec.Body.String())
	}
	if len(list.AllowedBaseURLs) != 1 || list.AllowedBaseURLs[0] != testGatewayURL {
		t.Errorf("allowed_base_urls = %v, want the operator entry %q", list.AllowedBaseURLs, testGatewayURL)
	}
	rec = bearerReqBody(f.router, http.MethodGet, "/api/me/providers/"+p.ID, f.uzc, "")
	if rec.Code != http.StatusOK {
		t.Errorf("get = %d", rec.Code)
	}

	// Duplicate name is a 409 with a code.
	rec = bearerReqBody(f.router, http.MethodPost, "/api/me/providers", f.uzc, validCreateBody)
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_name_taken" {
		t.Errorf("duplicate name = %d %s", rec.Code, rec.Body.String())
	}
}

func TestProviderBaseURLRefusalsLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	for _, bad := range []string{
		"https://gw.example.com/other",
		"https://gw.example.com/anthropic/../other",
		"https://gw.example.com.evil.test/anthropic",
		"https://gw.example.com/anthropic/v1",
		"https://gw.example.com",
		"http://gw.example.com/anthropic",
		"https://gw.example.com/anthro%70ic",
		"https://user@gw.example.com/anthropic",
		"https://gw.example.com/anthropic?x=1",
		"",
	} {
		body := strings.Replace(validCreateBody, testGatewayURL, bad, 1)
		rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers", f.uzc, body)
		if rec.Code != http.StatusBadRequest || providerReason(t, rec) != "provider_base_url_not_allowed" {
			t.Errorf("create with %q = %d %s, want 400 provider_base_url_not_allowed", bad, rec.Code, rec.Body.String())
		}
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM model_providers WHERE owner_user_id = $1`, f.user).Scan(&n); err != nil || n != 0 {
		t.Fatalf("providers created by refused requests = %d, %v", n, err)
	}

	// The same refusal on the endpoint edit, which leaves the stored destination unchanged.
	p := f.create(t, validCreateBody)
	rec := cookieReq(t, f.router, http.MethodPatch, "/api/me/providers/"+p.ID+"/endpoint", f.jwt, `{"base_url":"https://gw.example.com/other"}`)
	if rec.Code != http.StatusBadRequest || providerReason(t, rec) != "provider_base_url_not_allowed" {
		t.Errorf("endpoint to /other = %d %s", rec.Code, rec.Body.String())
	}
	rec = bearerReqBody(f.router, http.MethodGet, "/api/me/providers/"+p.ID, f.uzc, "")
	var env providerEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Provider.BaseURL != testGatewayURL || env.Provider.CurrentRevision != 1 {
		t.Errorf("provider after refused edit = %+v", env.Provider)
	}
}

func TestProviderFeatureOffLiveDB(t *testing.T) {
	f := newProviderFixture(t) // no allowlist
	rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers", f.uzc, validCreateBody)
	if rec.Code != http.StatusBadRequest || providerReason(t, rec) != "provider_base_url_not_allowed" {
		t.Errorf("create with the feature off = %d %s", rec.Code, rec.Body.String())
	}
	rec = bearerReqBody(f.router, http.MethodGet, "/api/me/providers", f.uzc, "")
	var list apitypes.ProviderListDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || list.Enabled || list.Providers == nil || len(list.Providers) != 0 {
		t.Errorf("list with the feature off = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"providers":[]`) {
		t.Errorf("providers must serialize as an empty array: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"allowed_base_urls":[]`) {
		t.Errorf("allowed_base_urls must serialize as an empty array: %s", rec.Body.String())
	}
}

func TestProviderCrossUserIs404LiveDB(t *testing.T) {
	a := newProviderFixture(t, testGatewayURL)
	b := a.other(t)
	p := a.create(t, validCreateBody)
	if rec := cookieReq(t, a.router, http.MethodPut, "/api/me/providers/"+p.ID+"/key", a.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set key = %d %s", rec.Code, rec.Body.String())
	}
	id := "/api/me/providers/" + p.ID
	for _, c := range []struct {
		method, path, body string
		cookie             bool
	}{
		{http.MethodGet, id, "", false},
		{http.MethodPatch, id, `{"name":"stolen"}`, false},
		{http.MethodPost, id + "/retire", "", false},
		{http.MethodPut, id + "/key", `{"key":"` + testProviderKey + `"}`, true},
		{http.MethodDelete, id + "/key", "", true},
		{http.MethodPatch, id + "/key/enabled", `{"enabled":false}`, true},
		{http.MethodPatch, id + "/endpoint", `{"auth_style":"x_api_key"}`, true},
		{http.MethodDelete, id, "", true},
	} {
		var rec *httptest.ResponseRecorder
		if c.cookie {
			rec = cookieReq(t, b.router, c.method, c.path, b.jwt, c.body)
		} else {
			rec = bearerReqBody(b.router, c.method, c.path, b.uzc, c.body)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as another user = %d %s, want 404", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// B's list does not show it; A's provider is untouched.
	rec := bearerReqBody(b.router, http.MethodGet, "/api/me/providers", b.uzc, "")
	var list apitypes.ProviderListDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Providers) != 0 {
		t.Errorf("B sees %d providers", len(list.Providers))
	}
	rec = bearerReqBody(a.router, http.MethodGet, id, a.uzc, "")
	var env providerEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Provider.Name != "corp-gw" || env.Provider.Retired || env.Provider.CurrentRevision != 1 || !env.Provider.HasKey || !env.Provider.KeyEnabled {
		t.Errorf("A's provider after B's attempts = %+v", env.Provider)
	}
}

func TestProviderPatchAndKeyValidationLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	p := f.create(t, validCreateBody)
	id := "/api/me/providers/" + p.ID

	// PATCH edits name and model_map only.
	for _, body := range []string{
		`{"base_url":"` + testGatewayURL + `"}`,
		`{"auth_style":"x_api_key"}`,
		`{"name":"x","base_url":"` + testGatewayURL + `"}`,
		`{"key":"` + testProviderKey + `"}`,
		`{}`,
		`{"unknown":1}`,
		`{"model_map":{"opus":{"model":"o"}}}`,
		`{"name":""}`,
	} {
		if rec := bearerReqBody(f.router, http.MethodPatch, id, f.uzc, body); rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400", body, rec.Code)
		}
	}
	// Create never accepts a key.
	rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers", f.uzc,
		strings.Replace(validCreateBody, `"name":"corp-gw",`, `"name":"keyed","key":"`+testProviderKey+`",`, 1))
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), testProviderKey) {
		t.Errorf("create with a key = %d %s, want 400 without echoing it", rec.Code, rec.Body.String())
	}

	// A model-map edit is a new revision; a rename is not.
	rec = bearerReqBody(f.router, http.MethodPatch, id, f.uzc, `{"model_map":{"haiku":{"model":"h2"}}}`)
	var env providerEnvelope
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil || env.Provider.CurrentRevision != 2 || env.Provider.ModelMap.Haiku.Model != "h2" || env.Provider.ModelMap.Opus != nil {
		t.Fatalf("model_map edit = %d %s", rec.Code, rec.Body.String())
	}
	rec = bearerReqBody(f.router, http.MethodPatch, id, f.uzc, `{"name":"renamed"}`)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil || env.Provider.Name != "renamed" || env.Provider.CurrentRevision != 2 {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body.String())
	}
	// The unreferenced first revision was pruned after the edit.
	var revs int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM model_provider_revisions WHERE provider_id = $1`, p.ID).Scan(&revs); err != nil || revs != 1 {
		t.Errorf("revisions after edit = %d, %v; want 1", revs, err)
	}
	// The endpoint edit (cookie-only) keeps the model map and bumps the revision.
	rec = cookieReq(t, f.router, http.MethodPatch, id+"/endpoint", f.jwt, `{"auth_style":"x_api_key"}`)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil || env.Provider.CurrentRevision != 3 || env.Provider.AuthStyle != "x_api_key" || env.Provider.ModelMap.Haiku.Model != "h2" {
		t.Fatalf("endpoint edit = %d %s", rec.Code, rec.Body.String())
	}

	// Key validation: too short and non-ASCII are refused without echoing the value.
	for name, key := range map[string]string{"7 chars": "abcdefg", "non-ascii": "dummy-gateway-key-é-0001", "inner space": "dummy gateway key 0001", "too long": strings.Repeat("k", 4097)} {
		rec := cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"`+key+`"}`)
		if rec.Code != http.StatusBadRequest || (len(key) > 8 && strings.Contains(rec.Body.String(), key)) {
			t.Errorf("%s: PUT key = %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	rec = cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil || !env.Provider.HasKey || !env.Provider.KeyEnabled {
		t.Fatalf("set key = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), testProviderKey) {
		t.Error("the key came back in the response")
	}
	// The sealed value is stored with the provider-key kind, not as plaintext.
	var kind string
	var def, auto bool
	var ct []byte
	var rev1 int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT kind, is_default, auto_eligible, ciphertext, enablement_rev FROM user_secrets WHERE user_id = $1 AND provider_id = $2`, f.user, p.ID).
		Scan(&kind, &def, &auto, &ct, &rev1); err != nil {
		t.Fatal(err)
	}
	if kind != "anthropic_compatible_key" || def || auto || strings.Contains(string(ct), testProviderKey) {
		t.Errorf("stored key = kind %q default %v auto %v plaintext-in-ciphertext %v", kind, def, auto, strings.Contains(string(ct), testProviderKey))
	}
	// Rotation reseals in place and bumps enablement_rev; disable and enable bump it again.
	if rec := cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"dummy-gateway-key-0002"}`); rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body.String())
	}
	var rev2 int64
	var rows int
	_ = f.pool.QueryRow(context.Background(), `SELECT enablement_rev, (SELECT count(*) FROM user_secrets WHERE provider_id = $2) FROM user_secrets WHERE user_id = $1 AND provider_id = $2`, f.user, p.ID).Scan(&rev2, &rows)
	if rev2 != rev1+1 || rows != 1 {
		t.Errorf("enablement_rev %d -> %d (rows %d), want +1 and one row", rev1, rev2, rows)
	}
	rec = cookieReq(t, f.router, http.MethodPatch, id+"/key/enabled", f.jwt, `{"enabled":false}`)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil || !env.Provider.HasKey || env.Provider.KeyEnabled {
		t.Fatalf("disable key = %d %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, f.router, http.MethodPatch, id+"/key/enabled", f.jwt, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("enabled missing = %d, want 400", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodDelete, id+"/key", f.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete key = %d", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodDelete, id+"/key", f.jwt, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete key = %d, want 404", rec.Code)
	}
}

func TestProviderRetireDeleteAndSourceLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	p := f.create(t, validCreateBody)
	id := "/api/me/providers/" + p.ID
	var env providerEnvelope

	rec := bearerReqBody(f.router, http.MethodPost, id+"/retire", f.uzc, "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil || !env.Provider.Retired {
		t.Fatalf("retire = %d %s", rec.Code, rec.Body.String())
	}
	// A retired provider is not edited.
	rec = bearerReqBody(f.router, http.MethodPatch, id, f.uzc, `{"name":"after"}`)
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_retired" {
		t.Errorf("edit of a retired provider = %d %s", rec.Code, rec.Body.String())
	}
	// Deleting a provider that is selected as a source is refused with a typed 409.
	cliMustExec(t, f.pool, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, f.user, p.ID)
	rec = cookieReq(t, f.router, http.MethodDelete, id, f.jwt, "")
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_in_use_as_source" {
		t.Fatalf("delete of a selected provider = %d %s, want 409 provider_in_use_as_source", rec.Code, rec.Body.String())
	}
	cliMustExec(t, f.pool, `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, f.user)
	if rec := cookieReq(t, f.router, http.MethodDelete, id, f.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReqBody(f.router, http.MethodGet, id, f.uzc, ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d", rec.Code)
	}
}

func TestProviderCapOf20LiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	for i := 0; i < 20; i++ {
		f.create(t, strings.Replace(validCreateBody, "corp-gw", fmt.Sprintf("p%02d", i), 1))
	}
	rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers", f.uzc, strings.Replace(validCreateBody, "corp-gw", "p21", 1))
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_limit_reached" {
		t.Errorf("21st provider = %d %s", rec.Code, rec.Body.String())
	}
	// Retiring does not free a slot.
	list := bearerReqBody(f.router, http.MethodGet, "/api/me/providers", f.uzc, "")
	var l apitypes.ProviderListDTO
	_ = json.Unmarshal(list.Body.Bytes(), &l)
	bearerReqBody(f.router, http.MethodPost, "/api/me/providers/"+l.Providers[0].ID+"/retire", f.uzc, "")
	rec = bearerReqBody(f.router, http.MethodPost, "/api/me/providers", f.uzc, strings.Replace(validCreateBody, "corp-gw", "p22", 1))
	if rec.Code != http.StatusConflict {
		t.Errorf("create after a retire = %d, want 409 (retired providers count)", rec.Code)
	}
}

// TestProviderRouteAuthMatrixLiveDB is the differential-auth proof: the key and destination
// routes are cookie-only (a uzc_ Bearer is refused and changes nothing), the others accept Bearer.
func TestProviderRouteAuthMatrixLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	p := f.create(t, validCreateBody)
	id := "/api/me/providers/" + p.ID
	if rec := cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed key = %d", rec.Code)
	}

	var keyRev0 int64
	if err := f.pool.QueryRow(context.Background(), `SELECT enablement_rev FROM user_secrets WHERE provider_id = $1`, p.ID).Scan(&keyRev0); err != nil {
		t.Fatal(err)
	}
	cookieOnly := []struct{ method, path, body string }{
		{http.MethodPut, id + "/key", `{"key":"dummy-gateway-key-9999"}`},
		{http.MethodDelete, id + "/key", ""},
		{http.MethodPatch, id + "/key/enabled", `{"enabled":false}`},
		{http.MethodPatch, id + "/endpoint", `{"auth_style":"x_api_key"}`},
		{http.MethodDelete, id, ""},
	}
	for _, c := range cookieOnly {
		for label, token := range map[string]string{"uzc_ bearer": f.uzc, "no credential": ""} {
			rec := bearerReqBody(f.router, c.method, c.path, token, c.body)
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with %s = %d, want 401/403", c.method, c.path, label, rec.Code)
			}
		}
	}
	// Nothing changed: the provider still exists with its key at revision 1 and the default style.
	var rev int32
	var style string
	var keys int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT p.current_revision, r.auth_style, (SELECT count(*) FROM user_secrets WHERE provider_id = p.id AND enablement_rev = $2 AND disabled_at IS NULL)
		   FROM model_providers p JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision WHERE p.id = $1`, p.ID, keyRev0).Scan(&rev, &style, &keys); err != nil {
		t.Fatalf("provider vanished after refused Bearer writes: %v", err)
	}
	if rev != 1 || style != "bearer" || keys != 1 {
		t.Errorf("after refused Bearer writes: revision %d style %s untouched keys %d", rev, style, keys)
	}

	// The cookie path reaches every cookie-only route.
	for _, c := range cookieOnly {
		path := c.path
		if c.method == http.MethodPut || c.method == http.MethodPatch {
			if rec := cookieReq(t, f.router, c.method, path, f.jwt, c.body); rec.Code >= 300 {
				t.Errorf("%s %s with a session = %d %s", c.method, path, rec.Code, rec.Body.String())
			}
		}
	}

	bearerOK := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/me/providers", "", http.StatusOK},
		{http.MethodGet, id, "", http.StatusOK},
		{http.MethodPatch, id, `{"name":"via-bearer"}`, http.StatusOK},
		{http.MethodPost, id + "/retire", "", http.StatusOK},
		{http.MethodPost, "/api/me/providers", strings.Replace(validCreateBody, "corp-gw", "via-bearer-2", 1), http.StatusCreated},
	}
	for _, c := range bearerOK {
		if rec := bearerReqBody(f.router, c.method, c.path, f.uzc, c.body); rec.Code != c.want {
			t.Errorf("%s %s with a uzc_ bearer = %d %s, want %d", c.method, c.path, rec.Code, rec.Body.String(), c.want)
		}
		if rec := bearerReqBody(f.router, c.method, c.path, "", c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential = %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

// TestProviderKeyIsInvisibleToGenericSecretRoutesLiveDB pins that the generic /me/secrets surface
// neither lists, tests nor mutates a provider key, so nothing can reseal or default it.
func TestProviderKeyIsInvisibleToGenericSecretRoutesLiveDB(t *testing.T) {
	for _, c := range []struct{ engine, kind, body string }{
		{"claude", "anthropic_compatible_key", validCreateBody},
		{"codex", "openai_compatible_key", `{"name":"codex-gateway","engine":"codex","protocol":"openai_responses","base_url":"` + testGatewayURL + `","auth_style":"bearer","model_map":{"default":{"model":"gateway-default"}}}`},
	} {
		t.Run(c.engine, func(t *testing.T) {
			f := newProviderFixture(t, testGatewayURL)
			p := f.create(t, c.body)
			if rec := cookieReq(t, f.router, http.MethodPut, "/api/me/providers/"+p.ID+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
				t.Fatalf("seed key = %d", rec.Code)
			}
			var keyID uuid.UUID
			if err := f.pool.QueryRow(context.Background(), `SELECT id FROM user_secrets WHERE provider_id = $1`, p.ID).Scan(&keyID); err != nil {
				t.Fatal(err)
			}
			var before []byte
			_ = f.pool.QueryRow(context.Background(), `SELECT ciphertext FROM user_secrets WHERE id = $1`, keyID).Scan(&before)
			nativeToken := seedTestConnection(t, f.h, f.pool, f.user, "anthropic_token", "fixture-native-token")
			nativeKey := seedTestConnection(t, f.h, f.pool, f.user, "openai_api_key", "fixture-native-key")

			// Provider keys are hidden while native credentials remain visible.
			rec := bearerReqBody(f.router, http.MethodGet, "/api/me/secrets", f.uzc, "")
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), keyID.String()) || strings.Contains(rec.Body.String(), c.kind) || !strings.Contains(rec.Body.String(), nativeToken.String()) || !strings.Contains(rec.Body.String(), nativeKey.String()) {
				t.Errorf("GET /me/secrets = %d %s; the provider key must not be listed", rec.Code, rec.Body.String())
			}
			// The generic connection test 404s for the kind, as a Bearer and with a session.
			testPath := "/api/me/secrets/" + c.kind + "/" + keyID.String() + "/test"
			if rec := bearerReqBody(f.router, http.MethodPost, testPath, f.uzc, ""); rec.Code != http.StatusNotFound {
				t.Errorf("generic test of a provider key = %d %s, want 404", rec.Code, rec.Body.String())
			}
			// Generic mutations addressed at the key's id are 404s that leave the sealed value untouched.
			for _, c := range []struct{ method, path, body string }{
				{http.MethodPatch, "/api/me/secrets/anthropic_token/" + keyID.String(), `{"token":"dummy-anthropic-token-1234"}`},
				{http.MethodPatch, "/api/me/secrets/anthropic_token/" + keyID.String(), `{"default":true}`},
				{http.MethodDelete, "/api/me/secrets/anthropic_token/" + keyID.String(), ""},
				{http.MethodPatch, "/api/me/secrets/anthropic_token/" + keyID.String() + "/auto-eligible", `{"auto_eligible":true}`},
				{http.MethodPatch, "/api/me/secrets/" + c.kind + "/" + keyID.String() + "/enabled", `{"enabled":false}`},
				{http.MethodGet, "/api/me/secrets/" + c.kind + "/" + keyID.String() + "/dependents", ""},
			} {
				var rec *httptest.ResponseRecorder
				if strings.HasSuffix(c.path, "/auto-eligible") {
					rec = bearerReqBody(f.router, c.method, c.path, f.uzc, c.body)
				} else {
					rec = cookieReq(t, f.router, c.method, c.path, f.jwt, c.body)
				}
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s %s = %d %s, want 404", c.method, c.path, rec.Code, rec.Body.String())
				}
			}
			var after []byte
			var isDefault, auto bool
			if err := f.pool.QueryRow(context.Background(), `SELECT ciphertext, is_default, auto_eligible FROM user_secrets WHERE id = $1`, keyID).Scan(&after, &isDefault, &auto); err != nil {
				t.Fatalf("provider key gone after generic mutations: %v", err)
			}
			if string(after) != string(before) || isDefault || auto {
				t.Error("a generic secret route changed the provider key")
			}
			// The generic create route does not exist for the kind.
			if rec := cookieReq(t, f.router, http.MethodPost, "/api/me/secrets/"+c.kind, f.jwt, `{"token":"dummy-anthropic-token-1234","label":"x"}`); rec.Code < 400 {
				t.Errorf("POST /me/secrets/%s = %d, want a refusal", c.kind, rec.Code)
			}
		})
	}
}

// An operator who removes an allowlist entry stops later saves of providers that still carry it:
// the base URL a revision edit or a key save carries is re-checked, not only a newly requested one.
func TestProviderSavesRecheckAllowlistLiveDB(t *testing.T) {
	const otherURL = "https://gw2.example.com/anthropic"
	f := newProviderFixture(t, testGatewayURL, otherURL)
	p := f.create(t, validCreateBody)
	id := "/api/me/providers/" + p.ID
	f.h.cfg.ProviderAllowedBaseURLs = []string{otherURL}

	check := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusBadRequest || providerReason(t, rec) != "provider_base_url_not_allowed" {
			t.Errorf("%s = %d %s, want 400 provider_base_url_not_allowed", name, rec.Code, rec.Body.String())
		}
	}
	check("PATCH model_map", bearerReqBody(f.router, http.MethodPatch, id, f.uzc, `{"model_map":{"haiku":{"model":"h2"}}}`))
	check("PATCH endpoint auth_style only", cookieReq(t, f.router, http.MethodPatch, id+"/endpoint", f.jwt, `{"auth_style":"x_api_key"}`))
	check("PUT key", cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`))

	var revs, keys int
	_ = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM model_provider_revisions WHERE provider_id = $1), (SELECT count(*) FROM user_secrets WHERE provider_id = $1)`, p.ID).Scan(&revs, &keys)
	if revs != 1 || keys != 0 {
		t.Errorf("refused saves wrote revisions=%d keys=%d, want 1 and 0", revs, keys)
	}
	// A rename changes no destination and stays allowed; moving to an allowed URL is the remedy.
	if rec := bearerReqBody(f.router, http.MethodPatch, id, f.uzc, `{"name":"renamed"}`); rec.Code != http.StatusOK {
		t.Errorf("rename = %d %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, f.router, http.MethodPatch, id+"/endpoint", f.jwt, `{"base_url":"`+otherURL+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("move to allowed URL = %d %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, f.router, http.MethodPut, id+"/key", f.jwt, `{"key":"`+testProviderKey+`"}`); rec.Code != http.StatusOK {
		t.Errorf("PUT key after move = %d %s", rec.Code, rec.Body.String())
	}
}

func TestProviderNameUniqueCaseInsensitiveLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	f.create(t, strings.Replace(validCreateBody, "corp-gw", "Corp", 1))
	rec := bearerReqBody(f.router, http.MethodPost, "/api/me/providers", f.uzc, strings.Replace(validCreateBody, "corp-gw", "corp", 1))
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_name_taken" {
		t.Errorf("create corp after Corp = %d %s, want 409 provider_name_taken", rec.Code, rec.Body.String())
	}
	// Another owner may reuse the name, and a rename onto a taken name collides too.
	o := f.other(t)
	o.create(t, strings.Replace(validCreateBody, "corp-gw", "corp", 1))
	second := f.create(t, strings.Replace(validCreateBody, "corp-gw", "second", 1))
	rec = bearerReqBody(f.router, http.MethodPatch, "/api/me/providers/"+second.ID, f.uzc, `{"name":"CORP"}`)
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_name_taken" {
		t.Errorf("rename to CORP = %d %s, want 409 provider_name_taken", rec.Code, rec.Body.String())
	}
}
