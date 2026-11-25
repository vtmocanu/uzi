package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Router-level coverage for admin-shared provider definitions (PRD #4324): the admin routes, the
// recipient routes that see a shared definition, the owner routes that must not, and the lock
// interleavings of select, edit, retire and delete. Driven through the REAL h.Routes() router. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

const (
	adminProvidersPath = "/api/admin/providers"
	sharedNilScope     = "00000000-0000-0000-0000-000000000000"
)

// sharedFx is one shared definition with three recipients and an admin. f is the first recipient (A).
type sharedFx struct {
	f      providerFixture // A: owns a private provider in some tests
	b, c   providerFixture
	admin  providerFixture // jwt is an admin session; uzc is unused
	uza    string          // admin-read token
	gw     *fakeProviderGateway
	base   string
	shared apitypes.AdminSharedProviderDTO
	// cgw and codexBase are the Responses fixture gateway of the Codex tests (useCodexGateway).
	cgw       *codexGateway
	codexBase string
}

func newSharedFx(t *testing.T, createShared bool) *sharedFx {
	t.Helper()
	f, gw, _ := probeFixture(t)
	s := &sharedFx{f: f, b: f.other(t), c: f.other(t), gw: gw, base: gw.srv.URL + "/anthropic"}
	adminID := cliSeedUser(t, f.pool, true)
	s.admin = providerFixture{h: f.h, router: f.router, pool: f.pool, user: adminID, jwt: cliMintJWT(t, f.pool, adminID)}
	s.uza = cliMintToken(t, f.pool, adminID, clitoken.ScopeAdminRO)
	if createShared {
		s.shared = s.createShared(t, "shared-"+uuid.NewString()[:8], "bearer")
	}
	return s
}

// personalBody is a personal-provider create body at the fixture's allowlisted loopback gateway.
func (s *sharedFx) personalBody(name string) string {
	return strings.Replace(strings.Replace(validCreateBody, testGatewayURL, s.base, 1), "corp-gw", name, 1)
}

func (s *sharedFx) sharedBody(name, auth string) string {
	return `{"name":"` + name + `","base_url":"` + s.base + `","auth_style":"` + auth + `",` +
		`"model_map":{"opus":{"model":"corp-opus"},"sonnet":{"model":"corp-sonnet"},"haiku":{"model":"corp-haiku","context_window":200000}}}`
}

// createShared posts through the admin route and registers cleanup that removes every trace of it.
func (s *sharedFx) createShared(t *testing.T, name, auth string) apitypes.AdminSharedProviderDTO {
	t.Helper()
	rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath, s.admin.jwt, s.sharedBody(name, auth))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create = %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Provider apitypes.AdminSharedProviderDTO `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	s.cleanupShared(t, env.Provider.ID)
	return env.Provider
}

// cleanupShared registers cleanup that removes every trace of a shared definition: both engines'
// selections, its keys and the row itself.
func (s *sharedFx) cleanupShared(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE claude_provider_id = $1`,
			`UPDATE users SET codex_provider_id = NULL, codex_provider_scope_id = NULL WHERE codex_provider_id = $1`,
			`DELETE FROM user_secrets WHERE provider_id = $1`,
			`DELETE FROM model_providers WHERE id = $1`,
		} {
			if _, err := s.f.pool.Exec(ctx, q, id); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
}

func (s *sharedFx) sharedPath() string { return "/api/me/providers/" + s.shared.ID }

func (s *sharedFx) adminGet(t *testing.T) apitypes.AdminSharedProviderDTO {
	t.Helper()
	return s.adminGetID(t, s.shared.ID)
}

func (s *sharedFx) adminGetID(t *testing.T, id string) apitypes.AdminSharedProviderDTO {
	t.Helper()
	rec := cookieReq(t, s.f.router, http.MethodGet, adminProvidersPath+"/"+id, s.admin.jwt, "")
	var env struct {
		Provider apitypes.AdminSharedProviderDTO `json:"provider"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil {
		t.Fatalf("admin get = %d %s", rec.Code, rec.Body.String())
	}
	return env.Provider
}

func (f providerFixture) putKeyFor(t *testing.T, providerID, key string) {
	t.Helper()
	if rec := cookieReq(t, f.router, http.MethodPut, "/api/me/providers/"+providerID+"/key", f.jwt, `{"key":"`+key+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("put key = %d %s", rec.Code, rec.Body.String())
	}
}

func (f providerFixture) getProvider(t *testing.T, id string) apitypes.ProviderDTO {
	t.Helper()
	rec := bearerReqBody(f.router, http.MethodGet, "/api/me/providers/"+id, f.uzc, "")
	var env providerEnvelope
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil {
		t.Fatalf("get provider = %d %s", rec.Code, rec.Body.String())
	}
	return env.Provider
}

func countRows(t *testing.T, s *sharedFx, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSharedProviderVisibilityLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	priv := s.f.create(t, s.personalBody("corp-gw"))
	own := s.b.create(t, s.personalBody("b-own-gw"))

	rec := bearerReqBody(s.b.router, http.MethodGet, "/api/me/providers", s.b.uzc, "")
	var list apitypes.ProviderListDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("B list = %d %s", rec.Code, rec.Body.String())
	}
	sawShared, sawOwn := false, false
	for _, p := range list.Providers {
		if p.ID == own.ID {
			sawOwn = true
			if p.CredentialMode != nil {
				t.Errorf("B's own provider has credential_mode %q", *p.CredentialMode)
			}
		}
		if p.ID == priv.ID {
			t.Errorf("B's list contains A's private provider")
		}
		if p.ID == s.shared.ID {
			sawShared = true
			if p.CredentialMode == nil || *p.CredentialMode != "per_user_key" || p.HasKey || p.Probe != nil {
				t.Errorf("shared provider as B sees it = %+v", p)
			}
		}
	}
	if !sawShared {
		t.Errorf("B's list lacks the shared definition: %s", rec.Body.String())
	}
	if !sawOwn || len(list.Providers) != 2 {
		t.Errorf("B's list = %d providers (own present %v), want exactly B's own and the shared one: %s", len(list.Providers), sawOwn, rec.Body.String())
	}
	// A personal provider has a null credential_mode on the wire.
	rec = bearerReqBody(s.f.router, http.MethodGet, "/api/me/providers/"+priv.ID, s.f.uzc, "")
	if !strings.Contains(rec.Body.String(), `"credential_mode":null`) {
		t.Errorf("personal provider credential_mode is not null: %s", rec.Body.String())
	}

	// A's private provider is a 404 for B on get, select, key PUT and Test.
	p := "/api/me/providers/" + priv.ID
	for _, c := range []struct {
		name, method, path, body string
		bearer                   bool
	}{
		{"get", http.MethodGet, p, "", true},
		{"select", http.MethodPut, "/api/me/providers/claude-source", `{"provider_id":"` + priv.ID + `"}`, true},
		{"key put", http.MethodPut, p + "/key", `{"key":"` + testProviderKey + `"}`, false},
		{"key delete", http.MethodDelete, p + "/key", "", false},
		{"key enable", http.MethodPatch, p + "/key/enabled", `{"enabled":false}`, false},
		{"test", http.MethodPost, p + "/test", "", true},
	} {
		var rec *httptest.ResponseRecorder
		if c.bearer {
			rec = bearerReqBody(s.b.router, c.method, c.path, s.b.uzc, c.body)
		} else {
			rec = cookieReq(t, s.b.router, c.method, c.path, s.b.jwt, c.body)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("B %s on A's private provider = %d %s, want 404", c.name, rec.Code, rec.Body.String())
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM user_secrets WHERE provider_id = $1`, priv.ID); n != 0 {
		t.Errorf("a refused key PUT left %d key row(s) on A's provider", n)
	}
	if got := s.b.sourceInSettings(t); got != nil {
		t.Errorf("a refused select left B's source = %s", *got)
	}
}

func TestSharedProviderAdminAuthMatrixLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	priv := s.f.create(t, s.personalBody("corp-gw"))
	one := adminProvidersPath + "/" + s.shared.ID
	writes := []struct{ method, path, body string }{
		{http.MethodPost, adminProvidersPath, s.sharedBody("denied-"+uuid.NewString()[:8], "bearer")},
		{http.MethodPatch, one, `{"name":"denied"}`},
		{http.MethodPost, one + "/retire", ""},
		{http.MethodDelete, one, ""},
	}
	for _, w := range writes {
		// uza_ and uzc_ Bearer tokens and no credential are refused before the handler.
		for label, token := range map[string]string{"uza_": s.uza, "uzc_": s.f.uzc, "none": ""} {
			rec := bearerReqBody(s.f.router, w.method, w.path, token, w.body)
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with %s = %d, want 401/403", w.method, w.path, label, rec.Code)
			}
			if label == "uza_" && rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with uza_ = %d, want 401", w.method, w.path, rec.Code)
			}
		}
		// A non-admin session is 403.
		if rec := cookieReq(t, s.f.router, w.method, w.path, s.b.jwt, w.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a non-admin session = %d, want 403", w.method, w.path, rec.Code)
		}
	}
	got := s.adminGet(t)
	if got.Name != s.shared.Name || got.Retired || got.CurrentRevision != 1 {
		t.Errorf("a refused admin write changed the definition: %+v", got)
	}

	// Reads: uza_ may list and get; a non-admin session and a user token may not.
	for _, path := range []string{adminProvidersPath, one} {
		if rec := bearerReqBody(s.f.router, http.MethodGet, path, s.uza, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s with uza_ = %d %s", path, rec.Code, rec.Body.String())
		}
		if rec := cookieReq(t, s.f.router, http.MethodGet, path, s.b.jwt, ""); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as a non-admin session = %d, want 403", path, rec.Code)
		}
		if rec := bearerReqBody(s.f.router, http.MethodGet, path, s.f.uzc, ""); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a user token = %d, want 401/403", path, rec.Code)
		}
	}
	rec := bearerReqBody(s.f.router, http.MethodGet, adminProvidersPath, s.uza, "")
	if strings.Contains(rec.Body.String(), "ciphertext") || strings.Contains(rec.Body.String(), `"key"`) {
		t.Errorf("admin list carries key state: %s", rec.Body.String())
	}
	// The list holds shared definitions only; a personal id is a 404 on admin get.
	if strings.Contains(rec.Body.String(), priv.ID) {
		t.Errorf("admin list contains a personal provider")
	}
	if rec := bearerReqBody(s.f.router, http.MethodGet, adminProvidersPath+"/"+priv.ID, s.uza, ""); rec.Code != http.StatusNotFound {
		t.Errorf("admin get of a personal provider = %d, want 404", rec.Code)
	}
	if rec := bearerReqBody(s.f.router, http.MethodGet, adminProvidersPath+"/"+uuid.NewString(), s.uza, ""); rec.Code != http.StatusNotFound {
		t.Errorf("admin get of an unknown id = %d, want 404", rec.Code)
	}
}

func TestSharedProviderOwnerRoutesRefuseSharedLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	sharedCodex := s.createSharedCodex(t, "owner-codex-"+uuid.NewString()[:8])
	for _, id := range []string{s.sharedPath(), "/api/me/providers/" + sharedCodex.ID} {
		for who, cred := range map[string]providerFixture{"recipient": s.b, "admin": s.admin} {
			cookie := func(method, path, body string) *httptest.ResponseRecorder {
				return cookieReq(t, s.f.router, method, path, cred.jwt, body)
			}
			for _, c := range []struct{ name, method, path, body string }{
				{"rename-only patch", http.MethodPatch, id, `{"name":"renamed"}`},
				{"model-map patch", http.MethodPatch, id, `{"model_map":{"haiku":{"model":"h2"}}}`},
				{"endpoint", http.MethodPatch, id + "/endpoint", `{"auth_style":"x_api_key"}`},
				{"retire", http.MethodPost, id + "/retire", ""},
				{"delete", http.MethodDelete, id, ""},
			} {
				if rec := cookie(c.method, c.path, c.body); rec.Code != http.StatusNotFound {
					t.Errorf("%s: owner route %s on %s = %d %s, want 404", who, c.name, id, rec.Code, rec.Body.String())
				}
			}
		}
	}
	got := s.adminGet(t)
	if got.Name != s.shared.Name || got.Retired || got.CurrentRevision != 1 || got.AuthStyle != "bearer" {
		t.Errorf("an owner route changed the shared definition: %+v", got)
	}
	if cur := s.adminGetID(t, sharedCodex.ID); cur.Name != sharedCodex.Name || cur.Retired || cur.CurrentRevision != 1 {
		t.Errorf("an owner route changed the shared Codex definition: %+v", cur)
	}
	// A shared Claude definition is not a Codex source (the cross-engine guard).
	if rec := cookieReq(t, s.f.router, http.MethodPut, "/api/me/providers/codex-source", s.b.jwt, `{"provider_id":"`+s.shared.ID+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("codex-source with a shared Claude id = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if n := countRows(t, s, `SELECT count(*) FROM users WHERE codex_provider_id IS NOT NULL AND id = $1`, s.b.user); n != 0 {
		t.Errorf("a refused codex select left a selection")
	}
}

func TestSharedProviderAdminCreateRefusalsLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	before := countRows(t, s, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`)
	post := func(body string) *httptest.ResponseRecorder {
		return cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath, s.admin.jwt, body)
	}
	name := "refusal-" + uuid.NewString()[:8]
	good := s.sharedBody(name, "bearer")

	// An engine that is neither claude nor codex is refused with no row.
	gemini := strings.Replace(good, `"name"`, `"engine":"gemini","name"`, 1)
	if rec := post(gemini); rec.Code != http.StatusBadRequest {
		t.Errorf("gemini create = %d %s, want 400", rec.Code, rec.Body.String())
	}
	// Bidi and ANSI names, an unallowlisted URL, and a key field are refused.
	for label, body := range map[string]string{
		"bidi name":      strings.Replace(good, name, `ab\u202ecd`, 1),
		"ansi name":      strings.Replace(good, name, `\u001b[31mred`, 1),
		"empty name":     strings.Replace(good, name, ``, 1),
		"base url":       strings.Replace(good, s.base, "https://evil.example.com/anthropic", 1),
		"key in body":    strings.Replace(good, `"name"`, `"key":"x","name"`, 1),
		"unknown auth":   strings.Replace(good, `"bearer"`, `"basic"`, 1),
		"missing haiku":  strings.Replace(good, `"haiku":{"model":"corp-haiku","context_window":200000}`, `"fable":{"model":"f"}`, 1),
		"unknown fields": strings.Replace(good, `"name"`, `"credential_mode":"operator_key","name"`, 1),
	} {
		if rec := post(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", label, rec.Code, rec.Body.String())
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`); n != before {
		t.Fatalf("a refused create wrote a row: %d -> %d", before, n)
	}

	// A first create works; the same name in another case is a 409 with a code.
	created := s.createShared(t, name, "bearer")
	if created.CredentialMode != "per_user_key" || created.Engine != "claude" || created.CurrentRevision != 1 || created.AttachedKeyCount != 0 {
		t.Errorf("created = %+v", created)
	}
	rec := post(s.sharedBody(strings.ToUpper(name), "bearer"))
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_name_taken" {
		t.Errorf("name collision = %d %s, want 409 provider_name_taken", rec.Code, rec.Body.String())
	}
	// A personal provider with the same name is unaffected: namespaces are separate.
	s.f.create(t, s.personalBody(name))
}

func TestSharedProviderCapCountsRetiredLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	have := countRows(t, s, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`)
	if have > maxSharedProviders {
		t.Fatalf("%d shared definitions already exist", have)
	}
	tag := uuid.NewString()[:8]
	if _, err := s.f.pool.Exec(context.Background(),
		`INSERT INTO model_providers (owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
		 SELECT NULL, 'cap-fill-' || $1::text || '-' || g, 'claude', 1, 'per_user_key', $3::uuid FROM generate_series(1, $2::int) g`,
		tag, maxSharedProviders-have, sharedNilScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.f.pool.Exec(context.Background(), `DELETE FROM model_providers WHERE owner_user_id IS NULL AND name LIKE $1`, "cap-fill-"+tag+"-%"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	post := func() *httptest.ResponseRecorder {
		return cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath, s.admin.jwt, s.sharedBody("over-cap-"+tag, "bearer"))
	}
	if rec := post(); rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_limit_reached" {
		t.Fatalf("create at the cap = %d %s, want 409 provider_limit_reached", rec.Code, rec.Body.String())
	}
	// Retiring does not free a slot.
	if _, err := s.f.pool.Exec(context.Background(), `UPDATE model_providers SET retired_at = now() WHERE owner_user_id IS NULL AND name LIKE $1`, "cap-fill-"+tag+"-%"); err != nil {
		t.Fatal(err)
	}
	if rec := post(); rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_limit_reached" {
		t.Fatalf("create at the cap with every filler retired = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if n := countRows(t, s, `SELECT count(*) FROM model_providers WHERE name = $1`, "over-cap-"+tag); n != 0 {
		t.Errorf("a refused create left a row")
	}
}

func TestSharedProviderAdminEditCopiesDestinationLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	s.shared = s.createShared(t, "edit-"+uuid.NewString()[:8], "x_api_key")
	path := adminProvidersPath + "/" + s.shared.ID
	patch := func(body string) *httptest.ResponseRecorder {
		return cookieReq(t, s.f.router, http.MethodPatch, path, s.admin.jwt, body)
	}
	// The destination is fixed at creation: any of these fields is a 400 and changes nothing.
	for _, body := range []string{
		`{"base_url":"https://other.example.com/x"}`,
		`{"auth_style":"bearer"}`,
		`{"engine":"codex"}`,
		`{"protocol":"openai_responses"}`,
		`{"name":"ok","base_url":"` + s.base + `"}`,
		`{"key":"x"}`,
	} {
		if rec := patch(body); rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d %s, want 400", body, rec.Code, rec.Body.String())
		}
	}
	if rec := patch(`{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty PATCH = %d, want 400", rec.Code)
	}
	for label, name := range map[string]string{"bidi": `a\u202eb`, "ansi": `\u001b[2Jx`} {
		if rec := patch(`{"name":"` + name + `"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("%s name PATCH = %d, want 400", label, rec.Code)
		}
	}
	if got := s.adminGet(t); got.CurrentRevision != 1 || got.Name != s.shared.Name {
		t.Fatalf("a refused edit changed the definition: %+v", got)
	}

	// A model-map edit makes revision 2 that copies base URL, auth style and protocol.
	rec := patch(`{"model_map":{"haiku":{"model":"h2"},"opus":{"model":"o2"}}}`)
	var env struct {
		Provider apitypes.AdminSharedProviderDTO `json:"provider"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &env) != nil {
		t.Fatalf("model-map edit = %d %s", rec.Code, rec.Body.String())
	}
	if p := env.Provider; p.CurrentRevision != 2 || p.BaseURL != s.base || p.AuthStyle != "x_api_key" || p.Protocol != "anthropic_messages" || p.ModelMap.Haiku.Model != "h2" {
		t.Fatalf("edited = %+v", p)
	}
	var base, auth, protocol string
	if err := s.f.pool.QueryRow(context.Background(),
		`SELECT base_url, auth_style, protocol FROM model_provider_revisions WHERE provider_id = $1 AND revision = 2`, s.shared.ID).Scan(&base, &auth, &protocol); err != nil {
		t.Fatal(err)
	}
	if base != s.base || auth != "x_api_key" || protocol != "anthropic_messages" {
		t.Errorf("revision 2 = %s %s %s, want the creation destination copied", base, auth, protocol)
	}
	// Rename alone keeps the revision; a case-insensitive collision with another shared name is a 409.
	other := s.createShared(t, "other-"+uuid.NewString()[:8], "bearer")
	rec = patch(`{"name":"` + strings.ToUpper(other.Name) + `"}`)
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_name_taken" {
		t.Errorf("rename collision = %d %s", rec.Code, rec.Body.String())
	}
	newName := "renamed-" + uuid.NewString()[:8]
	if rec = patch(`{"name":"` + newName + `"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body.String())
	}
	if got := s.adminGet(t); got.Name != newName || got.CurrentRevision != 2 {
		t.Errorf("after rename = %+v", got)
	}
	// A retired definition is not edited.
	if rec := cookieReq(t, s.f.router, http.MethodPost, path+"/retire", s.admin.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d %s", rec.Code, rec.Body.String())
	}
	if rec = patch(`{"name":"after-retire"}`); rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_retired" {
		t.Errorf("edit of a retired definition = %d %s", rec.Code, rec.Body.String())
	}
}

// providerAuditLog captures slog output for the duration of the test.
type providerAuditLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (a *providerAuditLog) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.Write(p)
}

func (a *providerAuditLog) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.String()
}

func captureProviderAudit(t *testing.T) *providerAuditLog {
	t.Helper()
	a := &providerAuditLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(a, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return a
}

func TestSharedProviderDeleteGuardAndCascadeLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	logs := captureProviderAudit(t)
	id := s.shared.ID
	s.b.putKeyFor(t, id, testProviderKey)
	s.c.putKeyFor(t, id, "dummy-gateway-key-cccc")
	// B tests (evidence row), selects.
	if rec := bearerReqBody(s.b.router, http.MethodPost, s.sharedPath()+"/test", s.b.uzc, ""); rec.Code != http.StatusOK {
		t.Fatalf("B test = %d %s", rec.Code, rec.Body.String())
	}
	if code, _, body := s.b.putSource(t, true, `{"provider_id":"`+id+`"}`); code != http.StatusOK {
		t.Fatalf("B select = %d %s", code, body)
	}
	if got := s.adminGet(t); got.SelectedUserCount != 1 || got.AttachedKeyCount != 2 {
		t.Fatalf("admin counts = %+v, want 1 selected and 2 keys", got)
	}
	del := func() *httptest.ResponseRecorder {
		return cookieReq(t, s.f.router, http.MethodDelete, adminProvidersPath+"/"+id, s.admin.jwt, "")
	}
	rec := del()
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_in_use_as_source" {
		t.Fatalf("delete of a selected definition = %d %s, want 409 provider_in_use_as_source", rec.Code, rec.Body.String())
	}
	if n := countRows(t, s, `SELECT count(*) FROM user_secrets WHERE provider_id = $1`, id); n != 2 {
		t.Fatalf("a refused delete removed keys: %d left", n)
	}
	// Retiring neither clears the selection nor lets the definition be deleted.
	if rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath+"/"+id+"/retire", s.admin.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	if got := s.b.sourceInSettings(t); got == nil || *got != id {
		t.Fatalf("retire cleared B's selection: %v", got)
	}
	if rec := del(); rec.Code != http.StatusConflict {
		t.Fatalf("delete of a retired selected definition = %d, want 409", rec.Code)
	}
	if code, _, body := s.b.putSource(t, true, `{"provider_id":null}`); code != http.StatusOK {
		t.Fatalf("clear = %d %s", code, body)
	}

	rec = del()
	var out struct {
		Deleted bool `json:"deleted"`
		Count   int  `json:"deleted_key_count"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.Deleted || out.Count != 2 {
		t.Fatalf("delete = %d %s, want 200 with deleted_key_count 2", rec.Code, rec.Body.String())
	}
	for _, q := range []string{
		`SELECT count(*) FROM model_providers WHERE id = $1`,
		`SELECT count(*) FROM user_secrets WHERE provider_id = $1`,
		`SELECT count(*) FROM model_provider_key_test_results WHERE provider_id = $1`,
		`SELECT count(*) FROM model_provider_revisions WHERE provider_id = $1`,
	} {
		if n := countRows(t, s, q, id); n != 0 {
			t.Errorf("%s = %d after delete, want 0", q, n)
		}
	}
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "provider_audit") && strings.Contains(l, "action=admin_delete") {
			line = l
		}
	}
	if !strings.Contains(line, "deleted_key_count=2") || !strings.Contains(line, "provider_id="+id) || !strings.Contains(line, "actor_id="+s.admin.user.String()) {
		t.Errorf("delete audit line = %q", line)
	}
	if strings.Contains(logs.String(), testProviderKey) {
		t.Errorf("the log carries key material")
	}
	if rec := del(); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
}

func TestSharedProviderRecipientKeyRoutesLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	id := s.shared.ID
	s.b.putKeyFor(t, id, testProviderKey)
	s.b.putKeyFor(t, id, "dummy-gateway-key-0002") // rotation through the visible lock path

	if got := s.b.getProvider(t, id); !got.HasKey || !got.KeyEnabled {
		t.Errorf("B after attach = %+v", got)
	}
	if got := s.c.getProvider(t, id); got.HasKey {
		t.Errorf("C sees B's key: %+v", got)
	}
	// The admin read carries counts and no key.
	got := s.adminGet(t)
	if got.AttachedKeyCount != 1 || got.SelectedUserCount != 0 {
		t.Errorf("admin counts = %+v", got)
	}
	var kind string
	var scope uuid.UUID
	if err := s.f.pool.QueryRow(context.Background(), `SELECT kind, provider_scope_id FROM user_secrets WHERE user_id = $1 AND provider_id = $2`, s.b.user, id).Scan(&kind, &scope); err != nil {
		t.Fatal(err)
	}
	if kind != "anthropic_compatible_key" || scope.String() != sharedNilScope {
		t.Errorf("B's key row = %s scope %s", kind, scope)
	}

	// Retired: key writes are refused, but disable, enable and delete stay available.
	if rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath+"/"+id+"/retire", s.admin.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	if rec := s.c.putKeyAttempt(t, id, testProviderKey); rec.Code == http.StatusOK {
		t.Errorf("key attach to a retired definition = %d, want a refusal", rec.Code)
	}
	if rec := cookieReq(t, s.f.router, http.MethodPatch, s.sharedPath()+"/key/enabled", s.b.jwt, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable on a retired definition = %d %s", rec.Code, rec.Body.String())
	}
	if got := s.b.getProvider(t, id); !got.HasKey || got.KeyEnabled || !got.Retired {
		t.Errorf("after disable = %+v", got)
	}
	if rec := cookieReq(t, s.f.router, http.MethodDelete, s.sharedPath()+"/key", s.b.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("key delete on a retired definition = %d %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, s.f.router, http.MethodDelete, s.sharedPath()+"/key", s.b.jwt, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second key delete = %d, want 404", rec.Code)
	}
	// A Test on a retired definition is refused.
	if rec := bearerReqBody(s.b.router, http.MethodPost, s.sharedPath()+"/test", s.b.uzc, ""); rec.Code != http.StatusConflict {
		t.Errorf("test on a retired definition = %d, want 409", rec.Code)
	}
}

func (f providerFixture) putKeyAttempt(t *testing.T, providerID, key string) *httptest.ResponseRecorder {
	t.Helper()
	return cookieReq(t, f.router, http.MethodPut, "/api/me/providers/"+providerID+"/key", f.jwt, `{"key":"`+key+`"}`)
}

func TestSharedProviderSelectionRefusalsLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	id := s.shared.ID
	// No key: the existing refusal with its key-missing text, nothing selected.
	code, reason, body := s.c.putSource(t, true, `{"provider_id":"`+id+`"}`)
	if code != http.StatusConflict || reason != "provider_source_unusable" || !strings.Contains(body, "no key") {
		t.Fatalf("keyless select = %d %s", code, body)
	}
	if got := s.c.sourceInSettings(t); got != nil {
		t.Fatalf("a refused select left source %s", *got)
	}
	s.b.putKeyFor(t, id, testProviderKey)
	if code, _, body := s.b.putSource(t, true, `{"provider_id":"`+id+`"}`); code != http.StatusOK {
		t.Fatalf("B select = %d %s", code, body)
	}
	var scope uuid.UUID
	if err := s.f.pool.QueryRow(context.Background(), `SELECT claude_provider_scope_id FROM users WHERE id = $1`, s.b.user).Scan(&scope); err != nil || scope.String() != sharedNilScope {
		t.Fatalf("B's source scope = %v %v", scope, err)
	}

	// A retire that committed first makes a later select fail with "retired" and leaves no selection.
	other := s.createShared(t, "retired-"+uuid.NewString()[:8], "bearer")
	s.c.putKeyFor(t, other.ID, testProviderKey)
	if rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath+"/"+other.ID+"/retire", s.admin.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	code, reason, body = s.c.putSource(t, true, `{"provider_id":"`+other.ID+`"}`)
	if code != http.StatusConflict || reason != "provider_source_unusable" || !strings.Contains(body, "retired") {
		t.Fatalf("select of a retired definition = %d %s", code, body)
	}
	if got := s.c.sourceInSettings(t); got != nil {
		t.Fatalf("a refused select left source %s", *got)
	}
}

// blockedRequest runs a prepared request on its own goroutine.
func blockedRequest(run func() *httptest.ResponseRecorder) chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- run() }()
	return done
}

func assertBlocked(t *testing.T, done chan *httptest.ResponseRecorder, what string) {
	t.Helper()
	select {
	case rec := <-done:
		t.Fatalf("%s completed (%d) while the lock was held", what, rec.Code)
	case <-time.After(500 * time.Millisecond):
	}
}

func awaitDone(t *testing.T, done chan *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-done:
		return rec
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never completed after the lock was released", what)
		return nil
	}
}

// SQL lock-conflict pin only: a test transaction takes the share lock itself (it does not drive the
// select route) and shows an admin retire or edit waits for it. The route-level fences are
// TestSharedProviderSelectWaitsForAdminHoldLiveDB and TestSharedProviderAdminWaitsForSelectLiveDB.
func TestSharedProviderShareLockConflictsWithAdminEditSQLPinLiveDB(t *testing.T) {
	for _, op := range []struct {
		name, method, suffix, body string
	}{
		{"retire", http.MethodPost, "/retire", ""},
		{"model map edit", http.MethodPatch, "", `{"model_map":{"haiku":{"model":"h3"}}}`},
		{"rename", http.MethodPatch, "", `{"name":"renamed-while-held"}`},
	} {
		t.Run(op.name, func(t *testing.T) {
			s := newSharedFx(t, true)
			ctx := context.Background()
			tx, err := s.f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			// What setMyProviderSource holds after its lock step: the share lock on the definition.
			if _, err := store.New(tx).LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: uuid.MustParse(s.shared.ID), OwnerUserID: s.b.user}); err != nil {
				t.Fatal(err)
			}
			done := blockedRequest(cookieReqPrepared(t, s.f.router, op.method, adminProvidersPath+"/"+s.shared.ID+op.suffix, s.admin.jwt, op.body))
			assertBlocked(t, done, "admin "+op.name)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if rec := awaitDone(t, done, "admin "+op.name); rec.Code != http.StatusOK {
				t.Fatalf("admin %s = %d %s", op.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// A select whose admin op holds the definition first waits for it (the real select route). The held op
// is a retire (the select then reads "retired", 409, and leaves no selection) or a rename, which takes
// FOR NO KEY UPDATE and so does NOT conflict with the users FK's KEY SHARE: only the route's own
// LockModelProviderForShare can make the select wait for it.
func TestSharedProviderSelectWaitsForAdminHoldLiveDB(t *testing.T) {
	for _, held := range []string{"retire", "rename"} {
		t.Run(held, func(t *testing.T) {
			s := newSharedFx(t, true)
			ctx := context.Background()
			s.b.putKeyFor(t, s.shared.ID, testProviderKey)
			tx, err := s.f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			q := store.New(tx)
			pid := uuid.MustParse(s.shared.ID)
			if _, err := q.LockSharedModelProviderForEdit(ctx, pid); err != nil {
				t.Fatal(err)
			}
			if held == "retire" {
				_, err = q.RetireSharedModelProvider(ctx, pid)
			} else {
				_, err = q.RenameSharedModelProvider(ctx, store.RenameSharedModelProviderParams{ID: pid, Name: "held-rename-" + uuid.NewString()[:8]})
			}
			if err != nil {
				t.Fatal(err)
			}
			done := blockedRequest(cookieReqPrepared(t, s.b.router, http.MethodPut, "/api/me/providers/claude-source", s.b.jwt, `{"provider_id":"`+s.shared.ID+`"}`))
			assertBlocked(t, done, "select")
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			rec := awaitDone(t, done, "select")
			if held == "retire" {
				if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_source_unusable" || !strings.Contains(rec.Body.String(), "retired") {
					t.Fatalf("select after a committed retire = %d %s, want 409 retired", rec.Code, rec.Body.String())
				}
				if got := s.b.sourceInSettings(t); got != nil {
					t.Fatalf("a refused select left source %s", *got)
				}
				var n int
				if err := s.f.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1 AND claude_provider_id IS NULL`, s.b.user).Scan(&n); err != nil || n != 1 {
					t.Fatalf("users.claude_provider_id not NULL: n=%d err=%v", n, err)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("select after a committed rename = %d %s, want 200", rec.Code, rec.Body.String())
			}
			if got := s.b.sourceInSettings(t); got == nil || *got != s.shared.ID {
				t.Fatalf("source = %v, want the shared definition", got)
			}
		})
	}
}

// afterShareLockTx wraps a real transaction and, right after the select route's
// LockModelProviderForShare statement ran, parks until released.
type afterShareLockTx struct {
	pgx.Tx
	held    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (x *afterShareLockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := x.Tx.QueryRow(ctx, sql, args...)
	if strings.Contains(sql, "-- name: LockModelProviderForShare ") {
		x.once.Do(func() {
			close(x.held)
			<-x.release
		})
	}
	return row
}

// A real select that already holds the share lock makes a real admin retire wait; the selection then
// commits and the source reads as retired.
func TestSharedProviderAdminWaitsForSelectLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	s.b.putKeyFor(t, s.shared.ID, testProviderKey)
	hook := &afterShareLockTx{held: make(chan struct{}), release: make(chan struct{})}
	s.f.h.secretBeginTx = func(c context.Context) (pgx.Tx, error) {
		tx, err := s.f.pool.BeginTx(c, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return nil, err
		}
		if hook.Tx == nil {
			hook.Tx = tx
			return hook, nil
		}
		return tx, nil
	}
	defer func() { s.f.h.secretBeginTx = nil }()
	var once sync.Once
	releaseSelect := func() { once.Do(func() { close(hook.release) }) }
	defer releaseSelect()

	sel := blockedRequest(cookieReqPrepared(t, s.b.router, http.MethodPut, "/api/me/providers/claude-source", s.b.jwt, `{"provider_id":"`+s.shared.ID+`"}`))
	select {
	case <-hook.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the select never reached its share lock")
	}
	retire := blockedRequest(cookieReqPrepared(t, s.f.router, http.MethodPost, adminProvidersPath+"/"+s.shared.ID+"/retire", s.admin.jwt, ""))
	assertBlocked(t, retire, "admin retire")
	releaseSelect()
	if rec := awaitDone(t, sel, "select"); rec.Code != http.StatusOK {
		t.Fatalf("select = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := awaitDone(t, retire, "admin retire"); rec.Code != http.StatusOK {
		t.Fatalf("admin retire = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := s.b.sourceInSettings(t); got == nil || *got != s.shared.ID {
		t.Fatalf("the selection did not persist: %v", got)
	}
	if got := s.b.getProvider(t, s.shared.ID); !got.Retired {
		t.Errorf("provider after retire = %+v, want retired", got)
	}
}

// Admin create, edit and retire each write one provider_audit line (actor and provider) and none carries
// key material.
func TestSharedProviderAdminAuditLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	logs := captureProviderAudit(t)
	s.shared = s.createShared(t, "audit-"+uuid.NewString()[:8], "bearer")
	s.b.putKeyFor(t, s.shared.ID, testProviderKey)
	path := adminProvidersPath + "/" + s.shared.ID
	if rec := cookieReq(t, s.f.router, http.MethodPatch, path, s.admin.jwt, `{"model_map":{"haiku":{"model":"h7"}}}`); rec.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, s.f.router, http.MethodPost, path+"/retire", s.admin.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d %s", rec.Code, rec.Body.String())
	}
	for _, action := range []string{"admin_create", "admin_edit", "admin_retire"} {
		want := fmt.Sprintf("action=%s actor_id=%s provider_id=%s", action, s.admin.user, s.shared.ID)
		found := false
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, "provider_audit") && strings.Contains(l, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no provider_audit line %q in:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), testProviderKey) {
		t.Errorf("the log carries key material")
	}
}

// A select racing an admin delete gets 404: the share lock waits for the delete and finds no row.
func TestSharedProviderSelectRacingDeleteIs404LiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	ctx := context.Background()
	s.b.putKeyFor(t, s.shared.ID, testProviderKey)
	tx, err := s.f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	pid := uuid.MustParse(s.shared.ID)
	if _, err := q.LockSharedModelProviderForDelete(ctx, pid); err != nil {
		t.Fatal(err)
	}
	done := blockedRequest(cookieReqPrepared(t, s.b.router, http.MethodPut, "/api/me/providers/claude-source", s.b.jwt, `{"provider_id":"`+s.shared.ID+`"}`))
	assertBlocked(t, done, "select")
	if _, err := q.DeleteSharedProviderKeys(ctx, pid); err != nil {
		t.Fatal(err)
	}
	if n, err := q.DeleteSharedModelProvider(ctx, pid); err != nil || n != 1 {
		t.Fatalf("delete = %d %v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := awaitDone(t, done, "select"); rec.Code != http.StatusNotFound {
		t.Fatalf("select racing a delete = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if got := s.b.sourceInSettings(t); got != nil {
		t.Fatalf("select left source %s", *got)
	}
}

// Admin paths take no per-user secret-mutation lock: with a recipient's lock held exclusively, an admin
// create, edit and retire still complete (a path that took it would cycle with the select order).
func TestSharedProviderAdminTakesNoPerUserLockLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	ctx := context.Background()
	tx, err := s.f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, u := range []uuid.UUID{s.b.user, s.admin.user} {
		if err := store.New(tx).LockSecretMutation(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct{ method, path, body string }{
		{http.MethodPost, adminProvidersPath, s.sharedBody("nolock-"+uuid.NewString()[:8], "bearer")},
		{http.MethodPatch, adminProvidersPath + "/" + s.shared.ID, `{"model_map":{"haiku":{"model":"h4"}}}`},
		{http.MethodPost, adminProvidersPath + "/" + s.shared.ID + "/retire", ""},
	}
	var created string
	for _, st := range steps {
		done := blockedRequest(cookieReqPrepared(t, s.f.router, st.method, st.path, s.admin.jwt, st.body))
		select {
		case rec := <-done:
			if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
				t.Fatalf("%s %s = %d %s", st.method, st.path, rec.Code, rec.Body.String())
			}
			if st.method == http.MethodPost && st.body != "" {
				var env struct {
					Provider apitypes.AdminSharedProviderDTO `json:"provider"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &env)
				created = env.Provider.ID
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s %s blocked on a per-user lock", st.method, st.path)
		}
	}
	if created != "" {
		done := blockedRequest(cookieReqPrepared(t, s.f.router, http.MethodDelete, adminProvidersPath+"/"+created, s.admin.jwt, ""))
		select {
		case rec := <-done:
			if rec.Code != http.StatusOK {
				t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("delete blocked on a per-user lock")
		}
	}
}

// Per-user Test evidence: B's success and C's failure are separate, and rotating B's key makes only
// B's result unknown.
func TestSharedProviderTestEvidenceIsPerUserLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	keyC := "dummy-gateway-key-cccc"
	s.gw.mu.Lock()
	s.gw.rejectAuth = map[string]bool{"Bearer " + keyC: true}
	s.gw.mu.Unlock()
	s.b.putKeyFor(t, s.shared.ID, testProviderKey)
	s.c.putKeyFor(t, s.shared.ID, keyC)
	test := s.sharedPath() + "/test"
	for _, u := range []struct {
		f    providerFixture
		want string
	}{{s.b, "ok"}, {s.c, "rejected"}} {
		var out struct {
			Probe *apitypes.ProviderProbeDTO `json:"probe"`
		}
		rec := bearerReqBody(u.f.router, http.MethodPost, test, u.f.uzc, "")
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Probe == nil || out.Probe.AuthResult != u.want {
			t.Fatalf("test = %d %s, want %s", rec.Code, rec.Body.String(), u.want)
		}
	}
	if got := s.b.getProvider(t, s.shared.ID); got.Probe == nil || got.Probe.AuthResult != "ok" {
		t.Errorf("B reads %+v", got.Probe)
	}
	if got := s.c.getProvider(t, s.shared.ID); got.Probe == nil || got.Probe.AuthResult != "rejected" {
		t.Errorf("C reads %+v", got.Probe)
	}
	if got := s.f.getProvider(t, s.shared.ID); got.Probe != nil || got.HasKey {
		t.Errorf("A (no key) reads %+v", got)
	}
	if n := countRows(t, s, `SELECT count(*) FROM model_provider_key_test_results WHERE provider_id = $1`, s.shared.ID); n != 2 {
		t.Errorf("evidence rows = %d, want one per user", n)
	}
	// Rotating B's key makes B's result unknown and leaves C's.
	s.b.putKeyFor(t, s.shared.ID, "dummy-gateway-key-0009")
	if got := s.b.getProvider(t, s.shared.ID); got.Probe != nil {
		t.Errorf("B's result survived a rotation: %+v", got.Probe)
	}
	if got := s.c.getProvider(t, s.shared.ID); got.Probe == nil || got.Probe.AuthResult != "rejected" {
		t.Errorf("C's result changed: %+v", got.Probe)
	}
	// Each user's listing shows only their own evidence.
	rec := bearerReqBody(s.c.router, http.MethodGet, "/api/me/providers", s.c.uzc, "")
	if strings.Contains(rec.Body.String(), testProviderKey) {
		t.Errorf("C's list carries B's key")
	}
}

// A rotation, revision bump or disable between the dial and the evidence write writes nothing and is a
// 409 (no lock spans the dial).
func TestSharedProviderEvidenceFenceAfterDialLiveDB(t *testing.T) {
	for _, change := range []string{"rotation", "admin model-map edit", "disable", "key delete", "retire"} {
		t.Run(change, func(t *testing.T) {
			s := newSharedFx(t, true)
			s.b.putKeyFor(t, s.shared.ID, testProviderKey)
			var prepared func() *httptest.ResponseRecorder
			switch change {
			case "rotation":
				prepared = cookieReqPrepared(t, s.b.router, http.MethodPut, s.sharedPath()+"/key", s.b.jwt, `{"key":"dummy-gateway-key-0002"}`)
			case "admin model-map edit":
				prepared = cookieReqPrepared(t, s.f.router, http.MethodPatch, adminProvidersPath+"/"+s.shared.ID, s.admin.jwt, `{"model_map":{"haiku":{"model":"h5"}}}`)
			case "disable":
				prepared = cookieReqPrepared(t, s.b.router, http.MethodPatch, s.sharedPath()+"/key/enabled", s.b.jwt, `{"enabled":false}`)
			case "key delete":
				prepared = cookieReqPrepared(t, s.b.router, http.MethodDelete, s.sharedPath()+"/key", s.b.jwt, "")
			case "retire":
				prepared = cookieReqPrepared(t, s.f.router, http.MethodPost, adminProvidersPath+"/"+s.shared.ID+"/retire", s.admin.jwt, "")
			}
			var once sync.Once
			mutated := make(chan *httptest.ResponseRecorder, 1)
			s.gw.mu.Lock()
			s.gw.onRequest = func() { once.Do(func() { mutated <- prepared() }) }
			s.gw.mu.Unlock()
			rec := bearerReqBody(s.b.router, http.MethodPost, s.sharedPath()+"/test", s.b.uzc, "")
			select {
			case mutation := <-mutated:
				if mutation.Code >= 300 {
					t.Fatalf("interleaved %s = %d %s", change, mutation.Code, mutation.Body.String())
				}
			default:
				t.Fatalf("the probe never dialed, so %s was not interleaved", change)
			}
			if rec.Code != http.StatusConflict {
				t.Errorf("test after a %s during the dial = %d %s, want 409", change, rec.Code, rec.Body.String())
			}
			if n := countRows(t, s, `SELECT count(*) FROM model_provider_key_test_results WHERE provider_id = $1`, s.shared.ID); n != 0 {
				t.Errorf("a late result wrote %d evidence row(s)", n)
			}
		})
	}
}

// Before the dial: a rotation, disable or admin edit that lands between the provider read and the key
// open refuses the Test and nothing reaches the gateway, for a shared definition too.
func TestSharedProviderProbeRefusesChangedSnapshotBeforeDialLiveDB(t *testing.T) {
	for _, change := range []string{"key only", "disable key", "admin model-map edit"} {
		t.Run(change, func(t *testing.T) {
			s := newSharedFx(t, true)
			s.b.putKeyFor(t, s.shared.ID, testProviderKey)
			interleaved := false
			hook := &providerProbeOpenInterleave{DBTX: s.f.pool, before: func() {
				interleaved = true
				var rec *httptest.ResponseRecorder
				switch change {
				case "key only":
					rec = s.b.putKeyAttempt(t, s.shared.ID, "dummy-gateway-key-0002")
				case "disable key":
					rec = cookieReq(t, s.f.router, http.MethodPatch, s.sharedPath()+"/key/enabled", s.b.jwt, `{"enabled":false}`)
				default:
					rec = cookieReq(t, s.f.router, http.MethodPatch, adminProvidersPath+"/"+s.shared.ID, s.admin.jwt, `{"model_map":{"haiku":{"model":"h6"}}}`)
				}
				if rec.Code != http.StatusOK {
					t.Errorf("interleaved %s = %d %s", change, rec.Code, rec.Body.String())
				}
			}}
			orig := s.f.h.q
			s.f.h.q = store.New(hook)
			defer func() { s.f.h.q = orig }()
			rec := bearerReqBody(s.b.router, http.MethodPost, s.sharedPath()+"/test", s.b.uzc, "")
			if !interleaved {
				t.Fatal("the probe never reached the key-open interleaving")
			}
			if rec.Code != http.StatusConflict {
				t.Errorf("probe = %d %s, want 409", rec.Code, rec.Body.String())
			}
			if n := len(s.gw.seenAuth()); n != 0 {
				t.Errorf("gateway received %d request(s) after the snapshot changed", n)
			}
		})
	}
}

func TestSharedProviderPersonalAuditLiveDB(t *testing.T) {
	f := newProviderFixture(t, testGatewayURL)
	logs := captureProviderAudit(t)
	p := f.create(t, validCreateBody)
	path := "/api/me/providers/" + p.ID
	if rec := bearerReqBody(f.router, http.MethodPatch, path, f.uzc, `{"name":"renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d", rec.Code)
	}
	if rec := bearerReqBody(f.router, http.MethodPost, path+"/retire", f.uzc, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodDelete, path, f.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	for _, action := range []string{"create", "edit", "retire", "delete"} {
		want := fmt.Sprintf("action=%s actor_id=%s provider_id=%s", action, f.user, p.ID)
		found := false
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, "provider_audit") && strings.Contains(l, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no provider_audit line for %q in:\n%s", want, logs.String())
		}
	}
}

func TestSharedProviderKeyPutWaitsForRetirementLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	ctx := context.Background()
	tx, err := s.f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if rows, err := store.New(tx).RetireSharedModelProvider(ctx, uuid.MustParse(s.shared.ID)); err != nil || rows != 1 {
		t.Fatalf("retire inside transaction: rows=%d, err=%v", rows, err)
	}

	put := cookieReqPrepared(t, s.b.router, http.MethodPut, s.sharedPath()+"/key", s.b.jwt, `{"key":"`+testProviderKey+`"}`)
	done := blockedRequest(put)
	assertBlocked(t, done, "key PUT behind retirement")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := awaitDone(t, done, "key PUT"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "provider changed while sealing") {
		t.Fatalf("key PUT after retirement = %d %s, want the existing 400 refusal", rec.Code, rec.Body.String())
	}
	if got := s.b.getProvider(t, s.shared.ID); !got.Retired || got.HasKey {
		t.Fatalf("retired definition after refused PUT: retired=%v, has_key=%v", got.Retired, got.HasKey)
	}
}

type providerProbeRereadInterleave struct {
	store.DBTX
	reads           int
	beforeFinalRead func()
}

func (db *providerProbeRereadInterleave) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: GetVisibleProviderForUse ") {
		db.reads++
		if db.reads == 3 { // The third read follows the evidence transaction's commit.
			db.beforeFinalRead()
		}
	}
	return db.DBTX.QueryRow(ctx, sql, args...)
}

func TestSharedProviderProbeRejectsRetirementAfterEvidenceCommitLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	s.b.putKeyFor(t, s.shared.ID, testProviderKey)
	ctx := context.Background()
	var keyID uuid.UUID
	var keyRev int64
	if err := s.f.pool.QueryRow(ctx, `SELECT id, enablement_rev FROM user_secrets WHERE user_id = $1 AND provider_id = $2`,
		s.b.user, s.shared.ID).Scan(&keyID, &keyRev); err != nil {
		t.Fatal(err)
	}
	retire := cookieReqPrepared(t, s.admin.router, http.MethodPost, adminProvidersPath+"/"+s.shared.ID+"/retire", s.admin.jwt, "")
	hook := &providerProbeRereadInterleave{DBTX: s.f.pool, beforeFinalRead: func() {
		if n := countRows(t, s, `SELECT count(*) FROM model_provider_key_test_results WHERE provider_id = $1`, s.shared.ID); n != 1 {
			t.Fatalf("evidence rows before interleaved retirement = %d, want 1", n)
		}
		var owner, storedKey uuid.UUID
		var revision int
		var storedKeyRev int64
		var auth, tool string
		if err := s.f.pool.QueryRow(ctx, `SELECT user_id, revision, key_secret_id, key_enablement_rev, auth_result, tool_result
			FROM model_provider_key_test_results WHERE provider_id = $1`, s.shared.ID).
			Scan(&owner, &revision, &storedKey, &storedKeyRev, &auth, &tool); err != nil {
			t.Fatal(err)
		}
		if owner != s.b.user || revision != s.shared.CurrentRevision || storedKey != keyID || storedKeyRev != keyRev ||
			auth != "ok" || tool != "passed" {
			t.Fatalf("stored evidence = (%s, %d, %s, %d, %q, %q), want (%s, %d, %s, %d, ok, passed)",
				owner, revision, storedKey, storedKeyRev, auth, tool, s.b.user, s.shared.CurrentRevision, keyID, keyRev)
		}
		if rec := retire(); rec.Code != http.StatusOK {
			t.Fatalf("interleaved retire = %d %s", rec.Code, rec.Body.String())
		}
	}}
	orig := s.f.h.q
	s.f.h.q = store.New(hook)
	defer func() { s.f.h.q = orig }()
	rec := bearerReqBody(s.b.router, http.MethodPost, s.sharedPath()+"/test", s.b.uzc, "")
	if hook.reads != 3 || rec.Code != http.StatusConflict {
		t.Fatalf("probe after retirement = %d %s (reads=%d), want 409 after final reread", rec.Code, rec.Body.String(), hook.reads)
	}
}
