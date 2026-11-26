package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Router-level coverage for admin-shared CODEX provider definitions (issue #4633 M2): the admin
// create and edit rules, the Codex recipient routes, the per-user key and Test evidence, the delete
// guard and the cross-engine refusals. Driven through the REAL h.Routes() router. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// codexGateway is a key-aware OpenAI Responses fixture gateway: a key listed in rejectAuth is answered
// 401, every other key gets the probe's two answers (plain, then the tool call).
type codexGateway struct {
	srv        *httptest.Server
	mu         sync.Mutex
	rejectAuth map[string]bool
}

func newCodexGateway(t *testing.T) *codexGateway {
	t.Helper()
	g := &codexGateway{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		rejected := g.rejectAuth[r.Header.Get("Authorization")]
		g.mu.Unlock()
		if rejected || r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body["tools"] != nil {
			_, _ = w.Write([]byte(`{"object":"response","output":[{"type":"function_call","name":"uzi_probe","arguments":"{\"ok\":true}"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"response","output":[]}`))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *codexGateway) reject(authorization string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rejectAuth == nil {
		g.rejectAuth = map[string]bool{}
	}
	g.rejectAuth[authorization] = true
}

// useCodexGateway stands the Responses fixture gateway and allowlists it.
func (s *sharedFx) useCodexGateway(t *testing.T) {
	t.Helper()
	s.cgw = newCodexGateway(t)
	s.codexBase = s.cgw.srv.URL + "/v1"
	s.f.h.cfg.ProviderAllowedBaseURLs = append(s.f.h.cfg.ProviderAllowedBaseURLs, s.codexBase)
}

func (s *sharedFx) codexURL() string {
	if s.codexBase != "" {
		return s.codexBase
	}
	return s.base
}

const codexDefaultMap = `{"default":{"model":"gateway-default","context_window":200000}}`

// sharedCodexBody is an admin create body for a Codex definition; protocol is omitted on purpose (the
// handler defaults it per engine).
func (s *sharedFx) sharedCodexBody(name string) string {
	return `{"name":"` + name + `","engine":"codex","base_url":"` + s.codexURL() + `","auth_style":"bearer","model_map":` + codexDefaultMap + `}`
}

func (s *sharedFx) createSharedCodex(t *testing.T, name string) apitypes.AdminSharedProviderDTO {
	t.Helper()
	rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath, s.admin.jwt, s.sharedCodexBody(name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin codex create = %d %s", rec.Code, rec.Body.String())
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

func (f providerFixture) putCodexSource(t *testing.T, body string) (int, string, string) {
	t.Helper()
	rec := cookieReq(t, f.router, http.MethodPut, "/api/me/providers/codex-source", f.jwt, body)
	return rec.Code, providerReason(t, rec), rec.Body.String()
}

func (f providerFixture) putClaudeSourceCookie(t *testing.T, body string) (int, string, string) {
	t.Helper()
	rec := cookieReq(t, f.router, http.MethodPut, "/api/me/providers/claude-source", f.jwt, body)
	return rec.Code, providerReason(t, rec), rec.Body.String()
}

// lanes reads both selection columns pairs of a user straight from users.
type laneState struct{ claude, claudeScope, codex, codexScope *string }

func (s *sharedFx) lanes(t *testing.T, user uuid.UUID) laneState {
	t.Helper()
	var l laneState
	if err := s.f.pool.QueryRow(context.Background(), `SELECT claude_provider_id::text, claude_provider_scope_id::text,
        codex_provider_id::text, codex_provider_scope_id::text FROM users WHERE id = $1`, user).
		Scan(&l.claude, &l.claudeScope, &l.codex, &l.codexScope); err != nil {
		t.Fatal(err)
	}
	return l
}

func ptrIs(p *string, want string) bool { return p != nil && *p == want }

func (l laneState) codexSelected(id string) bool {
	return ptrIs(l.codex, id) && ptrIs(l.codexScope, sharedNilScope)
}

func (l laneState) codexClear() bool { return l.codex == nil && l.codexScope == nil }

func (s *sharedFx) wantProbe(t *testing.T, who providerFixture, id, want string) {
	t.Helper()
	var out struct {
		Probe *apitypes.ProviderProbeDTO `json:"probe"`
	}
	rec := bearerReqBody(who.router, http.MethodPost, "/api/me/providers/"+id+"/test", who.uzc, "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Probe == nil || out.Probe.AuthResult != want {
		t.Fatalf("test = %d %s, want %s", rec.Code, rec.Body.String(), want)
	}
}

func TestSharedProviderCodexAdminCreateLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	before := countRows(t, s, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`)
	post := func(jwt, body string) *httptest.ResponseRecorder {
		return cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath, jwt, body)
	}
	created := func(rec *httptest.ResponseRecorder) apitypes.AdminSharedProviderDTO {
		t.Helper()
		var env struct {
			Provider apitypes.AdminSharedProviderDTO `json:"provider"`
		}
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &env) != nil {
			t.Fatalf("create = %d %s, want 201", rec.Code, rec.Body.String())
		}
		s.cleanupShared(t, env.Provider.ID)
		return env.Provider
	}
	revision := func(id string) (engine, protocol, auth string) {
		t.Helper()
		if err := s.f.pool.QueryRow(context.Background(),
			`SELECT engine, protocol, auth_style FROM model_provider_revisions WHERE provider_id = $1 AND revision = 1`, id).Scan(&engine, &protocol, &auth); err != nil {
			t.Fatal(err)
		}
		return
	}
	tag := uuid.NewString()[:8]

	// An explicit Codex create.
	explicit := created(post(s.admin.jwt, `{"name":"cx-explicit-`+tag+`","engine":"codex","protocol":"openai_responses","base_url":"`+s.base+`","auth_style":"bearer","model_map":`+codexDefaultMap+`}`))
	if explicit.Engine != "codex" || explicit.Protocol != "openai_responses" || explicit.AuthStyle != "bearer" || explicit.CredentialMode != "per_user_key" ||
		explicit.CurrentRevision != 1 || explicit.AttachedKeyCount != 0 || explicit.SelectedUserCount != 0 {
		t.Errorf("explicit codex create = %+v", explicit)
	}
	if e, p, a := revision(explicit.ID); e != "codex" || p != "openai_responses" || a != "bearer" {
		t.Errorf("explicit codex revision = %s %s %s", e, p, a)
	}
	// An omitted protocol defaults per engine.
	omitted := created(post(s.admin.jwt, s.sharedCodexBody("cx-omitted-"+tag)))
	if e, p, _ := revision(omitted.ID); e != "codex" || p != "openai_responses" || omitted.Protocol != "openai_responses" {
		t.Errorf("codex with an omitted protocol stored %s %s (dto %s), want codex openai_responses", e, p, omitted.Protocol)
	}
	claude := created(post(s.admin.jwt, s.sharedBody("cl-omitted-"+tag, "bearer")))
	if e, p, _ := revision(claude.ID); claude.Engine != "claude" || e != "claude" || p != "anthropic_messages" {
		t.Errorf("a create with no engine and no protocol stored %s %s (dto engine %s), want claude anthropic_messages", e, p, claude.Engine)
	}
	if n := countRows(t, s, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`); n != before+3 {
		t.Fatalf("shared rows = %d, want %d", n, before+3)
	}

	// Refusals: each is a 400 and writes nothing.
	good := s.sharedCodexBody("cx-refused-" + tag)
	for label, body := range map[string]string{
		"x_api_key":             strings.Replace(good, `"bearer"`, `"x_api_key"`, 1),
		"anthropic protocol":    strings.Replace(good, `"engine":"codex"`, `"engine":"codex","protocol":"anthropic_messages"`, 1),
		"missing default slot":  strings.Replace(good, codexDefaultMap, `{"haiku":{"model":"h"},"opus":{"model":"o"},"sonnet":{"model":"s"}}`, 1),
		"third engine":          strings.Replace(good, `"engine":"codex"`, `"engine":"gemini"`, 1),
		"claude with responses": strings.Replace(s.sharedBody("cl-resp-"+tag, "bearer"), `"name"`, `"protocol":"openai_responses","name"`, 1),
	} {
		if rec := post(s.admin.jwt, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", label, rec.Code, rec.Body.String())
		}
	}
	// A non-admin session is refused and writes nothing.
	if rec := post(s.b.jwt, s.sharedCodexBody("cx-denied-"+tag)); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin codex create = %d, want 403", rec.Code)
	}
	if n := countRows(t, s, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`); n != before+3 {
		t.Errorf("a refused create wrote a row: %d, want %d", n, before+3)
	}
}

func TestSharedProviderCodexSelectAndClearLiveDB(t *testing.T) {
	s := newSharedFx(t, true) // a shared Claude definition for B's Claude lane
	cx := s.createSharedCodex(t, "sel-"+uuid.NewString()[:8])
	s.b.putKeyFor(t, s.shared.ID, testProviderKey)
	s.b.putKeyFor(t, cx.ID, testProviderKey)
	if code, _, body := s.b.putSource(t, true, `{"provider_id":"`+s.shared.ID+`"}`); code != http.StatusOK {
		t.Fatalf("B's Claude select = %d %s", code, body)
	}
	if l := s.lanes(t, s.b.user); !l.codexClear() || !ptrIs(l.claude, s.shared.ID) {
		t.Fatalf("starting lanes = %+v, want a Claude source and a cleared Codex lane", l)
	}

	code, _, body := s.b.putCodexSource(t, `{"provider_id":"`+cx.ID+`"}`)
	selected := code == http.StatusOK && strings.Contains(body, cx.ID)
	if !selected {
		t.Errorf("codex select = %d %s", code, body)
	}
	// Always read the columns back: a failed select must have left the Codex lane cleared.
	l := s.lanes(t, s.b.user)
	if !ptrIs(l.claude, s.shared.ID) || !ptrIs(l.claudeScope, sharedNilScope) {
		t.Errorf("the Codex select changed the Claude lane: %+v", l)
	}
	if selected && !l.codexSelected(cx.ID) {
		t.Errorf("lanes after the Codex select = %+v, want the Codex source set", l)
	}
	if !selected {
		if !l.codexClear() {
			t.Errorf("a failed Codex select left the Codex lane %+v, want it cleared", l)
		}
		return
	}
	code, _, body = s.b.putCodexSource(t, `{"provider_id":null}`)
	cleared := code == http.StatusOK
	if !cleared {
		t.Errorf("codex clear = %d %s", code, body)
	}
	l = s.lanes(t, s.b.user)
	if !ptrIs(l.claude, s.shared.ID) || !ptrIs(l.claudeScope, sharedNilScope) {
		t.Errorf("the Codex clear changed the Claude lane: %+v", l)
	}
	if cleared && !l.codexClear() {
		t.Errorf("lanes after the Codex clear = %+v, want a cleared Codex lane", l)
	}
	if !cleared {
		if !l.codexSelected(cx.ID) {
			t.Errorf("a failed Codex clear changed the Codex lane: %+v, want it still selected", l)
		}
		return
	}
	// With nothing selecting it, the admin can delete the Codex definition and B's key goes with it.
	rec := cookieReq(t, s.f.router, http.MethodDelete, adminProvidersPath+"/"+cx.ID, s.admin.jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete after the clear = %d %s", rec.Code, rec.Body.String())
	}
	if n := countRows(t, s, `SELECT count(*) FROM user_secrets WHERE provider_id = $1`, cx.ID); n != 0 {
		t.Errorf("%d key(s) left after the delete", n)
	}
}

func TestSharedProviderCodexSelectionRefusalsLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	cx := s.createSharedCodex(t, "ref-"+uuid.NewString()[:8])
	wantRefused := func(who providerFixture, id, why string) {
		t.Helper()
		code, reason, body := who.putCodexSource(t, `{"provider_id":"`+id+`"}`)
		if code != http.StatusConflict || reason != "provider_source_unusable" {
			t.Errorf("%s: select = %d %s, want 409 provider_source_unusable", why, code, body)
		}
		// Always read the columns back, so an unexpected status still shows whether anything was written.
		if l := s.lanes(t, who.user); !l.codexClear() {
			t.Errorf("%s: a refused select left the Codex lane %+v", why, l)
		}
	}
	// No key.
	wantRefused(s.c, cx.ID, "no key")
	// A disabled key.
	s.b.putKeyFor(t, cx.ID, testProviderKey)
	if rec := cookieReq(t, s.f.router, http.MethodPatch, "/api/me/providers/"+cx.ID+"/key/enabled", s.b.jwt, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	wantRefused(s.b, cx.ID, "disabled key")
	// A retired definition.
	retired := s.createSharedCodex(t, "ret-"+uuid.NewString()[:8])
	s.c.putKeyFor(t, retired.ID, testProviderKey)
	if rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath+"/"+retired.ID+"/retire", s.admin.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	wantRefused(s.c, retired.ID, "retired definition")
}

func TestSharedProviderCrossEngineSelectionRefusedLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	cx := s.createSharedCodex(t, "cross-"+uuid.NewString()[:8])
	s.b.putKeyFor(t, s.shared.ID, testProviderKey)
	s.b.putKeyFor(t, cx.ID, testProviderKey)

	if code, _, body := s.b.putCodexSource(t, `{"provider_id":"`+s.shared.ID+`"}`); code != http.StatusNotFound {
		t.Errorf("shared Claude id on /codex-source = %d %s, want 404", code, body)
	}
	if code, _, body := s.b.putClaudeSourceCookie(t, `{"provider_id":"`+cx.ID+`"}`); code != http.StatusNotFound {
		t.Errorf("shared Codex id on /claude-source = %d %s, want 404", code, body)
	}
	q := store.New(s.f.pool)
	ctx := context.Background()
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: s.b.user, Engine: "codex", ProviderID: pgtype.UUID{Bytes: uuid.MustParse(s.shared.ID), Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("store: codex + shared Claude id = %v, want no rows", err)
	}
	if _, err := q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: s.b.user, Engine: "claude", ProviderID: pgtype.UUID{Bytes: uuid.MustParse(cx.ID), Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("store: claude + shared Codex id = %v, want no rows", err)
	}
	if l := s.lanes(t, s.b.user); l.claude != nil || l.claudeScope != nil || !l.codexClear() {
		t.Errorf("a refused cross-engine select changed the lanes: %+v", l)
	}
}

func TestSharedProviderCodexDeleteGuardLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	cx := s.createSharedCodex(t, "del-"+uuid.NewString()[:8])
	s.b.putKeyFor(t, cx.ID, testProviderKey)
	s.c.putKeyFor(t, cx.ID, "dummy-gateway-key-cccc")
	if code, _, body := s.b.putCodexSource(t, `{"provider_id":"`+cx.ID+`"}`); code != http.StatusOK {
		t.Fatalf("B select = %d %s", code, body)
	}
	if got := s.adminGetID(t, cx.ID); got.SelectedUserCount != 1 || got.AttachedKeyCount != 2 {
		t.Fatalf("admin counts = %+v, want 1 selected and 2 keys", got)
	}
	del := func() *httptest.ResponseRecorder {
		return cookieReq(t, s.f.router, http.MethodDelete, adminProvidersPath+"/"+cx.ID, s.admin.jwt, "")
	}
	rec := del()
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_in_use_as_source" {
		t.Fatalf("delete of a selected Codex definition = %d %s, want 409 provider_in_use_as_source", rec.Code, rec.Body.String())
	}
	if n := countRows(t, s, `SELECT count(*) FROM user_secrets WHERE provider_id = $1`, cx.ID); n != 2 {
		t.Fatalf("a refused delete removed keys: %d left, want 2", n)
	}
	if n := countRows(t, s, `SELECT count(*) FROM model_providers WHERE id = $1`, cx.ID); n != 1 {
		t.Fatalf("a refused delete removed the definition")
	}
	if code, _, body := s.b.putCodexSource(t, `{"provider_id":null}`); code != http.StatusOK {
		t.Fatalf("clear = %d %s", code, body)
	}
	if rec := del(); rec.Code != http.StatusOK {
		t.Fatalf("delete after the clear = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSharedProviderCodexEditValidatesAsCodexLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	ctx := context.Background()
	// Seed the definition and its first revision by direct SQL, not through the revision query.
	id := uuid.New()
	if _, err := s.f.pool.Exec(ctx, `INSERT INTO model_providers (id, owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
        VALUES ($1, NULL, $2, 'codex', 1, 'per_user_key', $3)`, id, "edit-cx-"+uuid.NewString()[:8], sharedNilScope); err != nil {
		t.Fatal(err)
	}
	s.cleanupShared(t, id.String())
	if _, err := s.f.pool.Exec(ctx, `INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map, engine)
        VALUES ($1, 1, 'openai_responses', $2, 'bearer', $3, 'codex')`, id, s.base, codexDefaultMap); err != nil {
		t.Fatal(err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		return cookieReq(t, s.f.router, http.MethodPatch, adminProvidersPath+"/"+id.String(), s.admin.jwt, body)
	}
	revisions := func() int {
		return countRows(t, s, `SELECT count(*) FROM model_provider_revisions WHERE provider_id = $1`, id)
	}

	if rec := patch(`{"model_map":{"opus":{"model":"o"},"sonnet":{"model":"s"},"haiku":{"model":"h"}}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a Claude-shaped map on a Codex definition = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if n := revisions(); n != 1 {
		t.Fatalf("a refused edit left %d revision rows, want 1", n)
	}
	rec := patch(`{"model_map":{"default":{"model":"gateway-two","context_window":123456}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid Codex edit = %d %s", rec.Code, rec.Body.String())
	}
	var engine, protocol, auth, models string
	if err := s.f.pool.QueryRow(ctx, `SELECT engine, protocol, auth_style, model_map::text FROM model_provider_revisions WHERE provider_id = $1 AND revision = 2`, id).
		Scan(&engine, &protocol, &auth, &models); err != nil {
		t.Fatalf("revision 2: %v", err)
	}
	if engine != "codex" || protocol != "openai_responses" || auth != "bearer" || !strings.Contains(models, "gateway-two") {
		t.Errorf("revision 2 = %s %s %s %s, want a codex revision copying the destination with the new map", engine, protocol, auth, models)
	}
}

func TestSharedProviderClaudeEditPrecedenceLiveDB(t *testing.T) {
	s := newSharedFx(t, true)
	const invalid = `{"model_map":{"fable":{"model":"f"}}}`
	const valid = `{"model_map":{"opus":{"model":"o"},"sonnet":{"model":"s"},"haiku":{"model":"h"}}}`
	patch := func(id, body string) *httptest.ResponseRecorder {
		return cookieReq(t, s.f.router, http.MethodPatch, adminProvidersPath+"/"+id, s.admin.jwt, body)
	}
	unknown := uuid.NewString()
	if rec := patch(unknown, invalid); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid map on an unknown id = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := patch(unknown, valid); rec.Code != http.StatusNotFound {
		t.Errorf("valid map on an unknown id = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath+"/"+s.shared.ID+"/retire", s.admin.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	if rec := patch(s.shared.ID, invalid); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid map on a retired definition = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := patch(s.shared.ID, valid); rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_retired" {
		t.Errorf("valid map on a retired definition = %d %s, want 409 provider_retired", rec.Code, rec.Body.String())
	}
}

func TestSharedProviderCodexVisibilityLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	s.useCodexGateway(t)
	cx := s.createSharedCodex(t, "vis-"+uuid.NewString()[:8])
	personal := s.f.create(t, `{"name":"a-private-codex","engine":"codex","protocol":"openai_responses","base_url":"`+s.codexBase+`","auth_style":"bearer","model_map":`+codexDefaultMap+`}`)

	got := s.b.getProvider(t, cx.ID)
	if got.Engine != "codex" || got.CredentialMode == nil || *got.CredentialMode != "per_user_key" || got.HasKey || got.Probe != nil || got.Protocol != "openai_responses" {
		t.Errorf("B's view of the shared Codex definition = %+v", got)
	}
	rec := bearerReqBody(s.b.router, http.MethodGet, "/api/me/providers", s.b.uzc, "")
	var list apitypes.ProviderListDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("B list = %d %s", rec.Code, rec.Body.String())
	}
	saw := false
	for _, p := range list.Providers {
		if p.ID == cx.ID {
			saw = true
		}
		if p.ID == personal.ID {
			t.Errorf("B's list contains A's personal Codex definition")
		}
	}
	if !saw {
		t.Errorf("B's list lacks the shared Codex definition: %s", rec.Body.String())
	}
	if rec := bearerReqBody(s.b.router, http.MethodGet, "/api/me/providers/"+personal.ID, s.b.uzc, ""); rec.Code != http.StatusNotFound {
		t.Errorf("B reading A's personal Codex definition = %d, want 404", rec.Code)
	}
}

func TestSharedProviderCodexRecipientKeyRoutesLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	cx := s.createSharedCodex(t, "keys-"+uuid.NewString()[:8])
	s.f.putKeyFor(t, cx.ID, "dummy-gateway-key-aaaa")
	s.b.putKeyFor(t, cx.ID, testProviderKey)
	s.b.putKeyFor(t, cx.ID, "dummy-gateway-key-0002") // rotation

	var kind, scope string
	if err := s.f.pool.QueryRow(context.Background(), `SELECT kind, provider_scope_id::text FROM user_secrets WHERE user_id = $1 AND provider_id = $2`, s.b.user, cx.ID).Scan(&kind, &scope); err != nil {
		t.Fatal(err)
	}
	if kind != "openai_compatible_key" || scope != sharedNilScope {
		t.Errorf("B's key row = %s scope %s, want openai_compatible_key and the shared scope", kind, scope)
	}
	if got := s.b.getProvider(t, cx.ID); !got.HasKey || !got.KeyEnabled {
		t.Errorf("B after attach = %+v", got)
	}
	if got := s.c.getProvider(t, cx.ID); got.HasKey {
		t.Errorf("C sees a key: %+v", got)
	}
	if got := s.adminGetID(t, cx.ID); got.AttachedKeyCount != 2 {
		t.Errorf("admin attached keys = %d, want A's and B's", got.AttachedKeyCount)
	}
	if rec := cookieReq(t, s.f.router, http.MethodPatch, "/api/me/providers/"+cx.ID+"/key/enabled", s.b.jwt, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if got := s.b.getProvider(t, cx.ID); !got.HasKey || got.KeyEnabled {
		t.Errorf("B after disable = %+v", got)
	}
	if got := s.f.getProvider(t, cx.ID); !got.HasKey || !got.KeyEnabled {
		t.Errorf("A's key changed with B's disable: %+v", got)
	}
	if rec := cookieReq(t, s.f.router, http.MethodDelete, "/api/me/providers/"+cx.ID+"/key", s.b.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("key delete = %d %s", rec.Code, rec.Body.String())
	}
	if got := s.b.getProvider(t, cx.ID); got.HasKey {
		t.Errorf("B after delete = %+v", got)
	}
	if got := s.f.getProvider(t, cx.ID); !got.HasKey {
		t.Errorf("B's delete removed A's key")
	}
}

func TestSharedProviderCodexTestEvidenceIsPerUserLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	s.useCodexGateway(t)
	cx := s.createSharedCodex(t, "ev-"+uuid.NewString()[:8])
	keyC := "dummy-gateway-key-cccc"
	s.cgw.reject("Bearer " + keyC)
	s.b.putKeyFor(t, cx.ID, testProviderKey)
	s.c.putKeyFor(t, cx.ID, keyC)
	s.wantProbe(t, s.b, cx.ID, "ok")
	s.wantProbe(t, s.c, cx.ID, "rejected")

	if got := s.b.getProvider(t, cx.ID); got.Probe == nil || got.Probe.AuthResult != "ok" {
		t.Errorf("B reads %+v", got.Probe)
	}
	if got := s.c.getProvider(t, cx.ID); got.Probe == nil || got.Probe.AuthResult != "rejected" {
		t.Errorf("C reads %+v", got.Probe)
	}
	if got := s.f.getProvider(t, cx.ID); got.Probe != nil || got.HasKey {
		t.Errorf("A (no key) reads %+v", got)
	}
	for who, f := range map[string]providerFixture{"B": s.b, "C": s.c} {
		rec := bearerReqBody(f.router, http.MethodGet, "/api/me/providers", f.uzc, "")
		var list apitypes.ProviderListDTO
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
			t.Fatalf("%s list = %d", who, rec.Code)
		}
		want := "ok"
		if who == "C" {
			want = "rejected"
		}
		found := false
		for _, p := range list.Providers {
			if p.ID != cx.ID {
				continue
			}
			found = true
			if p.Probe == nil || p.Probe.AuthResult != want {
				t.Errorf("%s's list shows %+v for the shared Codex definition, want %s", who, p.Probe, want)
			}
		}
		if !found {
			t.Errorf("%s's list omits the shared Codex definition %s", who, cx.ID)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM model_provider_key_test_results WHERE provider_id = $1`, cx.ID); n != 2 {
		t.Errorf("evidence rows = %d, want one per user", n)
	}
	// Rotating B's key resets B's result and leaves C's.
	s.b.putKeyFor(t, cx.ID, "dummy-gateway-key-0009")
	if got := s.b.getProvider(t, cx.ID); got.Probe != nil {
		t.Errorf("B's result survived a rotation: %+v", got.Probe)
	}
	if got := s.c.getProvider(t, cx.ID); got.Probe == nil || got.Probe.AuthResult != "rejected" {
		t.Errorf("C's result changed: %+v", got.Probe)
	}

	// The admin reads carry the definition and two counts, nothing per user.
	want := []string{"attached_key_count", "auth_style", "base_url", "credential_mode", "current_revision", "engine", "id", "model_map", "name", "protocol", "retired", "selected_user_count"}
	keysOf := func(raw json.RawMessage) []string {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	rec := bearerReqBody(s.f.router, http.MethodGet, adminProvidersPath+"/"+cx.ID, s.uza, "")
	var one struct {
		Provider json.RawMessage `json:"provider"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &one) != nil || !reflect.DeepEqual(keysOf(one.Provider), want) {
		t.Errorf("admin get = %d keys %v, want %v", rec.Code, keysOf(one.Provider), want)
	}
	rec = bearerReqBody(s.f.router, http.MethodGet, adminProvidersPath, s.uza, "")
	var all struct {
		Providers []json.RawMessage `json:"providers"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &all) != nil {
		t.Fatalf("admin list = %d", rec.Code)
	}
	sawCx := false
	for _, p := range all.Providers {
		if !reflect.DeepEqual(keysOf(p), want) {
			t.Errorf("admin list entry keys %v, want %v", keysOf(p), want)
		}
		var idOnly struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(p, &idOnly) == nil && idOnly.ID == cx.ID {
			sawCx = true
		}
	}
	if !sawCx {
		t.Errorf("admin list omits the shared Codex definition %s", cx.ID)
	}
	for _, secret := range []string{testProviderKey, keyC, "auth_result", "ciphertext"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the admin list carries %q", secret)
		}
	}
}
