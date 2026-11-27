package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/operatorkey"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #4540 M1: the organization-funded definition's admin routes (create, edit, key, delete) and the
// proof that no recipient route reaches it, through the real router. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func TestOperatorCreateAndPatchLiveDB(t *testing.T) {
	s := newSharedFx(t, false)
	post := func(body string) (int, string) {
		rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath, s.admin.jwt, body)
		return rec.Code, rec.Body.String()
	}

	// Omitting both new fields creates the per-user-key definition, with no organization key state.
	perUser := s.createShared(t, "peruser-"+uuid.NewString()[:8], "bearer")
	if perUser.CredentialMode != "per_user_key" || perUser.OperatorKey != nil {
		t.Fatalf("default create = %+v, want a per_user_key definition with no operator_key", perUser)
	}

	name := "op-" + uuid.NewString()[:8]
	good := s.sharedBody(name, "bearer")
	withMode := func(extra string) string { return strings.Replace(good, `"name"`, extra+`"name"`, 1) }

	// Each refusal is its own 400 and writes no row.
	n0 := countRows(t, s, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`)
	for _, c := range []struct{ label, body, msg string }{
		{"unknown mode", withMode(`"credential_mode":"shared_key",`), "credential_mode must be per_user_key or operator_key"},
		{"missing acknowledgment", withMode(`"credential_mode":"operator_key",`), "acknowledge_recipient_trust must be true to create an organization-key provider"},
		{"acknowledgment false", withMode(`"credential_mode":"operator_key","acknowledge_recipient_trust":false,`), "acknowledge_recipient_trust must be true"},
		{"codex with operator mode", withMode(`"credential_mode":"operator_key","acknowledge_recipient_trust":true,"engine":"codex",`), "an organization key supports the claude engine only"},
		{"a key in the body", withMode(operatorFields + `"key":"x",`), "a provider is saved without its key"},
	} {
		code, body := post(c.body)
		if code != http.StatusBadRequest || !strings.Contains(body, c.msg) {
			t.Errorf("%s = %d %s, want 400 containing %q", c.label, code, body, c.msg)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL`); n != n0 {
		t.Fatalf("a refused create wrote a row: %d -> %d", n0, n)
	}

	// The acknowledged Claude request succeeds; the audit line records the mode and the acknowledgment.
	logs := captureProviderAudit(t)
	created := s.createOperator(t, name)
	if created.CredentialMode != "operator_key" || created.Engine != "claude" || created.CurrentRevision != 1 ||
		created.OperatorKey == nil || created.OperatorKey.Present || created.OperatorKey.Enabled || created.OperatorKey.Probe != nil {
		t.Fatalf("created = %+v", created)
	}
	if ev := auditEvents(logs, "admin_create"); len(ev) != 1 ||
		auditField(ev[0], "credential_mode") != "operator_key" || auditField(ev[0], "acknowledged_recipient_trust") != "true" {
		t.Errorf("admin_create audit = %v, want one line with credential_mode=operator_key acknowledged_recipient_trust=true", ev)
	}

	// The definition counts against the shared cap with the per-user-key ones, and is listed with its mode.
	rec := bearerReqBody(s.f.router, http.MethodGet, adminProvidersPath, s.uza, "")
	var list apitypes.AdminSharedProviderListDTO
	if rec.Code != http.StatusOK || strictUnmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	modes := map[string]string{}
	for _, p := range list.Providers {
		modes[p.ID] = p.CredentialMode
	}
	if modes[created.ID] != "operator_key" || modes[perUser.ID] != "per_user_key" {
		t.Errorf("listed modes = %v", modes)
	}

	// PATCH never changes the mode: the field is refused with no write, and a rename or a model-map edit
	// keeps the mode in both modes.
	for _, d := range []apitypes.AdminSharedProviderDTO{created, perUser} {
		path := opPath(d.ID)
		rec := s.adminDo(t, http.MethodPatch, path, `{"credential_mode":"operator_key"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "credential mode is fixed at creation") {
			t.Errorf("%s: patch credential_mode = %d %s, want 400", d.CredentialMode, rec.Code, rec.Body.String())
		}
		rec = s.adminDo(t, http.MethodPatch, path, `{"name":"x-`+d.Name+`","credential_mode":"per_user_key"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: patch name+credential_mode = %d, want 400 with no write", d.CredentialMode, rec.Code)
		}
		if got := s.adminGetID(t, d.ID); got.Name != d.Name || got.CredentialMode != d.CredentialMode || got.CurrentRevision != 1 {
			t.Fatalf("%s: a refused patch changed the definition: %+v", d.CredentialMode, got)
		}
		rec = s.adminDo(t, http.MethodPatch, path, `{"name":"renamed-`+uuid.NewString()[:6]+`"}`)
		if got := decodeAdminProvider(t, rec); rec.Code != http.StatusOK || got.CredentialMode != d.CredentialMode {
			t.Errorf("%s: rename = %d %+v, want the mode kept", d.CredentialMode, rec.Code, got)
		}
		rec = s.adminDo(t, http.MethodPatch, path, `{"model_map":{"haiku":{"model":"h-`+uuid.NewString()[:6]+`"}}}`)
		if got := decodeAdminProvider(t, rec); rec.Code != http.StatusOK || got.CredentialMode != d.CredentialMode || got.CurrentRevision != 2 {
			t.Errorf("%s: model-map edit = %d %+v, want the mode kept at revision 2", d.CredentialMode, rec.Code, got)
		}
	}
}

func TestOperatorKeyRoutesLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	id := s.op.ID
	key := orgKeyFixture()

	// A per-user-key definition, a personal provider and an unknown id: 409 for the first, 404 for the rest.
	perUser := s.createShared(t, "peruser-"+uuid.NewString()[:8], "bearer")
	personal := s.f.create(t, s.personalBody("personal-"+uuid.NewString()[:6]))
	for _, call := range []struct{ method, suffix, body string }{
		{http.MethodPut, "/key", `{"key":"` + key + `"}`},
		{http.MethodPatch, "/key/enabled", `{"enabled":false}`},
		{http.MethodDelete, "/key", ""},
	} {
		rec := s.adminDo(t, call.method, opPath(perUser.ID)+call.suffix, call.body)
		if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_not_operator_key" {
			t.Errorf("%s %s on a per-user-key definition = %d %s, want 409 provider_not_operator_key", call.method, call.suffix, rec.Code, rec.Body.String())
		}
		for label, other := range map[string]string{"personal": personal.ID, "unknown": uuid.NewString()} {
			if rec := s.adminDo(t, call.method, opPath(other)+call.suffix, call.body); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s on a %s id = %d, want 404", call.method, call.suffix, label, rec.Code)
			}
		}
	}
	if n := countRows(t, s.sharedFx, `SELECT count(*) FROM model_provider_operator_keys WHERE provider_id = ANY($1)`, []string{perUser.ID, personal.ID}); n != 0 {
		t.Fatalf("a refused key write left %d custody row(s)", n)
	}

	// Disable, enable and delete of a keyless definition are 404s (there is no key).
	for _, call := range []struct{ method, suffix, body string }{
		{http.MethodPatch, "/key/enabled", `{"enabled":true}`}, {http.MethodDelete, "/key", ""},
	} {
		if rec := s.adminDo(t, call.method, opPath(id)+call.suffix, call.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on a keyless definition = %d, want 404", call.method, call.suffix, rec.Code)
		}
	}

	// Bad keys and bodies are 400s with no row.
	for _, body := range []string{`{"key":""}`, `{"key":"short"}`, `{"key":"has space in it 0123456789"}`, `{"key":"` + key + `","extra":1}`, `{}`, `not json`} {
		if rec := s.adminDo(t, http.MethodPut, opPath(id, "key"), body); rec.Code != http.StatusBadRequest {
			t.Errorf("put %q = %d, want 400", body, rec.Code)
		}
	}
	if rec := s.adminDo(t, http.MethodPatch, opPath(id, "key", "enabled"), `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("patch enabled without a value = %d, want 400", rec.Code)
	}
	if _, _, _, ok := s.custody(t, id); ok {
		t.Fatal("a refused key write left a custody row")
	}

	// Set: present, enabled, no probe, and the response carries no key material.
	rec := s.adminDo(t, http.MethodPut, opPath(id, "key"), `{"key":"`+key+`"}`)
	set := decodeAdminProvider(t, rec)
	if rec.Code != http.StatusOK || set.OperatorKey == nil || !set.OperatorKey.Present || !set.OperatorKey.Enabled ||
		set.OperatorKey.SetAt == nil || set.OperatorKey.Probe != nil {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{key, "ciphertext", "key_rev"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the set response carries %q: %s", leak, rec.Body.String())
		}
	}
	// The key row is bound to its definition: it opens for this id and not for another.
	var ct []byte
	if err := s.f.pool.QueryRow(context.Background(), `SELECT ciphertext FROM model_provider_operator_keys WHERE provider_id = $1`, id).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	pid := uuid.MustParse(id)
	if plain, err := operatorkey.Open(s.f.h.box, pid, ct); err != nil || string(plain) != key {
		t.Fatalf("stored key does not open for its definition: %v", err)
	}
	if _, err := operatorkey.Open(s.f.h.box, uuid.New(), ct); err == nil {
		t.Fatal("stored key opened for another definition")
	}

	// Replace keeps the enablement; disable and enable toggle it; a repeat is a 200 with no change.
	if rep := s.mustPutOrgKey(t, id, orgKeyFixture()); !rep.OperatorKey.Enabled {
		t.Errorf("replace = %+v", rep.OperatorKey)
	}
	rec = s.adminDo(t, http.MethodPatch, opPath(id, "key", "enabled"), `{"enabled":false}`)
	if d := decodeAdminProvider(t, rec); rec.Code != http.StatusOK || d.OperatorKey.Enabled || !d.OperatorKey.Present {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if rep := s.mustPutOrgKey(t, id, orgKeyFixture()); rep.OperatorKey.Enabled {
		t.Errorf("a replace re-enabled a disabled key: %+v", rep.OperatorKey)
	}
	rec = s.adminDo(t, http.MethodPatch, opPath(id, "key", "enabled"), `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Errorf("repeated disable = %d, want 200", rec.Code)
	}
	rec = s.adminDo(t, http.MethodPatch, opPath(id, "key", "enabled"), `{"enabled":true}`)
	if d := decodeAdminProvider(t, rec); rec.Code != http.StatusOK || !d.OperatorKey.Enabled {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
	}

	// A retired definition still takes a key, a replacement, a disable and a delete.
	if rec := s.adminDo(t, http.MethodPost, opPath(id, "retire"), ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	s.mustPutOrgKey(t, id, orgKeyFixture())
	for _, call := range []struct{ method, suffix, body string }{
		{http.MethodPatch, "/key/enabled", `{"enabled":false}`}, {http.MethodPatch, "/key/enabled", `{"enabled":true}`}, {http.MethodDelete, "/key", ""},
	} {
		if rec := s.adminDo(t, call.method, opPath(id)+call.suffix, call.body); rec.Code != http.StatusOK {
			t.Errorf("%s %s on a retired definition = %d %s, want 200", call.method, call.suffix, rec.Code, rec.Body.String())
		}
	}
	if got := s.adminGetID(t, id); got.OperatorKey == nil || got.OperatorKey.Present {
		t.Errorf("after delete = %+v, want no key", got.OperatorKey)
	}
	s.mustPutOrgKey(t, id, orgKeyFixture())

	// Every write is audited with its own action and never with the key.
	for action, n := range map[string]int{"admin_key_set": 2, "admin_key_replace": 3, "admin_key_disable": 3, "admin_key_enable": 2, "admin_key_delete": 1} {
		if got := len(auditEvents(logs, action)); got != n {
			t.Errorf("%s audit lines = %d, want %d", action, got, n)
		}
	}
	if strings.Contains(logs.String(), key) {
		t.Error("the audit log carries the key")
	}
}

func TestOperatorKeyRevLiveDB(t *testing.T) {
	s := newOpFx(t)
	id := s.op.ID
	rev := func() (uuid.UUID, int64) {
		t.Helper()
		kid, r, _, ok := s.custody(t, id)
		if !ok {
			t.Fatal("no custody row")
		}
		return kid, r
	}
	put := func() { s.mustPutOrgKey(t, id, orgKeyFixture()) }
	toggle := func(enabled string) {
		t.Helper()
		if rec := s.adminDo(t, http.MethodPatch, opPath(id, "key", "enabled"), `{"enabled":`+enabled+`}`); rec.Code != http.StatusOK {
			t.Fatalf("enabled=%s = %d %s", enabled, rec.Code, rec.Body.String())
		}
	}

	put()
	kid, r1 := rev()
	put() // replace
	_, r2 := rev()
	toggle("false")
	_, r3 := rev()
	toggle("true")
	_, r4 := rev()
	if r1 >= r2 || r2 >= r3 || r3 >= r4 {
		t.Fatalf("key_rev did not strictly increase through set, replace, disable, enable: %d %d %d %d", r1, r2, r3, r4)
	}
	// A repeated enable or disable changes nothing.
	toggle("true")
	if _, r := rev(); r != r4 {
		t.Errorf("a repeated enable moved key_rev %d -> %d", r4, r)
	}
	toggle("false")
	_, r5 := rev()
	toggle("false")
	if _, r := rev(); r != r5 || r5 <= r4 {
		t.Errorf("disable/repeat: key_rev %d then %d (previous %d)", r5, r, r4)
	}
	toggle("true")
	_, r6 := rev()

	// Delete then restore: a new row id and a larger key_rev.
	if rec := s.adminDo(t, http.MethodDelete, opPath(id, "key"), ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	put()
	kid2, r7 := rev()
	if kid2 == kid || r7 <= r6 {
		t.Errorf("restore after delete: id %s -> %s, key_rev %d -> %d, want a new id and a larger key_rev", kid, kid2, r6, r7)
	}
}

func TestOperatorKeyAdminDeleteCascadeLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	del := func(id string) {
		t.Helper()
		rec := s.adminDo(t, http.MethodDelete, opPath(id), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("delete %s = %d %s", id, rec.Code, rec.Body.String())
		}
	}
	lastDelete := func(id string) string {
		t.Helper()
		for _, l := range auditEvents(logs, "admin_delete") {
			if auditField(l, "provider_id") == id {
				return auditField(l, "operator_key_deleted")
			}
		}
		return ""
	}

	// A custody row and its Test evidence go with the definition.
	s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
	if rec := s.adminDo(t, http.MethodPost, opPath(s.op.ID, "test"), ""); rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if s.evidenceRows(t, s.op.ID) != 1 {
		t.Fatal("no evidence row to cascade")
	}
	del(s.op.ID)
	for _, q := range []string{
		`SELECT count(*) FROM model_providers WHERE id = $1`,
		`SELECT count(*) FROM model_provider_operator_keys WHERE provider_id = $1`,
		`SELECT count(*) FROM model_provider_operator_key_test_results WHERE provider_id = $1`,
	} {
		if n := countRows(t, s.sharedFx, q, s.op.ID); n != 0 {
			t.Errorf("after delete %q = %d, want 0", q, n)
		}
	}
	if got := lastDelete(s.op.ID); got != "true" {
		t.Errorf("operator_key_deleted = %q, want true", got)
	}

	// A keyless organization definition deletes, and says no key was deleted.
	keyless := s.createOperator(t, "keyless-"+uuid.NewString()[:6])
	del(keyless.ID)
	if got := lastDelete(keyless.ID); got != "false" {
		t.Errorf("keyless operator_key_deleted = %q, want false", got)
	}
	// So does a per-user-key definition.
	perUser := s.createShared(t, "peruser-"+uuid.NewString()[:6], "bearer")
	del(perUser.ID)
	if got := lastDelete(perUser.ID); got != "false" {
		t.Errorf("per-user-key operator_key_deleted = %q, want false", got)
	}

	// Lock order: a key write holding the definition lock blocks the delete, which then completes with
	// no deadlock.
	held := s.createOperator(t, "held-"+uuid.NewString()[:6])
	s.mustPutOrgKey(t, held.ID, orgKeyFixture())
	ctx := context.Background()
	tx, err := s.f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	hid := uuid.MustParse(held.ID)
	if _, err := q.LockSharedModelProviderForEdit(ctx, hid); err != nil {
		t.Fatal(err)
	}
	if _, err := q.LockOperatorProviderKeyForWrite(ctx, hid); err != nil {
		t.Fatal(err)
	}
	done := blockedRequest(cookieReqPrepared(t, s.f.router, http.MethodDelete, opPath(held.ID), s.admin.jwt, ""))
	assertBlocked(t, done, "the delete")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := awaitDone(t, done, "the delete"); rec.Code != http.StatusOK {
		t.Fatalf("delete after the key write committed = %d %s", rec.Code, rec.Body.String())
	}
	if got := lastDelete(held.ID); got != "true" {
		t.Errorf("held operator_key_deleted = %q, want true", got)
	}
}

func TestOperatorModeImmutableLiveDB(t *testing.T) {
	s := newOpFx(t)
	perUser := s.createShared(t, "peruser-"+uuid.NewString()[:6], "bearer")
	for _, d := range []apitypes.AdminSharedProviderDTO{s.op, perUser} {
		for _, mode := range []string{"operator_key", "per_user_key", "nonsense"} {
			rec := s.adminDo(t, http.MethodPatch, opPath(d.ID), `{"credential_mode":"`+mode+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s -> %s: patch = %d %s, want 400", d.CredentialMode, mode, rec.Code, rec.Body.String())
			}
		}
		var got string
		if err := s.f.pool.QueryRow(context.Background(), `SELECT credential_mode FROM model_providers WHERE id = $1`, d.ID).Scan(&got); err != nil || got != d.CredentialMode {
			t.Errorf("stored mode = %q (%v), want %q", got, err, d.CredentialMode)
		}
	}
}

// userCookie sends one /api/me request as the credential's session (cookie + CSRF).
func userCookie(t *testing.T, f providerFixture, method, path, body string) (int, string) {
	t.Helper()
	rec := cookieReq(t, f.router, method, path, f.jwt, body)
	return rec.Code, rec.Body.String()
}

// cookieSource reads the user's Claude source id through the cookie session ("" when none).
func cookieSource(t *testing.T, f providerFixture) string {
	t.Helper()
	code, body := userCookie(t, f, http.MethodGet, "/api/me/settings", "")
	var s struct {
		Settings struct {
			ClaudeProviderID *string `json:"claude_provider_id"`
		} `json:"settings"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("settings = %d %s", code, body)
	}
	if s.Settings.ClaudeProviderID == nil {
		return ""
	}
	return *s.Settings.ClaudeProviderID
}

// TestOperatorProviderUnreachableLiveDB: no user route reaches an organization-funded definition, for a
// plain user and for an admin acting as a user, each with a valid cookie and CSRF. The admin's own
// definition routes are the only ones that see it.
func TestOperatorProviderUnreachableLiveDB(t *testing.T) {
	s := newOpFx(t)
	s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
	id := s.op.ID
	me := "/api/me/providers/" + id

	for who, f := range map[string]providerFixture{"non-admin": s.b, "admin": s.admin} {
		t.Run(who, func(t *testing.T) {
			// A personal provider and source the user already has: it must survive every refusal below.
			code, body := userCookie(t, f, http.MethodPost, "/api/me/providers", s.personalBody("mine-"+uuid.NewString()[:6]))
			if code != http.StatusCreated {
				t.Fatalf("create personal = %d %s", code, body)
			}
			var own struct {
				Provider apitypes.ProviderDTO `json:"provider"`
			}
			if err := strictUnmarshal([]byte(body), &own); err != nil {
				t.Fatal(err)
			}
			if code, body := userCookie(t, f, http.MethodPut, "/api/me/providers/"+own.Provider.ID+"/key", `{"key":"`+orgKeyFixture()+`"}`); code != http.StatusOK {
				t.Fatalf("personal key = %d %s", code, body)
			}
			if code, _, body := f.putSource(t, false, `{"provider_id":"`+own.Provider.ID+`"}`); code != http.StatusOK {
				t.Fatalf("select personal = %d %s", code, body)
			}

			// list omits it; get is the same 404 as an unknown id.
			code, body = userCookie(t, f, http.MethodGet, "/api/me/providers", "")
			if code != http.StatusOK || strings.Contains(body, id) {
				t.Errorf("list = %d, contains the operator definition: %v", code, strings.Contains(body, id))
			}
			code, unknown := userCookie(t, f, http.MethodGet, "/api/me/providers/"+uuid.NewString(), "")
			code2, got := userCookie(t, f, http.MethodGet, me, "")
			if code != http.StatusNotFound || code2 != http.StatusNotFound || got != unknown {
				t.Errorf("get = %d %s, want the same 404 as an unknown id (%d %s)", code2, got, code, unknown)
			}

			// select is refused and the previous selection is unchanged.
			if code, _, body := f.putSource(t, false, `{"provider_id":"`+id+`"}`); code == http.StatusOK {
				t.Errorf("select = %d %s, want a refusal", code, body)
			}
			if sel := cookieSource(t, f); sel != own.Provider.ID {
				t.Errorf("source after the refused select = %v, want %s", sel, own.Provider.ID)
			}

			// key PUT, DELETE and enable are 404; Test is a 404 with no gateway request.
			for _, c := range []struct{ method, suffix, body string }{
				{http.MethodPut, "/key", `{"key":"` + orgKeyFixture() + `"}`},
				{http.MethodDelete, "/key", ""},
				{http.MethodPatch, "/key/enabled", `{"enabled":false}`},
				{http.MethodPost, "/test", ""},
			} {
				if code, body := userCookie(t, f, c.method, me+c.suffix, c.body); code != http.StatusNotFound {
					t.Errorf("%s %s = %d %s, want 404", c.method, c.suffix, code, body)
				}
			}
		})
	}
	if n := len(s.gw.seenAuth()); n != 0 {
		t.Errorf("the gateway saw %d request(s) from a user route", n)
	}
	if _, _, dis, ok := s.custody(t, id); !ok || dis {
		t.Error("a user route changed the custody row")
	}
	if n := countRows(t, s.sharedFx, `SELECT count(*) FROM user_secrets WHERE provider_id = $1`, id); n != 0 {
		t.Errorf("a user route attached %d key(s) to the operator definition", n)
	}
	// The admin definition routes do see it.
	if got := s.adminGetID(t, id); got.CredentialMode != "operator_key" || got.OperatorKey == nil || !got.OperatorKey.Present {
		t.Errorf("admin get = %+v", got)
	}
}

// TestOperatorSelectionForcedReadsMissingLiveDB: an organization-funded definition forced (in SQL) as a
// user's Claude source reads as a missing provider to every path that resolves the source: the schedule
// preview, run creation and job creation answer a typed refusal and write no run; a lane that refuses
// any selected provider (a site-list job) still answers provider_lane_unsupported.
func TestOperatorSelectionForcedReadsMissingLiveDB(t *testing.T) {
	t.Run("preview and run creation", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.anthropic()
		op := seedOperatorDefinition(t, f.pool, f.h.q, newHandlerTestBox(t), effProviderBase)
		forceOperatorSource(t, f.pool, f.user, op)
		for _, body := range []string{`{"target":"sweep"}`, `{"target":"prompt","harness":"claude"}`} {
			dto, raw := f.schedulePreview(body)
			wantRefusal(t, dto, raw, "provider_source_unusable")
		}
		before := countRowsPool(t, f.pool, `SELECT count(*) FROM runs WHERE user_id = $1`, f.user)
		_, err := f.create(nil, nil)
		var unusable *workersvc.ProviderSourceUnusableError
		if !errors.As(err, &unusable) || unusable.Reason != workersvc.SourceUnusableProviderMissing {
			t.Fatalf("create with a forced operator source = %v, want a provider_missing refusal", err)
		}
		if n := countRowsPool(t, f.pool, `SELECT count(*) FROM runs WHERE user_id = $1`, f.user); n != before {
			t.Fatalf("a refused create inserted %d run row(s)", n-before)
		}
	})

	t.Run("job creation and the lane that refuses any selected source", func(t *testing.T) {
		e := newV1JobsEnv(t, 0)
		e.enableLane(true)
		for _, tc := range []struct {
			name, reason string
			body         func() string
		}{
			{"a plain job", "provider_source_unusable", func() string { return v1MinimalJob }},
			{"a site-list job", "provider_lane_unsupported", func() string {
				return `{"type":"research","prompt":"p","egress_profile":"` + e.siteList() + `"}`
			}},
		} {
			user, tok := e.user()
			op := seedOperatorDefinition(t, e.pool, e.h.q, newHandlerTestBox(t), v1ProviderBase)
			forceOperatorSource(t, e.pool, user, op)
			r := e.call(http.MethodPost, "/api/v1/jobs", tok, tc.body())
			e.want(r, http.StatusConflict, tc.reason)
			if tc.reason == "provider_source_unusable" && !strings.Contains(string(r.body), "no longer exists") {
				t.Errorf("%s: body = %s, want the provider reported missing", tc.name, r.body)
			}
			if n := e.jobCount(tok); n != 0 {
				t.Fatalf("%s: job runs = %d, want 0", tc.name, n)
			}
			if n := e.jobOriginCount(user); n != 0 {
				t.Fatalf("%s: job origins = %d, want 0", tc.name, n)
			}
		}
	})
}
