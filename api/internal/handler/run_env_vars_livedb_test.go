package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// PRD #4416 M1: the per-repo environment variable routes, driven through the real
// router (h.Routes) against PostgreSQL. Skipped without UZI_TEST_DATABASE_URL.

type envFixture struct {
	t      *testing.T
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool
	owner  uuid.UUID
	repo   uuid.UUID
	jwt    string
}

// newEnvFixture builds an unprotected owner (master-sealed, never locked) with one repo.
func newEnvFixture(t *testing.T) *envFixture {
	t.Helper()
	h, router, pool := cliVaultLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	return &envFixture{t: t, h: h, router: router, pool: pool, owner: owner,
		repo: cliSeedOwnedRepo(t, pool, owner), jwt: cliMintJWT(t, pool, owner)}
}

func (f *envFixture) path(suffix string) string {
	return "/api/repos/" + f.repo.String() + "/env" + suffix
}

func (f *envFixture) do(method, suffix, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	if method == http.MethodGet {
		return cookieReq(f.t, f.router, method, f.path(suffix), f.jwt, "")
	}
	return cookieReq(f.t, f.router, method, f.path(suffix), f.jwt, body)
}

func (f *envFixture) must(rec *httptest.ResponseRecorder, want int) {
	f.t.Helper()
	if rec.Code != want {
		body := rec.Body.String()
		if len(body) > 300 {
			body = body[:300] + "..."
		}
		f.t.Fatalf("status = %d, want %d; body: %s", rec.Code, want, body)
	}
}

func (f *envFixture) put(name, body string, want int) apitypes.RunEnvVarDTO {
	f.t.Helper()
	rec := f.do(http.MethodPut, "/"+name, body)
	f.must(rec, want)
	var out apitypes.RunEnvWriteDTO
	if want < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			f.t.Fatal(err)
		}
	}
	return out.Variable
}

func (f *envFixture) list() apitypes.RunEnvListDTO {
	f.t.Helper()
	rec := f.do(http.MethodGet, "", "")
	f.must(rec, http.StatusOK)
	var out apitypes.RunEnvListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *envFixture) find(l apitypes.RunEnvListDTO, name string) *apitypes.RunEnvVarDTO {
	for i := range l.Variables {
		if l.Variables[i].Name == name {
			return &l.Variables[i]
		}
	}
	return nil
}

func (f *envFixture) rowCount(where string, args ...any) int {
	f.t.Helper()
	var n int
	q := `SELECT count(*) FROM run_env_vars WHERE user_id=$1 AND repo_id=$2 AND ` + where
	if err := f.pool.QueryRow(context.Background(), q, append([]any{f.owner, f.repo}, args...)...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// rowSnapshot reads every stored column a refused write could alter, for the named
// variables of the fixture repo, rendered as one comparable string per name.
func (f *envFixture) rowSnapshot(names ...string) map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for _, n := range names {
		var (
			id, varID  uuid.UUID
			ct         []byte
			desc       string
			disabledAt *time.Time
			rev        int
		)
		err := f.pool.QueryRow(context.Background(),
			`SELECT id, var_id, ciphertext, description, disabled_at, rev FROM run_env_vars WHERE repo_id=$1 AND name=$2 AND retired_at IS NULL`,
			f.repo, n).Scan(&id, &varID, &ct, &desc, &disabledAt, &rev)
		if err != nil {
			f.t.Fatalf("snapshot %s: %v", n, err)
		}
		out[n] = fmt.Sprintf("id=%s var_id=%s ct=%x desc=%q disabled_at=%v rev=%d", id, varID, ct, desc, disabledAt, rev)
	}
	return out
}

func (f *envFixture) requireSnapshot(before map[string]string, after string) {
	f.t.Helper()
	now := f.rowSnapshot("PAYMENTS_SANDBOX_KEY", "PAYMENTS_SANDBOX_URL")
	for n, b := range before {
		if now[n] != b {
			f.t.Fatalf("%s changed by %s:\n before %s\n after  %s", n, after, b, now[n])
		}
	}
}

// envSecondRepo adds another repo on the same forge connection as repo (a user has one
// connection per forge and base URL) and returns its id.
func envSecondRepo(t *testing.T, pool *pgxpool.Pool, repo uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 SELECT $1, connection_id, 2, 'g/second', 'https://forge.e2e/g/second', 'main', true FROM repos WHERE id = $2`, id, repo)
	return id
}

func jsonBody(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// secretFixture is a dummy credential shaped like nothing a scanner knows.
func secretFixture(n int) string { return fmt.Sprintf("dummy-secret-value-%03d", n) }

func TestRunEnvAuthDifferentialLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	token := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)
	other := cliSeedUser(t, f.pool, false)
	otherToken := cliMintToken(t, f.pool, other, clitoken.ScopeUser)
	f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("PAYMENTS_SANDBOX_URL", jsonBody(map[string]any{"value": "https://sandbox.example.test", "secret": false}), http.StatusCreated)

	// A CLI Bearer token reads the list, with plain values and no secret value.
	rec := bearerReq(f.router, http.MethodGet, f.path(""), token)
	f.must(rec, http.StatusOK)
	var l apitypes.RunEnvListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	if v := f.find(l, "PAYMENTS_SANDBOX_URL"); v == nil || v.Value == nil || *v.Value != "https://sandbox.example.test" {
		t.Fatalf("a Bearer list lost the plain value: %+v", v)
	}
	if strings.Contains(rec.Body.String(), secretFixture(1)) {
		t.Fatal("the Bearer list carried a secret value")
	}

	before := f.rowSnapshot("PAYMENTS_SANDBOX_KEY", "PAYMENTS_SANDBOX_URL")

	// Every write is cookie-only: a Bearer token is refused before the handler runs and
	// nothing changes.
	for _, c := range []struct{ method, suffix, body string }{
		{http.MethodPut, "/NEW_ONE", `{"value":"abcdefgh12345"}`},
		{http.MethodPut, "/PAYMENTS_SANDBOX_KEY", `{"description":"changed"}`},
		{http.MethodPost, "/PAYMENTS_SANDBOX_KEY/disable", ``},
		{http.MethodPost, "/PAYMENTS_SANDBOX_KEY/enable", ``},
		{http.MethodDelete, "/PAYMENTS_SANDBOX_KEY", ``},
	} {
		rec := bearerReqBody(f.router, c.method, f.path(c.suffix), token, c.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Bearer %s %s = %d, want 401; body: %s", c.method, c.suffix, rec.Code, rec.Body.String())
		}
	}
	if got := f.rowCount("true"); got != 2 {
		t.Fatalf("rows after refused writes = %d, want 2", got)
	}
	f.requireSnapshot(before, "the refused Bearer writes")

	// No credential at all, and a cookie without the CSRF header.
	f.must(bearerReq(f.router, http.MethodGet, f.path(""), ""), http.StatusUnauthorized)
	noCSRF := func(method, suffix, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, f.path(suffix), strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: f.jwt}) //nolint:gosec // G124: test-only client cookie.
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}
	if rec := noCSRF(http.MethodPut, "/NO_CSRF", `{"value":"abcdefgh12345"}`); rec.Code < 400 || f.rowCount("name='NO_CSRF'") != 0 {
		t.Errorf("cookie PUT without CSRF = %d (rows %d)", rec.Code, f.rowCount("name='NO_CSRF'"))
	}
	// The other cookie writers on an existing variable are refused too, and change nothing.
	for _, c := range []struct{ method, suffix string }{
		{http.MethodPost, "/PAYMENTS_SANDBOX_KEY/disable"},
		{http.MethodPost, "/PAYMENTS_SANDBOX_KEY/enable"},
		{http.MethodDelete, "/PAYMENTS_SANDBOX_KEY"},
	} {
		if rec := noCSRF(c.method, c.suffix, ""); rec.Code < 400 {
			t.Errorf("cookie %s %s without CSRF = %d, want >= 400", c.method, c.suffix, rec.Code)
		}
	}
	if got := f.rowCount("name='PAYMENTS_SANDBOX_KEY' AND disabled_at IS NULL AND pending_delete_at IS NULL"); got != 1 || f.rowCount("true") != 2 {
		t.Fatalf("a CSRF-less write changed the variable: live rows %d, total %d", got, f.rowCount("true"))
	}
	f.requireSnapshot(before, "the CSRF-less writes")

	// Owner only: another user's token sees a 404, not the list.
	f.must(bearerReq(f.router, http.MethodGet, f.path(""), otherToken), http.StatusNotFound)
	foreign := cookieReq(t, f.router, http.MethodPut, f.path("/FOREIGN_WRITE"), cliMintJWT(t, f.pool, other), `{"value":"abcdefgh12345"}`)
	f.must(foreign, http.StatusNotFound)
	f.must(cookieReq(t, f.router, http.MethodDelete, f.path("/PAYMENTS_SANDBOX_KEY"), cliMintJWT(t, f.pool, other), ""), http.StatusNotFound)
	if got := f.rowCount("true"); got != 2 {
		t.Fatalf("rows after foreign writes = %d, want 2", got)
	}
	f.requireSnapshot(before, "the foreign-user writes")
}

func TestRunEnvLifecycleLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	secret := secretFixture(1)

	// Create a secret (the default) and a plain variable, each with a description.
	created := f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"value": secret, "description": "sandbox key"}), http.StatusCreated)
	if !created.Secret || created.Rev != 1 || created.Value != nil || created.State != apitypes.RunEnvStateEnabled {
		t.Fatalf("created secret = %+v", created)
	}
	plain := f.put("PAYMENTS_SANDBOX_URL", jsonBody(map[string]any{"value": "https://sandbox.example.test", "secret": false, "description": "sandbox base url"}), http.StatusCreated)
	if plain.Secret || plain.Value == nil || *plain.Value != "https://sandbox.example.test" {
		t.Fatalf("created plain = %+v", plain)
	}
	rec := f.do(http.MethodGet, "", "")
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("the list response carries a secret value")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the list is cacheable")
	}
	l := f.list()
	if l.Counts != (apitypes.RunEnvCountsDTO{Total: 2, Disabled: 0, PendingDeletion: 0, Limit: 50}) || l.VaultLocked || len(l.Variables) != 2 {
		t.Fatalf("list = %+v", l)
	}
	if k := f.find(l, "PAYMENTS_SANDBOX_KEY"); k == nil || k.Value != nil || k.ValueWithheld || k.Description != "sandbox key" {
		t.Fatalf("secret in list = %+v", k)
	}
	// The ciphertext at rest holds the value, not the other way around.
	var ct []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT ciphertext FROM run_env_vars WHERE name='PAYMENTS_SANDBOX_KEY' AND repo_id=$1`, f.repo).Scan(&ct); err != nil || strings.Contains(string(ct), secret) {
		t.Fatalf("stored ciphertext: %v / plaintext visible: %v", err, strings.Contains(string(ct), secret))
	}

	// A description-only save changes the live row in place: same row, same rev.
	var rowID uuid.UUID
	_ = f.pool.QueryRow(context.Background(), `SELECT id FROM run_env_vars WHERE name='PAYMENTS_SANDBOX_KEY' AND repo_id=$1`, f.repo).Scan(&rowID)
	edited := f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"description": "rotated nightly"}), http.StatusOK)
	var rowAfter uuid.UUID
	_ = f.pool.QueryRow(context.Background(), `SELECT id FROM run_env_vars WHERE name='PAYMENTS_SANDBOX_KEY' AND repo_id=$1`, f.repo).Scan(&rowAfter)
	if edited.Rev != 1 || edited.Description != "rotated nightly" || rowAfter != rowID || f.rowCount("name='PAYMENTS_SANDBOX_KEY'") != 1 {
		t.Fatalf("description-only save wrote a version: %+v (row %s -> %s)", edited, rowID, rowAfter)
	}
	// An empty body on an existing variable is a no-op, not a rotation.
	if noop := f.put("PAYMENTS_SANDBOX_KEY", `{}`, http.StatusOK); noop.Rev != 1 {
		t.Fatalf("empty save changed rev: %+v", noop)
	}

	// A value writes rev+1 under the same var_id; with no receipt the previous version is
	// dropped, not kept.
	var varID uuid.UUID
	_ = f.pool.QueryRow(context.Background(), `SELECT var_id FROM run_env_vars WHERE name='PAYMENTS_SANDBOX_KEY' AND repo_id=$1`, f.repo).Scan(&varID)
	rotated := f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusOK)
	if rotated.Rev != 2 || rotated.Description != "rotated nightly" {
		t.Fatalf("rotated = %+v; the description must carry over", rotated)
	}
	var revs int
	var gotVar uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*), min(var_id::text)::uuid FROM run_env_vars WHERE name='PAYMENTS_SANDBOX_KEY' AND repo_id=$1`, f.repo).Scan(&revs, &gotVar); err != nil || revs != 1 || gotVar != varID {
		t.Fatalf("after rotation: %d rows, var_id %s (want 1 row, %s): %v", revs, gotVar, varID, err)
	}

	// secret -> plain needs a value in the same save.
	f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"secret": false}), http.StatusUnprocessableEntity)
	toPlain := f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"secret": false, "value": "now-a-plain-value"}), http.StatusOK)
	if toPlain.Secret || toPlain.Rev != 3 || toPlain.Value == nil || *toPlain.Value != "now-a-plain-value" {
		t.Fatalf("secret to plain = %+v", toPlain)
	}
	// plain -> secret reseals the stored value as a new version; the value is readable
	// to the vault, never to the response.
	toSecret := f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"secret": true}), http.StatusOK)
	if !toSecret.Secret || toSecret.Rev != 4 || toSecret.Value != nil {
		t.Fatalf("plain to secret = %+v", toSecret)
	}
	var sealed []byte
	var marker string
	if err := f.pool.QueryRow(context.Background(), `SELECT ciphertext, sealed_with FROM run_env_vars WHERE name='PAYMENTS_SANDBOX_KEY' AND repo_id=$1`, f.repo).Scan(&sealed, &marker); err != nil {
		t.Fatal(err)
	}
	opened, err := f.h.vault.Open(f.owner, runenv.AADKind(runenv.ScopeRepo, &f.repo, true, "PAYMENTS_SANDBOX_KEY"), marker, sealed)
	if err != nil || string(opened) != "now-a-plain-value" {
		t.Fatalf("resealed value = %q, %v", opened, err)
	}
	// A stored plain value too short to be a secret cannot be flipped.
	f.put("SHORT_PLAIN", jsonBody(map[string]any{"value": "1234567", "secret": false}), http.StatusCreated)
	f.put("SHORT_PLAIN", jsonBody(map[string]any{"secret": true}), http.StatusUnprocessableEntity)

	// Disable and enable are metadata-only; a rotation keeps a disabled variable disabled.
	off := f.do(http.MethodPost, "/PAYMENTS_SANDBOX_KEY/disable", "")
	f.must(off, http.StatusOK)
	if k := f.find(f.list(), "PAYMENTS_SANDBOX_KEY"); k.State != apitypes.RunEnvStateDisabled || k.Rev != 4 || k.DisabledAt == nil {
		t.Fatalf("after disable = %+v", k)
	}
	if l := f.list(); l.Counts.Disabled != 1 {
		t.Fatalf("counts = %+v", l.Counts)
	}
	if kept := f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"value": secretFixture(5)}), http.StatusOK); kept.State != apitypes.RunEnvStateDisabled || kept.Rev != 5 {
		t.Fatalf("rotation of a disabled variable = %+v", kept)
	}
	f.must(f.do(http.MethodPost, "/PAYMENTS_SANDBOX_KEY/enable", ""), http.StatusOK)
	if k := f.find(f.list(), "PAYMENTS_SANDBOX_KEY"); k.State != apitypes.RunEnvStateEnabled || k.DisabledAt != nil {
		t.Fatalf("after enable = %+v", k)
	}
	f.must(f.do(http.MethodPost, "/MISSING_ONE/disable", ""), http.StatusNotFound)

	// Delete drops at once (no receipts in M1), frees the name, and a re-create is a
	// different variable.
	del := f.do(http.MethodDelete, "/PAYMENTS_SANDBOX_KEY", "")
	f.must(del, http.StatusOK)
	var d apitypes.RunEnvDeleteDTO
	_ = json.Unmarshal(del.Body.Bytes(), &d)
	if d.State != "deleted" || f.rowCount("name='PAYMENTS_SANDBOX_KEY'") != 0 {
		t.Fatalf("delete = %+v, rows %d", d, f.rowCount("name='PAYMENTS_SANDBOX_KEY'"))
	}
	f.must(f.do(http.MethodDelete, "/PAYMENTS_SANDBOX_KEY", ""), http.StatusNotFound)
	if again := f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"value": secretFixture(6)}), http.StatusCreated); again.Rev != 1 {
		t.Fatalf("re-created = %+v", again)
	}
	var newVar uuid.UUID
	_ = f.pool.QueryRow(context.Background(), `SELECT var_id FROM run_env_vars WHERE name='PAYMENTS_SANDBOX_KEY' AND repo_id=$1`, f.repo).Scan(&newVar)
	if newVar == varID {
		t.Fatal("a delete then re-create reused the var_id")
	}

	// The repo row carries the variable count.
	rec = cookieReq(t, f.router, http.MethodGet, "/api/repos/", f.jwt, "")
	f.must(rec, http.StatusOK)
	var repos struct {
		Repos []apitypes.RepoDTO `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &repos); err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range repos.Repos {
		if r.ID == f.repo.String() {
			seen = true
			if r.EnvVarCount != 3 {
				t.Errorf("env_var_count = %d, want 3 (the key, the url and the short plain one)", r.EnvVarCount)
			}
		}
	}
	if !seen {
		t.Fatal("the repo is not in the list")
	}
}

// A variable stored under a name that was denied later can still be disabled and
// deleted, but never enabled or edited (PRD #4416).
func TestRunEnvDeniedNameDisableDeleteLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("LEGACY_TMP", jsonBody(map[string]any{"value": "https://legacy.example.test", "secret": false}), http.StatusCreated)
	// Rename the stored row to an already-denied name, bypassing PUT validation.
	cliMustExec(t, f.pool, `UPDATE run_env_vars SET name='TMPDIR' WHERE repo_id=$1 AND name='LEGACY_TMP'`, f.repo)

	f.must(f.do(http.MethodPost, "/TMPDIR/disable", ""), http.StatusOK)
	if n := f.rowCount("name='TMPDIR' AND disabled_at IS NOT NULL AND retired_at IS NULL"); n != 1 {
		t.Fatalf("disabled rows = %d, want 1", n)
	}
	f.must(f.do(http.MethodPost, "/TMPDIR/enable", ""), http.StatusUnprocessableEntity)
	f.must(f.do(http.MethodPut, "/TMPDIR", jsonBody(map[string]any{"description": "x"})), http.StatusUnprocessableEntity)
	if n := f.rowCount("name='TMPDIR' AND disabled_at IS NOT NULL"); n != 1 {
		t.Fatalf("a refused enable or PUT changed the row: %d disabled rows", n)
	}
	f.must(f.do(http.MethodDelete, "/TMPDIR", ""), http.StatusOK)
	if n := f.rowCount("name='TMPDIR'"); n != 0 {
		t.Fatalf("rows after delete = %d, want 0", n)
	}
}

func TestRunEnvValueAndNameRulesLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	body := func(m map[string]any) string { return jsonBody(m) }

	// Denied names are refused with an error naming the rule.
	for name, rule := range map[string]string{
		"ANTHROPIC_BASE_URL": "denied prefix ANTHROPIC_",
		"NODE_OPTIONS":       "NODE_OPTIONS",
		"GITHUB_TOKEN":       "denied prefix GITHUB_",
		"PATH":               "PATH",
		"lower_case":         "^[A-Z_][A-Z0-9_]{0,127}$",
		"HTTPS_PROXY":        "denied suffix _PROXY",
	} {
		rec := f.do(http.MethodPut, "/"+name, body(map[string]any{"value": secretFixture(1)}))
		f.must(rec, http.StatusUnprocessableEntity)
		var e struct {
			Error, Field, Rule string
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Field != "name" || !strings.Contains(e.Error, rule) {
			t.Errorf("%s: error = %q (field %q), want the rule %q named", name, e.Error, e.Field, rule)
		}
	}
	if f.rowCount("true") != 0 {
		t.Fatal("a refused name was stored")
	}

	// Values: NUL refused, empty plain accepted, 7-char secret refused, 8 accepted.
	f.put("NUL_VALUE", body(map[string]any{"value": "abc\u0000defgh", "secret": false}), http.StatusUnprocessableEntity)
	f.put("NUL_SECRET", body(map[string]any{"value": "abcdefgh\u0000"}), http.StatusUnprocessableEntity)
	if e := f.put("EMPTY_PLAIN", body(map[string]any{"value": "", "secret": false}), http.StatusCreated); e.Value == nil || *e.Value != "" {
		t.Fatalf("empty plain = %+v", e)
	}
	f.put("EMPTY_SECRET", body(map[string]any{"value": ""}), http.StatusUnprocessableEntity)
	f.put("SEVEN", body(map[string]any{"value": "1234567"}), http.StatusUnprocessableEntity)
	f.put("EIGHT", body(map[string]any{"value": "12345678"}), http.StatusCreated)
	f.put("SEVEN_PLAIN", body(map[string]any{"value": "1234567", "secret": false}), http.StatusCreated)
	// UTF-16 length: three astral runes are 6 units, four are 8.
	f.put("ASTRAL_THREE", body(map[string]any{"value": "\U0001F600\U0001F600\U0001F600"}), http.StatusUnprocessableEntity)
	f.put("ASTRAL_FOUR", body(map[string]any{"value": "\U0001F600\U0001F600\U0001F600\U0001F600"}), http.StatusCreated)
	// A plain value that is a URL with userinfo is refused; the same string saves as a secret.
	const dsn = "postgres://app:dummy-pass@db.example.test/app" //nolint:gosec // G101: a dummy userinfo URL, the exact shape the plain-value rule must refuse.
	f.put("DATABASE_URL", body(map[string]any{"value": dsn, "secret": false}), http.StatusUnprocessableEntity)
	f.put("DATABASE_URL", body(map[string]any{"value": dsn}), http.StatusCreated)
	f.put("HOST_ONLY", body(map[string]any{"value": "db.example.test", "secret": false}), http.StatusCreated)
	// Sizes at and one past the limit.
	f.put("BIG_OK", body(map[string]any{"value": strings.Repeat("a", 65536), "secret": false}), http.StatusCreated)
	f.put("BIG_BAD", body(map[string]any{"value": strings.Repeat("a", 65537), "secret": false}), http.StatusUnprocessableEntity)
	f.put("DESC_OK", body(map[string]any{"value": "12345678", "description": strings.Repeat("d", 1024)}), http.StatusCreated)
	f.put("DESC_BAD", body(map[string]any{"value": "12345678", "description": strings.Repeat("d", 1025)}), http.StatusUnprocessableEntity)
	// Malformed bodies, unknown fields, and a create without a value.
	f.put("BAD_JSON", `{"value":`, http.StatusBadRequest)
	f.put("UNKNOWN_FIELD", `{"value":"12345678","nope":1}`, http.StatusBadRequest)
	f.put("NO_VALUE", `{"description":"x"}`, http.StatusUnprocessableEntity)
	// A validation failure never echoes the value.
	rec := f.do(http.MethodPut, "/ECHO_CHECK", body(map[string]any{"value": "short"}))
	f.must(rec, http.StatusUnprocessableEntity)
	if strings.Contains(rec.Body.String(), "short\"") {
		t.Errorf("the error echoes the value: %s", rec.Body.String())
	}
	// A failed rotation changes nothing.
	f.put("EIGHT", body(map[string]any{"value": "short"}), http.StatusUnprocessableEntity)
	if k := f.find(f.list(), "EIGHT"); k.Rev != 1 {
		t.Fatalf("a refused rotation moved rev: %+v", k)
	}
}

func TestRunEnvScopeLimitsLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	conflictCode := func(rec *httptest.ResponseRecorder) string {
		var e struct{ Code string }
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return e.Code
	}
	// Count: 50 fit, the 51st is refused with env_limit_reached.
	for i := range runenv.MaxVarsPerScope {
		f.put(fmt.Sprintf("VAR_%02d", i), jsonBody(map[string]any{"value": "v", "secret": false}), http.StatusCreated)
	}
	rec := f.do(http.MethodPut, "/ONE_TOO_MANY", jsonBody(map[string]any{"value": "v", "secret": false}))
	f.must(rec, http.StatusConflict)
	if conflictCode(rec) != "env_limit_reached" {
		t.Fatalf("51st variable: %s", rec.Body.String())
	}
	if f.rowCount("true") != 50 {
		t.Fatalf("rows = %d", f.rowCount("true"))
	}
	// Rotating an existing one at the limit is not a create.
	f.put("VAR_00", jsonBody(map[string]any{"value": "w", "secret": false}), http.StatusOK)
	// Disabled variables and ones pending deletion still count; deleting frees a slot.
	f.must(f.do(http.MethodPost, "/VAR_01/disable", ""), http.StatusOK)
	f.must(f.do(http.MethodPut, "/ONE_TOO_MANY", jsonBody(map[string]any{"value": "v", "secret": false})), http.StatusConflict)
	f.must(f.do(http.MethodDelete, "/VAR_02", ""), http.StatusOK)
	f.put("ONE_TOO_MANY", jsonBody(map[string]any{"value": "v", "secret": false}), http.StatusCreated)

	// Bytes: a second scope (another repo) is independent; 4 x 65536 fills 262144 exactly.
	g := newEnvFixture(t)
	for i := range 4 {
		g.put(fmt.Sprintf("BIG_%d", i), jsonBody(map[string]any{"value": strings.Repeat("a", 65536), "secret": false}), http.StatusCreated)
	}
	rec = g.do(http.MethodPut, "/ONE_BYTE_OVER", jsonBody(map[string]any{"value": "a", "secret": false}))
	g.must(rec, http.StatusConflict)
	if conflictCode(rec) != "env_limit_reached" {
		t.Fatalf("value-byte limit: %s", rec.Body.String())
	}
	// Shrinking one makes room for exactly the freed bytes, no more.
	g.put("BIG_0", jsonBody(map[string]any{"value": strings.Repeat("a", 65535), "secret": false}), http.StatusOK)
	g.put("FITS_EXACTLY", jsonBody(map[string]any{"value": "a", "secret": false}), http.StatusCreated)
	// Growing a rotation past the scope total is refused too; the same size is not growth.
	g.must(g.do(http.MethodPut, "/BIG_0", jsonBody(map[string]any{"value": strings.Repeat("b", 65536), "secret": false})), http.StatusConflict)
	g.put("BIG_1", jsonBody(map[string]any{"value": strings.Repeat("b", 65536), "secret": false}), http.StatusOK)
}

// withReceipts makes every version look receipted, to reach the paths M2 will use.
func (f *envFixture) withReceipts() {
	f.h.runEnvHasReceipt = func(context.Context, uuid.UUID, int32) (bool, error) { return true, nil }
}

func TestRunEnvRetiredVersionsAndHistoryBudgetLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.withReceipts()
	code := func(rec *httptest.ResponseRecorder) string {
		var e struct{ Code string }
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return e.Code
	}
	f.put("ROTATE_ME", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("ROTATE_ME", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusOK)
	f.put("ROTATE_ME", jsonBody(map[string]any{"value": secretFixture(3)}), http.StatusOK)
	// A receipted secret version is retired, not dropped: history rows stay sealed, are
	// never listed, and only the latest is live.
	if f.rowCount("name='ROTATE_ME'") != 3 || f.rowCount("name='ROTATE_ME' AND retired_at IS NOT NULL") != 2 {
		t.Fatalf("rows=%d retired=%d", f.rowCount("name='ROTATE_ME'"), f.rowCount("name='ROTATE_ME' AND retired_at IS NOT NULL"))
	}
	l := f.list()
	if len(l.Variables) != 1 || l.Variables[0].Rev != 3 || l.Counts.Total != 1 {
		t.Fatalf("list with retired history = %+v", l)
	}
	// A plain version is never retired, even when receipted.
	f.put("PLAIN_ONE", jsonBody(map[string]any{"value": "one", "secret": false}), http.StatusCreated)
	f.put("PLAIN_ONE", jsonBody(map[string]any{"value": "two", "secret": false}), http.StatusOK)
	if f.rowCount("name='PLAIN_ONE'") != 1 {
		t.Fatal("a plain version was retained")
	}
	// secret -> plain retires the secret side.
	f.put("ROTATE_ME", jsonBody(map[string]any{"secret": false, "value": "plain-now"}), http.StatusOK)
	if f.rowCount("name='ROTATE_ME' AND retired_at IS NOT NULL") != 3 {
		t.Fatalf("secret to plain did not retire the secret version")
	}

	// Budget: count. Pad the per-user retired history to 199, then one more rotation fits;
	// the next one is refused with env_history_full and changes nothing.
	pad := func(n int, valueBytes int) {
		for range n {
			if _, err := f.pool.Exec(context.Background(), `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev,retired_at)
 VALUES ($1,$2,$3,'PADDING',true,'\x01','master_aad',$4,1,now())`, uuid.New(), f.owner, f.repo, valueBytes); err != nil {
				t.Fatal(err)
			}
		}
	}
	var retired int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_env_vars WHERE user_id=$1 AND retired_at IS NOT NULL`, f.owner).Scan(&retired)
	pad(runenv.MaxRetiredVersions-1-retired, 1)
	f.put("SECOND", jsonBody(map[string]any{"value": secretFixture(10)}), http.StatusCreated)
	f.put("SECOND", jsonBody(map[string]any{"value": secretFixture(11)}), http.StatusOK) // the 200th retired version
	rec := f.do(http.MethodPut, "/SECOND", jsonBody(map[string]any{"value": secretFixture(12)}))
	f.must(rec, http.StatusConflict)
	if code(rec) != "env_history_full" {
		t.Fatalf("201st retired version: %s", rec.Body.String())
	}
	if k := f.find(f.list(), "SECOND"); k.Rev != 2 {
		t.Fatalf("a refused rotation changed the variable: %+v", k)
	}

	// With the budget full, Disable and Delete are never refused. Delete of a receipted
	// secret answers 202 pending_deletion, the name is free at once and re-creates as a
	// new var_id, and the pending row still counts toward the scope limit.
	f.must(f.do(http.MethodPost, "/SECOND/disable", ""), http.StatusOK)
	f.must(f.do(http.MethodPost, "/SECOND/enable", ""), http.StatusOK)
	var oldVar uuid.UUID
	_ = f.pool.QueryRow(context.Background(), `SELECT var_id FROM run_env_vars WHERE name='SECOND' AND retired_at IS NULL AND repo_id=$1`, f.repo).Scan(&oldVar)
	del := f.do(http.MethodDelete, "/SECOND", "")
	f.must(del, http.StatusAccepted)
	var d apitypes.RunEnvDeleteDTO
	_ = json.Unmarshal(del.Body.Bytes(), &d)
	if d.State != apitypes.RunEnvStatePendingDeletion {
		t.Fatalf("delete state = %q", d.State)
	}
	l = f.list()
	pending := 0
	for _, v := range l.Variables {
		if v.Name == "SECOND" && v.State == apitypes.RunEnvStatePendingDeletion {
			pending++
			if v.Value != nil {
				t.Fatal("a pending-deletion secret carries a value")
			}
		}
	}
	if pending != 1 || l.Counts.PendingDeletion != 1 {
		t.Fatalf("pending in list: %d, counts %+v", pending, l.Counts)
	}
	f.put("SECOND", jsonBody(map[string]any{"value": secretFixture(20)}), http.StatusCreated)
	var newVar uuid.UUID
	_ = f.pool.QueryRow(context.Background(), `SELECT var_id FROM run_env_vars WHERE name='SECOND' AND retired_at IS NULL AND pending_delete_at IS NULL AND repo_id=$1`, f.repo).Scan(&newVar)
	if newVar == oldVar {
		t.Fatal("a re-created name reused the var_id")
	}
	// ROTATE_ME, PLAIN_ONE, the re-created SECOND and the pending SECOND all count toward the limit.
	if l := f.list(); l.Counts.Total != 4 || l.Counts.PendingDeletion != 1 {
		t.Fatalf("counts = %+v", l.Counts)
	}
}

func TestRunEnvRetiredByteBudgetBoundaryLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.withReceipts()
	f.put("BIG_SECRET", jsonBody(map[string]any{"value": strings.Repeat("s", 1000)}), http.StatusCreated)
	// Retire history up to (limit - the live value's bytes), 65536 bytes per row at most.
	remaining := runenv.MaxRetiredValueBytes - 1000
	for remaining > 0 {
		n := min(remaining, 65536)
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev,retired_at)
 VALUES ($1,$2,$3,'PADDING',true,'\x01','master_aad',$4,1,now())`, uuid.New(), f.owner, f.repo, n); err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	// 2097152 - 1000 retired + 1000 live = exactly the limit once the live one retires.
	f.put("BIG_SECRET", jsonBody(map[string]any{"value": strings.Repeat("t", 1000)}), http.StatusOK)
	// The new live version has 1000 bytes too: one more rotation is one past the byte limit.
	rec := f.do(http.MethodPut, "/BIG_SECRET", jsonBody(map[string]any{"value": strings.Repeat("u", 1000)}))
	f.must(rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "env_history_full") {
		t.Fatalf("one byte-budget past: %s", rec.Body.String())
	}
}

func TestRunEnvLockedVaultLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t)
	owner := cliVaultUser(t, pool, false)
	cliVaultSeed(t, h, owner)
	repo := cliSeedOwnedRepo(t, pool, owner)
	jwt := cliMintJWT(t, pool, owner)
	f := &envFixture{t: t, h: h, router: router, pool: pool, owner: owner, repo: repo, jwt: jwt}

	if err := h.vault.Unlock(context.Background(), owner, cliVaultPassword); err != nil {
		t.Fatal(err)
	}
	f.put("PAYMENTS_SANDBOX_URL", jsonBody(map[string]any{"value": "https://sandbox.example.test", "secret": false}), http.StatusCreated)
	f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	if f.list().VaultLocked {
		t.Fatal("an unlocked vault reports locked")
	}
	h.vault.Lock(owner)

	rec := f.do(http.MethodPut, "/NEW_ONE", jsonBody(map[string]any{"value": secretFixture(2)}))
	f.must(rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), `"vault_locked"`) {
		t.Fatalf("locked write: %s", rec.Body.String())
	}
	rec = f.do(http.MethodPut, "/PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"value": secretFixture(3)}))
	f.must(rec, http.StatusConflict)
	// plain -> secret needs the stored value, so it needs an unlocked vault.
	f.must(f.do(http.MethodPut, "/PAYMENTS_SANDBOX_URL", jsonBody(map[string]any{"secret": true})), http.StatusConflict)
	if f.rowCount("true") != 2 {
		t.Fatal("a locked write changed rows")
	}

	// The list shows names and metadata with plain values withheld.
	l := f.list()
	if !l.VaultLocked || len(l.Variables) != 2 {
		t.Fatalf("locked list = %+v", l)
	}
	if u := f.find(l, "PAYMENTS_SANDBOX_URL"); u == nil || u.Value != nil || !u.ValueWithheld {
		t.Fatalf("locked plain = %+v", u)
	}
	// Metadata-only changes need no key.
	f.put("PAYMENTS_SANDBOX_KEY", jsonBody(map[string]any{"description": "still editable"}), http.StatusOK)
	f.must(f.do(http.MethodPost, "/PAYMENTS_SANDBOX_KEY/disable", ""), http.StatusOK)
	f.must(f.do(http.MethodDelete, "/PAYMENTS_SANDBOX_KEY", ""), http.StatusOK)
}

// A ciphertext moved to another name, repo or secret flag fails to open: the list
// withholds the value rather than serving another variable's.
func TestRunEnvCiphertextSwapDoesNotOpenLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("FIRST_PLAIN", jsonBody(map[string]any{"value": "first-plain-value", "secret": false}), http.StatusCreated)
	f.put("SECOND_PLAIN", jsonBody(map[string]any{"value": "second-plain-value", "secret": false}), http.StatusCreated)
	f.put("A_SECRET", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)

	swap := func(from, to string) {
		t.Helper()
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE run_env_vars t SET ciphertext = s.ciphertext, sealed_with = s.sealed_with FROM run_env_vars s
			 WHERE t.name=$2 AND s.name=$1 AND t.repo_id=$3 AND s.repo_id=$3`, from, to, f.repo); err != nil {
			t.Fatal(err)
		}
	}
	// Control: reading is fine before any swap.
	if v := f.find(f.list(), "SECOND_PLAIN"); v == nil || v.Value == nil || *v.Value != "second-plain-value" {
		t.Fatalf("control read: %+v", v)
	}
	other := envSecondRepo(t, f.pool, f.repo)
	swap("FIRST_PLAIN", "SECOND_PLAIN") // name swap
	l := f.list()
	if v := f.find(l, "SECOND_PLAIN"); v == nil || v.Value != nil || !v.ValueWithheld || l.VaultLocked {
		t.Fatalf("a plain value moved to another name opened: %+v (locked=%v)", v, l.VaultLocked)
	}
	// secret -> plain row: relabelling a secret ciphertext as plain does not open either.
	swap("A_SECRET", "FIRST_PLAIN")
	if v := f.find(f.list(), "FIRST_PLAIN"); v == nil || v.Value != nil || !v.ValueWithheld {
		t.Fatalf("a secret ciphertext relabelled plain opened: %+v", v)
	}
	// Repo swap: a row of another repo of the same owner and name.
	f.put("CROSS_REPO", jsonBody(map[string]any{"value": "cross-repo-value", "secret": false}), http.StatusCreated)
	g := &envFixture{t: t, h: f.h, router: f.router, pool: f.pool, owner: f.owner, repo: other, jwt: f.jwt}
	g.put("CROSS_REPO", jsonBody(map[string]any{"value": "other-repo-value", "secret": false}), http.StatusCreated)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE run_env_vars t SET ciphertext = s.ciphertext, sealed_with = s.sealed_with FROM run_env_vars s
		 WHERE t.name='CROSS_REPO' AND s.name='CROSS_REPO' AND t.repo_id=$1 AND s.repo_id=$2`, f.repo, other); err != nil {
		t.Fatal(err)
	}
	if v := f.find(f.list(), "CROSS_REPO"); v == nil || v.Value != nil || !v.ValueWithheld {
		t.Fatalf("a ciphertext moved to another repo opened: %+v", v)
	}
}

// Enabling and disabling the vault, and an account reset, with env rows present: every
// row opens afterwards (or is gone), retired rows included, and the totals count them.
func TestRunEnvVaultLifecycleLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t)
	vaultFixturePolicy(t, pool, "optional")
	owner := cliSeedUser(t, pool, false)          // unprotected: master_aad-sealed
	const password = "a-vault-lifecycle-password" //nolint:gosec // G101: test-only vault password.
	setVaultFixturePassword(t, pool, owner, password)
	repo := cliSeedOwnedRepo(t, pool, owner)
	f := &envFixture{t: t, h: h, router: router, pool: pool, owner: owner, repo: repo, jwt: cliMintJWT(t, pool, owner)}
	f.withReceipts()

	f.put("LIVE_SECRET", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("LIVE_SECRET", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusOK) // retires rev 1
	f.put("LIVE_PLAIN", jsonBody(map[string]any{"value": "https://sandbox.example.test", "secret": false}), http.StatusCreated)
	f.put("DISABLED_SECRET", jsonBody(map[string]any{"value": secretFixture(3)}), http.StatusCreated)
	f.must(f.do(http.MethodPost, "/DISABLED_SECRET/disable", ""), http.StatusOK)
	f.put("PENDING_SECRET", jsonBody(map[string]any{"value": secretFixture(4)}), http.StatusCreated)
	f.must(f.do(http.MethodDelete, "/PENDING_SECRET", ""), http.StatusAccepted)
	if _, err := pool.Exec(context.Background(), `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev)
 SELECT gen_random_uuid(),user_id,NULL,'USER_SCOPE_SECRET',true,ciphertext,sealed_with,value_bytes,1 FROM run_env_vars WHERE name='LIVE_SECRET' AND retired_at IS NULL AND user_id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	// The cloned user-scope row carries the wrong AAD (a different name/scope): it must be
	// re-sealed properly, so replace it with a correctly sealed one.
	userScope, err := h.vault.Seal(context.Background(), owner, runenv.AADKind(runenv.ScopeUser, nil, true, "USER_SCOPE_SECRET"), []byte(secretFixture(5)), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(context.Background(), t, pool, `UPDATE run_env_vars SET ciphertext=$2, sealed_with=$3 WHERE user_id=$1 AND name='USER_SCOPE_SECRET'`, owner, userScope.Ciphertext, userScope.SealedWith)

	type rowInfo struct {
		id     uuid.UUID
		repo   *uuid.UUID
		name   string
		secret bool
		rev    int
		ct     []byte
		marker string
	}
	load := func() []rowInfo {
		rows, err := pool.Query(context.Background(), `SELECT id, repo_id, name, secret, rev, ciphertext, sealed_with FROM run_env_vars WHERE user_id=$1 ORDER BY id`, owner)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []rowInfo
		for rows.Next() {
			var r rowInfo
			if err := rows.Scan(&r.id, &r.repo, &r.name, &r.secret, &r.rev, &r.ct, &r.marker); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	// Plaintext originally written, keyed by "name#rev": the retired LIVE_SECRET rev 1
	// and its current rev 2, the other secrets at rev 1, and the plain value.
	wantPlain := map[string]string{
		"LIVE_SECRET#1":       secretFixture(1),
		"LIVE_SECRET#2":       secretFixture(2),
		"LIVE_PLAIN#1":        "https://sandbox.example.test",
		"DISABLED_SECRET#1":   secretFixture(3),
		"PENDING_SECRET#1":    secretFixture(4),
		"USER_SCOPE_SECRET#1": secretFixture(5),
	}
	opensAll := func(want string) {
		t.Helper()
		rows := load()
		if len(rows) != 6 {
			t.Fatalf("rows = %d, want 6 (live, retired, disabled, pending, plain, user scope)", len(rows))
		}
		seen := map[string]bool{}
		for _, r := range rows {
			if r.marker != want {
				t.Fatalf("%s sealed_with = %s, want %s", r.name, r.marker, want)
			}
			scope := runenv.ScopeUser
			if r.repo != nil {
				scope = runenv.ScopeRepo
			}
			pt, err := h.vault.Open(owner, runenv.AADKind(scope, r.repo, r.secret, r.name), r.marker, r.ct)
			if err != nil {
				t.Fatalf("%s does not open: %v", r.name, err)
			}
			key := fmt.Sprintf("%s#%d", r.name, r.rev)
			exp, ok := wantPlain[key]
			if !ok {
				t.Fatalf("unexpected row %s", key)
			}
			if string(pt) != exp {
				t.Fatalf("%s plaintext = %q, want %q", key, pt, exp)
			}
			seen[key] = true
		}
		if len(seen) != len(wantPlain) {
			t.Fatalf("rows seen %v, want all of %d expected name#rev keys", seen, len(wantPlain))
		}
	}
	opensAll(store.SealedWithMasterAAD)

	if err := h.vault.Enable(context.Background(), owner, password); err != nil {
		t.Fatalf("enable with env rows: %v", err)
	}
	opensAll(store.SealedWithDEK)
	if v := f.find(f.list(), "LIVE_PLAIN"); v == nil || v.Value == nil || *v.Value != "https://sandbox.example.test" {
		t.Fatalf("plain value after enable: %+v", v)
	}
	if err := h.vault.Disable(context.Background(), owner); err != nil {
		t.Fatalf("disable with env rows: %v", err)
	}
	opensAll(store.SealedWithMasterAAD)
	if v := f.find(f.list(), "LIVE_PLAIN"); v == nil || v.Value == nil {
		t.Fatalf("plain value after disable: %+v", v)
	}

	// Reset removes every env row of the owner (live, retired, pending, both scopes).
	if err := h.vault.Reset(context.Background(), owner, func(context.Context, vault.BrowserStore) error { return nil }); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := len(load()); got != 0 {
		t.Fatalf("%d env rows survived an account reset", got)
	}
}

func envConflictCode(rec *httptest.ResponseRecorder) string {
	var e struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Code
}

// A name freed by a pending deletion can be re-created as a new variable, but the
// pending row still counts toward the per-scope count and value-byte limits.
func TestRunEnvRecreateAgainstPendingDeletionLimitsLiveDB(t *testing.T) {
	secretBody := func(n int) string { return jsonBody(map[string]any{"value": secretFixture(n)}) }

	t.Run("count", func(t *testing.T) {
		f := newEnvFixture(t)
		f.withReceipts()
		for i := range runenv.MaxVarsPerScope {
			f.put(fmt.Sprintf("S_%02d", i), secretBody(i), http.StatusCreated)
		}
		f.must(f.do(http.MethodDelete, "/S_49", ""), http.StatusAccepted) // 49 live + 1 pending
		if f.rowCount("pending_delete_at IS NOT NULL") != 1 {
			t.Fatal("no pending-deletion row")
		}
		rec := f.do(http.MethodPut, "/S_49", secretBody(49))
		f.must(rec, http.StatusConflict)
		if envConflictCode(rec) != "env_limit_reached" {
			t.Fatalf("re-create at the limit: %s", rec.Body.String())
		}
		// One live variable fewer (it has no receipt, so it is dropped at once): 48 live + 1
		// pending fit, and the name comes back as a new variable.
		f.h.runEnvHasReceipt = func(context.Context, uuid.UUID, int32) (bool, error) { return false, nil }
		f.must(f.do(http.MethodDelete, "/S_00", ""), http.StatusOK)
		f.put("S_49", secretBody(49), http.StatusCreated)
		if f.rowCount("name='S_49' AND pending_delete_at IS NOT NULL") != 1 || f.rowCount("name='S_49' AND pending_delete_at IS NULL") != 1 {
			t.Fatal("expected one pending and one live S_49")
		}
	})

	t.Run("bytes", func(t *testing.T) {
		f := newEnvFixture(t)
		f.withReceipts()
		for i := range 3 {
			f.put(fmt.Sprintf("BIG_%d", i), jsonBody(map[string]any{"value": strings.Repeat("a", 65536)}), http.StatusCreated)
		}
		f.put("BIG_3", jsonBody(map[string]any{"value": strings.Repeat("a", 65536-8)}), http.StatusCreated) // 262136 bytes
		f.must(f.do(http.MethodDelete, "/BIG_3", ""), http.StatusAccepted)                                  // the pending row keeps them
		rec := f.do(http.MethodPut, "/BIG_3", jsonBody(map[string]any{"value": strings.Repeat("b", 9)}))
		f.must(rec, http.StatusConflict) // 262136 + 9 > 262144
		if envConflictCode(rec) != "env_limit_reached" {
			t.Fatalf("re-create over the byte limit: %s", rec.Body.String())
		}
		f.put("BIG_3", jsonBody(map[string]any{"value": strings.Repeat("b", 8)}), http.StatusCreated) // exactly 262144
	})
}

// A description-only save that commits between the planning read and the write
// transaction does not bump rev; the value save that follows keeps its description.
func TestRunEnvValueSaveKeepsConcurrentDescriptionLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("API_BASE", jsonBody(map[string]any{"value": "first-value", "secret": false, "description": "old"}), http.StatusCreated)
	f.h.secretBeginTx = func(ctx context.Context) (pgx.Tx, error) {
		f.h.secretBeginTx = nil // the nested save below takes the ordinary transaction
		f.put("API_BASE", jsonBody(map[string]any{"description": "new"}), http.StatusOK)
		return f.h.pool.BeginTx(ctx, pgx.TxOptions{})
	}
	got := f.put("API_BASE", jsonBody(map[string]any{"value": "second-value"}), http.StatusOK)
	if got.Rev != 2 || got.Description != "new" {
		t.Fatalf("new version lost the concurrent description: rev=%d description=%q", got.Rev, got.Description)
	}
	if v := f.find(f.list(), "API_BASE"); v == nil || v.Description != "new" || v.Value == nil || *v.Value != "second-value" {
		t.Fatalf("list: %+v", v)
	}
}

// A description-only save whose row is deleted between the planning read and the write
// transaction is a conflict the caller can retry, not a 500.
func TestRunEnvDescriptionSaveOfDeletedRowIs409LiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("API_BASE", jsonBody(map[string]any{"value": "first-value", "secret": false, "description": "old"}), http.StatusCreated)
	f.h.secretBeginTx = func(ctx context.Context) (pgx.Tx, error) {
		f.h.secretBeginTx = nil
		cliMustExec(t, f.pool, `DELETE FROM run_env_vars WHERE repo_id=$1 AND name='API_BASE'`, f.repo)
		return f.h.pool.BeginTx(ctx, pgx.TxOptions{})
	}
	rec := f.do(http.MethodPut, "/API_BASE", jsonBody(map[string]any{"description": "new"}))
	f.must(rec, http.StatusConflict)
	if envConflictCode(rec) != "env_changed" {
		t.Fatalf("description save of a deleted row: %s", rec.Body.String())
	}
}

// A body with raw bytes that are not UTF-8 is refused: encoding/json would otherwise
// store U+FFFD in place of what the caller sent.
func TestRunEnvPutRefusesInvalidUTF8LiveDB(t *testing.T) {
	f := newEnvFixture(t)
	rec := f.do(http.MethodPut, "/UTF_TEST", "{\"value\":\"abcd\xffefgh12345\",\"secret\":false}")
	f.must(rec, http.StatusBadRequest)
	if f.rowCount("name='UTF_TEST'") != 0 {
		t.Fatal("an invalid-UTF-8 body wrote a row")
	}
	// Control: the same body with valid text is accepted.
	f.put("UTF_TEST", "{\"value\":\"abcdéefgh12345\",\"secret\":false}", http.StatusCreated)
}

// vault_locked comes from the vault, so a list of secrets only (which never opens a
// value) still tells a locked protected vault from an unlocked or unprotected one.
func TestRunEnvListReportsLockedVaultForSecretOnlyLiveDB(t *testing.T) {
	h, router, pool := cliVaultLiveDB(t)
	owner := cliVaultUser(t, pool, false)
	cliVaultSeed(t, h, owner)
	repo := cliSeedOwnedRepo(t, pool, owner)
	f := &envFixture{t: t, h: h, router: router, pool: pool, owner: owner, repo: repo, jwt: cliMintJWT(t, pool, owner)}
	if err := h.vault.Unlock(context.Background(), owner, cliVaultPassword); err != nil {
		t.Fatal(err)
	}
	f.put("ONLY_SECRET", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	if f.list().VaultLocked {
		t.Fatal("an unlocked protected vault reports locked")
	}
	h.vault.Lock(owner)
	if l := f.list(); !l.VaultLocked || len(l.Variables) != 1 || !l.Variables[0].Secret {
		t.Fatalf("a secret-only list of a locked vault = %+v; want vault_locked", l)
	}

	u := newEnvFixture(t)
	u.put("ONLY_SECRET", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	if u.list().VaultLocked {
		t.Fatal("an unprotected vault reports locked")
	}
}

// A repo deleted between the ownership check and the write transaction answers 404
// on every writer (create, rotate, disable), never a 500 or a hang.
func TestRunEnvWriteAgainstDeletedRepoIs404LiveDB(t *testing.T) {
	deleteRepoInHook := func(f *envFixture) {
		f.h.secretBeginTx = func(ctx context.Context) (pgx.Tx, error) {
			f.h.secretBeginTx = nil
			cliMustExec(t, f.pool, `DELETE FROM repos WHERE id=$1`, f.repo)
			return f.h.pool.BeginTx(ctx, pgx.TxOptions{})
		}
	}
	t.Run("create", func(t *testing.T) {
		f := newEnvFixture(t)
		deleteRepoInHook(f)
		f.must(f.do(http.MethodPut, "/NEW_ONE", jsonBody(map[string]any{"value": secretFixture(1)})), http.StatusNotFound)
	})
	t.Run("rotate", func(t *testing.T) {
		f := newEnvFixture(t)
		f.put("OLD_ONE", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
		deleteRepoInHook(f)
		f.must(f.do(http.MethodPut, "/OLD_ONE", jsonBody(map[string]any{"value": secretFixture(2)})), http.StatusNotFound)
	})
	t.Run("disable", func(t *testing.T) {
		f := newEnvFixture(t)
		f.put("OLD_ONE", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
		deleteRepoInHook(f)
		f.must(f.do(http.MethodPost, "/OLD_ONE/disable", ""), http.StatusNotFound)
	})
}

// Deleting a (disabled) repo removes its variables, every version, by FK cascade.
func TestRunEnvRepoDeleteRemovesVariablesLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.withReceipts()
	f.put("KEEP_ME", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("KEEP_ME", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusOK) // retires v1
	f.put("PLAIN_ONE", jsonBody(map[string]any{"value": "x", "secret": false}), http.StatusCreated)
	if f.rowCount("true") != 3 {
		t.Fatalf("rows = %d, want 3", f.rowCount("true"))
	}
	cliMustExec(t, f.pool, `UPDATE repos SET enabled=false WHERE id=$1`, f.repo)
	rec := cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+f.repo.String(), f.jwt, "")
	f.must(rec, http.StatusNoContent)
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_env_vars WHERE repo_id=$1`, f.repo).Scan(&n); err != nil || n != 0 {
		t.Fatalf("env rows after repo delete = %d, %v", n, err)
	}
}

// The table refuses the nil-AAD legacy marker, so no env row can be opened without
// the name/repo binding.
func TestRunEnvTableRefusesMasterSealedLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("A_PLAIN", jsonBody(map[string]any{"value": "plain-value", "secret": false}), http.StatusCreated)
	if _, err := f.pool.Exec(context.Background(), `UPDATE run_env_vars SET sealed_with='master' WHERE repo_id=$1`, f.repo); err == nil {
		t.Fatal("sealed_with='master' was accepted on a run_env_vars row")
	}
}

// Rotating a value without a receipt replaces its ciphertext in place; a larger value
// that would push the owner's vault material past the conversion budget is refused.
func TestRunEnvRotationVaultMaterialHeadroomLiveDB(t *testing.T) {
	const maxMaterialBytes = 64 * 1024 * 1024 // vault.maxMaterialBytes
	const rowBytes = 1024 * 1024
	f := newEnvFixture(t)
	f.put("ROTATE_ME", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	other := envSecondRepo(t, f.pool, f.repo)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM run_env_vars WHERE repo_id=$1`, other) })

	totals, err := store.New(f.pool).GetVaultMaterialTotals(context.Background(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	const slack = 30000
	last := maxMaterialBytes - slack - totals.CiphertextBytes - 63*rowBytes
	fill := func(name string, count, size int64) {
		t.Helper()
		cliMustExec(t, f.pool, `INSERT INTO run_env_vars (var_id, user_id, repo_id, name, secret, ciphertext, sealed_with, value_bytes, rev)
			SELECT gen_random_uuid(), $1, $2, $3 || i, false, decode(repeat('00', $5::int), 'hex'), 'master_aad', 1, 1
			FROM generate_series(1, $4::int) AS i`, f.owner, other, name, count, size)
	}
	fill("FILL_A_", 63, rowBytes)
	fill("FILL_B_", 1, last)

	rec := f.do(http.MethodPut, "/ROTATE_ME", jsonBody(map[string]any{"value": strings.Repeat("r", 50000)}))
	f.must(rec, http.StatusConflict)
	if envConflictCode(rec) != "env_history_full" {
		t.Fatalf("rotation past the vault material budget: %s", rec.Body.String())
	}
	// The refusal speaks about the variable save, not about enabling or disabling the vault.
	for _, vaultWording := range []string{"vault conversion", "Your mode and credentials"} {
		if strings.Contains(rec.Body.String(), vaultWording) {
			t.Fatalf("the 409 carries the vault-conversion text %q: %s", vaultWording, rec.Body.String())
		}
	}
	if v := f.find(f.list(), "ROTATE_ME"); v == nil || v.Rev != 1 {
		t.Fatalf("a refused rotation moved the variable: %+v", v)
	}
	f.put("ROTATE_ME", jsonBody(map[string]any{"value": strings.Repeat("r", 10000)}), http.StatusOK) // growth inside the slack
}

// An escaped unpaired UTF-16 surrogate would decode to U+FFFD and store a value other
// than the one sent, so it is refused; a valid pair and an escaped backslash followed by
// literal text are stored byte-exact.
func TestRunEnvPutSurrogateEscapesLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	for i, v := range []string{`\ud800`, `\udc00`, `\udc00\ud800`, `abc\uD83D`, `\uDBFF`, `\udfff`} {
		rec := f.do(http.MethodPut, "/SURR_BAD", `{"value":"`+v+`","secret":false}`)
		f.must(rec, http.StatusBadRequest)
		if f.rowCount("name='SURR_BAD'") != 0 {
			t.Fatalf("case %d (%s) wrote a row", i, v)
		}
	}
	if rec := f.do(http.MethodPut, "/SURR_BAD", `{"value":"ok-value","description":"\ud800"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("lone surrogate in description: status %d", rec.Code)
	}
	for _, c := range []struct{ name, body, want string }{
		{"SURR_PAIR", `{"value":"x\ud83d\ude00y","secret":false}`, "x\U0001F600y"},
		{"SURR_ESC", `{"value":"\\ud800","secret":false}`, `\ud800`},
	} {
		f.put(c.name, c.body, http.StatusCreated)
		got := f.find(f.list(), c.name)
		if got == nil || got.Value == nil || *got.Value != c.want {
			t.Fatalf("%s stored %+v, want %q", c.name, got, c.want)
		}
	}
}
