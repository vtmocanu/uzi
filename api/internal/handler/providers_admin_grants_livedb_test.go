package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// PRD #4540 M2 m1: the admin grant API (named grants, the all-users switch, revoke-recipients and the
// paged grant reader), through the real router. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres.

func grantsPath(id string, parts ...string) string {
	return opPath(id, append([]string{"grants"}, parts...)...)
}

func (s *opFx) seedUser(t *testing.T) uuid.UUID {
	t.Helper()
	return cliSeedUser(t, s.f.pool, false)
}

func (s *opFx) grantCount(t *testing.T, id string) int {
	t.Helper()
	return countRows(t, s.sharedFx, `SELECT count(*) FROM model_provider_grants WHERE provider_id = $1`, id)
}

func (s *opFx) allUsers(t *testing.T, id string) bool {
	t.Helper()
	var on bool
	if err := s.f.pool.QueryRow(context.Background(), `SELECT operator_all_users FROM model_providers WHERE id = $1`, id).Scan(&on); err != nil {
		t.Fatal(err)
	}
	return on
}

// putGrant names user as a recipient and returns the response.
func (s *opFx) putGrant(t *testing.T, id string, user uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	return s.adminDo(t, http.MethodPut, grantsPath(id, user.String()), "")
}

// mustGrant names a user as a recipient and requires 204.
func (s *opFx) mustGrant(t *testing.T, id string, user uuid.UUID) {
	t.Helper()
	if rec := s.putGrant(t, id, user); rec.Code != http.StatusNoContent {
		t.Fatalf("put grant = %d %s", rec.Code, rec.Body.String())
	}
}

// readGrants reads one page through the admin-read token.
func (s *opFx) readGrants(t *testing.T, id, query string) (int, apitypes.AdminProviderGrantsDTO, string) {
	t.Helper()
	path := grantsPath(id)
	if query != "" {
		path += "?" + query
	}
	rec := bearerReqBody(s.f.router, http.MethodGet, path, s.uza, "")
	var page apitypes.AdminProviderGrantsDTO
	if rec.Code == http.StatusOK {
		if err := strictUnmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, page, rec.Body.String()
}

// seedGrantUsers inserts n user rows in one statement (the cap tests need thousands) and removes them,
// and so their grants, at cleanup.
func seedGrantUsers(t *testing.T, s *opFx, n int, tag string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.f.pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_admin, vault_mode)
		SELECT gen_random_uuid(), 'grantseed-' || $1::text || '-' || g || '@e2e', 'x', false, 'unprotected' FROM generate_series(1, $2::int) g`, tag, n); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.f.pool.Exec(ctx, `DELETE FROM users WHERE email LIKE 'grantseed-' || $1::text || '-%'`, tag); err != nil {
			t.Errorf("cleanup seeded users: %v", err)
		}
	})
}

// seedGrants grants every user seeded under tag to the definition directly in SQL.
func seedGrants(t *testing.T, s *opFx, id, tag string) {
	t.Helper()
	if _, err := s.f.pool.Exec(context.Background(), `INSERT INTO model_provider_grants (provider_id, user_id)
		SELECT $1::uuid, id FROM users WHERE email LIKE 'grantseed-' || $2::text || '-%'`, id, tag); err != nil {
		t.Fatalf("seed grants: %v", err)
	}
}

// TestProviderGrantRouteAuthLiveDB: the read is an admin-read route (a uza_ token may call it), and
// every write is a cookie + CSRF admin write: a missing or invalid CSRF, a uza_ or uzc_ Bearer and a
// non-admin session are refused before the handler, with no grant, all-users or audit change.
func TestProviderGrantRouteAuthLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	id := s.op.ID
	user := s.seedUser(t)
	s.mustGrant(t, id, s.seedUser(t))
	beforeAudit := strings.Count(logs.String(), "provider_audit")

	// Reads: uza_ succeeds; a non-admin session and a user token are refused.
	if code, page, body := s.readGrants(t, id, ""); code != http.StatusOK || page.NamedGrantCount != 1 {
		t.Fatalf("uza_ read = %d %s", code, body)
	}
	if rec := cookieReq(t, s.f.router, http.MethodGet, grantsPath(id), s.admin.jwt, ""); rec.Code != http.StatusOK {
		t.Errorf("admin cookie read = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, s.f.router, http.MethodGet, grantsPath(id), s.b.jwt, ""); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin read = %d, want 403", rec.Code)
	}
	if rec := bearerReqBody(s.f.router, http.MethodGet, grantsPath(id), s.f.uzc, ""); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("uzc_ read = %d, want 401/403", rec.Code)
	}
	if rec := bearerReqBody(s.f.router, http.MethodGet, grantsPath(id), "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous read = %d, want 401", rec.Code)
	}

	for _, c := range []struct{ name, method, path, body string }{
		{"put grant", http.MethodPut, grantsPath(id, user.String()), ""},
		{"delete grant", http.MethodDelete, grantsPath(id, user.String()), ""},
		{"all-users", http.MethodPut, opPath(id, "all-users"), `{"enabled":true}`},
		{"revoke", http.MethodPost, opPath(id, "revoke-recipients"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			bad := "AAAA.BBBB"
			for label, csrf := range map[string]*string{"missing CSRF": nil, "invalid CSRF": &bad} {
				rec := csrfCookieReq(t, s.f.router, c.method, c.path, s.admin.jwt, c.body, csrf)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CSRF validation failed") {
					t.Errorf("%s: %d %s, want 403 CSRF validation failed", label, rec.Code, rec.Body.String())
				}
			}
			for label, token := range map[string]string{"uza_": s.uza, "uzc_": s.f.uzc, "none": ""} {
				rec := bearerReqBody(s.f.router, c.method, c.path, token, c.body)
				if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
					t.Errorf("%s = %d, want 401/403", label, rec.Code)
				}
				if label == "uza_" && rec.Code != http.StatusUnauthorized {
					t.Errorf("uza_ = %d, want 401", rec.Code)
				}
			}
			if rec := cookieReq(t, s.f.router, c.method, c.path, s.b.jwt, c.body); rec.Code != http.StatusForbidden {
				t.Errorf("non-admin = %d, want 403", rec.Code)
			}
		})
	}
	if n := s.grantCount(t, id); n != 1 || s.allUsers(t, id) {
		t.Errorf("a refused write changed the recipients: %d grants, all-users %v", n, s.allUsers(t, id))
	}
	if n := strings.Count(logs.String(), "provider_audit"); n != beforeAudit {
		t.Errorf("refused callers wrote %d audit line(s)", n-beforeAudit)
	}
}

// TestProviderGrantDefinitionErrorsLiveDB: a per_user_key definition is a 409 provider_not_operator_key
// on every route; a personal or unknown definition is a 404; and none of them writes a row or an audit
// line.
func TestProviderGrantDefinitionErrorsLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	perUser := s.createShared(t, "peruser-"+uuid.NewString()[:8], "bearer")
	personal := s.f.create(t, s.personalBody("personal-"+uuid.NewString()[:6]))
	user := s.seedUser(t).String()
	calls := []struct{ name, method, suffix, body string }{
		{"put grant", http.MethodPut, "/grants/" + user, ""},
		{"delete grant", http.MethodDelete, "/grants/" + user, ""},
		{"all-users", http.MethodPut, "/all-users", `{"enabled":true}`},
		{"revoke", http.MethodPost, "/revoke-recipients", ""},
	}
	for _, c := range calls {
		if rec := s.adminDo(t, c.method, opPath(perUser.ID)+c.suffix, c.body); rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_not_operator_key" {
			t.Errorf("%s on a per-user-key definition = %d %s, want 409 provider_not_operator_key", c.name, rec.Code, rec.Body.String())
		}
		for label, other := range map[string]string{"personal": personal.ID, "unknown": uuid.NewString()} {
			if rec := s.adminDo(t, c.method, opPath(other)+c.suffix, c.body); rec.Code != http.StatusNotFound {
				t.Errorf("%s on a %s id = %d %s, want 404", c.name, label, rec.Code, rec.Body.String())
			}
		}
	}
	if code, _, body := s.readGrants(t, perUser.ID, ""); code != http.StatusConflict || !strings.Contains(body, "provider_not_operator_key") {
		t.Errorf("read on a per-user-key definition = %d %s, want 409 provider_not_operator_key", code, body)
	}
	for label, other := range map[string]string{"personal": personal.ID, "unknown": uuid.NewString()} {
		if code, _, body := s.readGrants(t, other, ""); code != http.StatusNotFound {
			t.Errorf("read on a %s id = %d %s, want 404", label, code, body)
		}
	}
	if n := countRows(t, s.sharedFx, `SELECT count(*) FROM model_provider_grants`+` WHERE provider_id = ANY($1)`, []string{perUser.ID, personal.ID}); n != 0 {
		t.Errorf("a refused write left %d grant row(s)", n)
	}
	if n := len(auditEvents(logs, "admin_grant_add")) + len(auditEvents(logs, "admin_all_users")) + len(auditEvents(logs, "admin_revoke_recipients")); n != 0 {
		t.Errorf("a refused write wrote %d audit line(s)", n)
	}
}

// TestProviderGrantWritesLiveDB: PUT and DELETE of a grant, the unknown-user 404, idempotence, the
// deleted-user DELETE, the cascade, the retired definition, and the audit lines (ids only).
func TestProviderGrantWritesLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	id := s.op.ID
	user := s.seedUser(t)

	// An unknown user is a 404 user_not_found and writes nothing; a malformed id is a 400.
	rec := s.putGrant(t, id, uuid.New())
	if rec.Code != http.StatusNotFound || providerReason(t, rec) != "user_not_found" {
		t.Errorf("put grant for an unknown user = %d %s, want 404 user_not_found", rec.Code, rec.Body.String())
	}
	if rec := s.adminDo(t, http.MethodPut, grantsPath(id, "not-a-uuid"), ""); rec.Code != http.StatusBadRequest {
		t.Errorf("put grant with a malformed user id = %d, want 400", rec.Code)
	}
	if rec := s.adminDo(t, http.MethodPut, opPath(id, "grants", "x", "y"), ""); rec.Code == http.StatusNoContent {
		t.Errorf("a malformed grant path succeeded: %d", rec.Code)
	}
	if n := s.grantCount(t, id); n != 0 || len(auditEvents(logs, "admin_grant_add")) != 0 {
		t.Fatalf("a refused put left %d grant(s) and %d audit line(s)", n, len(auditEvents(logs, "admin_grant_add")))
	}

	// PUT adds and audits once; the repeat is a 204 with no further line.
	s.mustGrant(t, id, user)
	s.mustGrant(t, id, user)
	if n := s.grantCount(t, id); n != 1 {
		t.Fatalf("grants after a repeated put = %d, want 1", n)
	}
	ev := auditEvents(logs, "admin_grant_add")
	if len(ev) != 1 || auditField(ev[0], "user_id") != user.String() || auditField(ev[0], "provider_id") != id {
		t.Fatalf("admin_grant_add audit = %v, want one line naming the user and the provider", ev)
	}
	if got := s.adminGetID(t, id); got.NamedGrantCount != 1 || got.OperatorAllUsers {
		t.Errorf("admin get = %+v, want named_grant_count 1 and all-users off", got)
	}

	// DELETE removes and audits once; a repeat, an absent user and a malformed path write no line.
	if rec := s.adminDo(t, http.MethodDelete, grantsPath(id, user.String()), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete grant = %d %s", rec.Code, rec.Body.String())
	}
	for _, u := range []string{user.String(), uuid.NewString()} {
		if rec := s.adminDo(t, http.MethodDelete, grantsPath(id, u), ""); rec.Code != http.StatusNoContent {
			t.Errorf("delete of an absent grant %s = %d, want 204", u, rec.Code)
		}
	}
	if rec := s.adminDo(t, http.MethodDelete, grantsPath(id, "not-a-uuid"), ""); rec.Code != http.StatusBadRequest {
		t.Errorf("delete with a malformed user id = %d, want 400", rec.Code)
	}
	if ev := auditEvents(logs, "admin_grant_remove"); len(ev) != 1 || auditField(ev[0], "user_id") != user.String() {
		t.Errorf("admin_grant_remove audit = %v, want exactly one line", ev)
	}
	if n := s.grantCount(t, id); n != 0 {
		t.Fatalf("grants after delete = %d, want 0", n)
	}

	// A deleted user's grant goes with the user (the foreign key cascade), and DELETE for that user is 204.
	gone := s.seedUser(t)
	s.mustGrant(t, id, gone)
	if _, err := s.f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, gone); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := s.grantCount(t, id); n != 0 {
		t.Errorf("grants after the user was deleted = %d, want 0 (cascade)", n)
	}
	removes := len(auditEvents(logs, "admin_grant_remove"))
	if rec := s.adminDo(t, http.MethodDelete, grantsPath(id, gone.String()), ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete grant of a deleted user = %d, want 204", rec.Code)
	}
	if rec := s.putGrant(t, id, gone); rec.Code != http.StatusNotFound || providerReason(t, rec) != "user_not_found" {
		t.Errorf("put grant for a deleted user = %d %s, want 404 user_not_found", rec.Code, rec.Body.String())
	}
	if got := len(auditEvents(logs, "admin_grant_remove")); got != removes {
		t.Errorf("deleting a grant of a deleted user wrote %d audit line(s)", got-removes)
	}

	// Writes are allowed on a retired definition, so removal stays available after retire.
	other := s.seedUser(t)
	s.mustGrant(t, id, other)
	if rec := s.adminDo(t, http.MethodPost, opPath(id, "retire"), ""); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d", rec.Code)
	}
	again := s.seedUser(t)
	s.mustGrant(t, id, again)
	if rec := s.adminDo(t, http.MethodDelete, grantsPath(id, other.String()), ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete grant on a retired definition = %d, want 204", rec.Code)
	}
	if rec := s.adminDo(t, http.MethodPut, opPath(id, "all-users"), `{"enabled":true}`); rec.Code != http.StatusNoContent {
		t.Errorf("all-users on a retired definition = %d, want 204", rec.Code)
	}
	if rec := s.adminDo(t, http.MethodPost, opPath(id, "revoke-recipients"), ""); rec.Code != http.StatusOK {
		t.Errorf("revoke on a retired definition = %d, want 200", rec.Code)
	}

	// Audit lines carry ids only.
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "provider_audit") && (strings.Contains(l, "@e2e") || strings.Contains(l, "email")) {
			t.Errorf("an audit line carries an email: %s", l)
		}
	}
}

// TestProviderGrantReaderLiveDB: the keyset page, the limit and cursor validation, the response shape
// and the all-users flag.
func TestProviderGrantReaderLiveDB(t *testing.T) {
	s := newOpFx(t)
	id := s.op.ID

	code, page, body := s.readGrants(t, id, "")
	if code != http.StatusOK || page.OperatorAllUsers || page.NamedGrantCount != 0 || page.Grants == nil || len(page.Grants) != 0 || page.NextCursor != nil {
		t.Fatalf("empty read = %d %s", code, body)
	}
	if !strings.Contains(body, `"next_cursor":null`) || !strings.Contains(body, `"grants":[]`) {
		t.Errorf("empty read body = %s, want an empty grants array and a null next_cursor", body)
	}

	var users []string
	for range 5 {
		u := s.seedUser(t)
		s.mustGrant(t, id, u)
		users = append(users, u.String())
	}
	sorted := slices.Sorted(slices.Values(users))

	// Walk the pages of two: the ids come back in user_id order, once each, and the last page has a null cursor.
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("the cursor never ended")
		}
		q := "limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		code, page, body := s.readGrants(t, id, q)
		if code != http.StatusOK || page.NamedGrantCount != 5 {
			t.Fatalf("page %d = %d %s", pages, code, body)
		}
		for _, g := range page.Grants {
			got = append(got, g.UserID)
		}
		if page.NextCursor == nil {
			if len(page.Grants) == 0 || len(page.Grants) > 2 {
				t.Errorf("last page has %d items", len(page.Grants))
			}
			break
		}
		if len(page.Grants) != 2 || *page.NextCursor != page.Grants[1].UserID {
			t.Fatalf("page %d = %s, want 2 items and next_cursor = the last item's user id", pages, body)
		}
		cursor = *page.NextCursor
	}
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Errorf("paged ids = %v, want %v", got, sorted)
	}
	// A page that exactly fills the limit with nothing after it has no cursor.
	if _, page, _ := s.readGrants(t, id, "limit=5"); len(page.Grants) != 5 || page.NextCursor != nil {
		t.Errorf("limit=5 page = %d items, cursor %v, want 5 items and no cursor", len(page.Grants), page.NextCursor)
	}
	// The default limit is 100 and the largest accepted is 500.
	for _, q := range []string{"", "limit=500", "limit=1"} {
		if code, _, body := s.readGrants(t, id, q); code != http.StatusOK {
			t.Errorf("read %q = %d %s, want 200", q, code, body)
		}
	}
	for _, q := range []string{"limit=0", "limit=-1", "limit=501", "limit=abc", "cursor=not-a-uuid", "limit=1&cursor=%00"} {
		if code, _, body := s.readGrants(t, id, q); code != http.StatusBadRequest {
			t.Errorf("read %q = %d %s, want 400", q, code, body)
		}
	}

	// The all-users flag is in the same read.
	if rec := s.adminDo(t, http.MethodPut, opPath(id, "all-users"), `{"enabled":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("all-users = %d", rec.Code)
	}
	if _, page, _ := s.readGrants(t, id, ""); !page.OperatorAllUsers || page.NamedGrantCount != 5 || len(page.Grants) != 5 {
		t.Errorf("read with all-users on = %+v", page)
	}
	// The admin catalogue carries the same two values and no user id.
	rec := bearerReqBody(s.f.router, http.MethodGet, adminProvidersPath, s.uza, "")
	var list apitypes.AdminSharedProviderListDTO
	if rec.Code != http.StatusOK || strictUnmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, p := range list.Providers {
		if p.ID == id {
			found = true
			if !p.OperatorAllUsers || p.NamedGrantCount != 5 {
				t.Errorf("listed definition = all-users %v, count %d, want true and 5", p.OperatorAllUsers, p.NamedGrantCount)
			}
		}
	}
	if !found {
		t.Error("the definition is not in the admin list")
	}
	for _, u := range users {
		if strings.Contains(rec.Body.String(), u) {
			t.Errorf("the admin list carries a grantee's user id %s", u)
		}
	}
}

// TestProviderAllUsersAndRevokeLiveDB: the switch is idempotent with an audit line every time and a
// `changed` flag; revoke turns it off and deletes every grant and answers the count.
func TestProviderAllUsersAndRevokeLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	id := s.op.ID
	set := func(body string) *httptest.ResponseRecorder {
		return s.adminDo(t, http.MethodPut, opPath(id, "all-users"), body)
	}

	for _, body := range []string{`{}`, `{"enabled":"yes"}`, `{"enabled":true,"extra":1}`, `not json`, ``} {
		if rec := set(body); rec.Code != http.StatusBadRequest {
			t.Errorf("all-users %q = %d, want 400", body, rec.Code)
		}
	}
	if n := len(auditEvents(logs, "admin_all_users")); n != 0 || s.allUsers(t, id) {
		t.Fatalf("a refused all-users body wrote %d audit line(s) or changed the switch", n)
	}

	for i, step := range []struct {
		body    string
		enabled bool
		changed string
	}{{`{"enabled":true}`, true, "true"}, {`{"enabled":true}`, true, "false"}, {`{"enabled":false}`, false, "true"}, {`{"enabled":false}`, false, "false"}} {
		if rec := set(step.body); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("step %d: all-users %s = %d %q, want an empty 204", i, step.body, rec.Code, rec.Body.String())
		}
		if s.allUsers(t, id) != step.enabled {
			t.Errorf("step %d: the switch is %v, want %v", i, !step.enabled, step.enabled)
		}
		ev := auditEvents(logs, "admin_all_users")
		if len(ev) != i+1 {
			t.Fatalf("step %d: %d admin_all_users line(s), want %d (one per call, repeats included)", i, len(ev), i+1)
		}
		last := ev[len(ev)-1]
		if auditField(last, "enabled") != map[bool]string{true: "true", false: "false"}[step.enabled] || auditField(last, "changed") != step.changed || auditField(last, "provider_id") != id {
			t.Errorf("step %d: audit line = %s, want enabled=%v changed=%s", i, last, step.enabled, step.changed)
		}
	}

	// Revoke: all-users off and every grant gone, in one answer; one aggregate audit line.
	for range 3 {
		s.mustGrant(t, id, s.seedUser(t))
	}
	if rec := set(`{"enabled":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("all-users on = %d", rec.Code)
	}
	revoke := func() *httptest.ResponseRecorder {
		return s.adminDo(t, http.MethodPost, opPath(id, "revoke-recipients"), "")
	}
	rec := revoke()
	var out apitypes.AdminRevokeRecipientsDTO
	if rec.Code != http.StatusOK || strictUnmarshal(rec.Body.Bytes(), &out) != nil || out.NamedGrantsRemoved != 3 {
		t.Fatalf("revoke = %d %s, want 200 named_grants_removed 3", rec.Code, rec.Body.String())
	}
	if s.allUsers(t, id) || s.grantCount(t, id) != 0 {
		t.Errorf("after revoke: all-users %v, %d grant(s)", s.allUsers(t, id), s.grantCount(t, id))
	}
	ev := auditEvents(logs, "admin_revoke_recipients")
	if len(ev) != 1 || auditField(ev[0], "named_grants_removed") != "3" || auditField(ev[0], "previous_all_users") != "true" {
		t.Fatalf("admin_revoke_recipients audit = %v, want one line with named_grants_removed=3 previous_all_users=true", ev)
	}
	if n := len(auditEvents(logs, "admin_grant_remove")); n != 0 {
		t.Errorf("revoke wrote %d per-grant line(s), want only the aggregate one", n)
	}
	// A repeat removes nothing and says so.
	rec = revoke()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"named_grants_removed":0`) {
		t.Errorf("repeated revoke = %d %s", rec.Code, rec.Body.String())
	}
	if ev := auditEvents(logs, "admin_revoke_recipients"); len(ev) != 2 || auditField(ev[1], "previous_all_users") != "false" || auditField(ev[1], "named_grants_removed") != "0" {
		t.Errorf("repeated revoke audit = %v", ev)
	}
}

// failingTx wraps a real transaction and fails any statement that deletes grants, to prove the writes
// of one handler share one transaction.
type failingTx struct {
	pgx.Tx
	failOn string
}

func (f failingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, f.failOn) {
		return pgconn.CommandTag{}, errors.New("injected failure")
	}
	return f.Tx.Exec(ctx, sql, args...)
}

// TestProviderRevokeIsOneTransactionLiveDB: when the grant delete fails, the all-users switch that
// revoke turned off in the same call is rolled back with it, and no audit line is written.
func TestProviderRevokeIsOneTransactionLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	id := s.op.ID
	for range 2 {
		s.mustGrant(t, id, s.seedUser(t))
	}
	if rec := s.adminDo(t, http.MethodPut, opPath(id, "all-users"), `{"enabled":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("all-users = %d", rec.Code)
	}
	s.f.h.secretBeginTx = func(ctx context.Context) (pgx.Tx, error) {
		tx, err := s.f.pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		return failingTx{Tx: tx, failOn: "DELETE FROM model_provider_grants WHERE provider_id = $1"}, nil
	}
	defer func() { s.f.h.secretBeginTx = nil }()
	rec := s.adminDo(t, http.MethodPost, opPath(id, "revoke-recipients"), "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("revoke with a failing grant delete = %d %s, want 500", rec.Code, rec.Body.String())
	}
	if !s.allUsers(t, id) || s.grantCount(t, id) != 2 {
		t.Errorf("a failed revoke left all-users %v and %d grant(s), want true and 2 (one transaction)", s.allUsers(t, id), s.grantCount(t, id))
	}
	if n := len(auditEvents(logs, "admin_revoke_recipients")); n != 0 {
		t.Errorf("a failed revoke wrote %d audit line(s)", n)
	}
}

// TestProviderAllUsersCheckLiveDB: the CHECK refuses all-users on a per_user_key row and on a row with
// no credential mode (a personal provider).
func TestProviderAllUsersCheckLiveDB(t *testing.T) {
	s := newOpFx(t)
	perUser := s.createShared(t, "peruser-"+uuid.NewString()[:8], "bearer")
	personal := s.f.create(t, s.personalBody("personal-"+uuid.NewString()[:6]))
	ctx := context.Background()
	for label, id := range map[string]string{"per_user_key": perUser.ID, "NULL credential mode": personal.ID} {
		_, err := s.f.pool.Exec(ctx, `UPDATE model_providers SET operator_all_users = true WHERE id = $1`, id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "model_providers_operator_all_users_check" {
			t.Errorf("%s: all-users = %v, want check_violation on model_providers_operator_all_users_check", label, err)
		}
	}
	if _, err := s.f.pool.Exec(ctx, `UPDATE model_providers SET operator_all_users = true WHERE id = $1`, s.op.ID); err != nil {
		t.Errorf("all-users on an operator_key row: %v", err)
	}
}

// TestProviderGrantCapLiveDB: a new grant at 10,000 is a 409, an existing one is still a 204 with no
// audit line, a removal frees a slot, and two concurrent adds at cap-1 admit exactly one.
func TestProviderGrantCapLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	id := s.op.ID
	tag := uuid.NewString()[:8]
	seedGrantUsers(t, s, maxProviderGrants, tag)
	seedGrants(t, s, id, tag)
	if n := s.grantCount(t, id); n != maxProviderGrants {
		t.Fatalf("seeded grants = %d, want %d", n, maxProviderGrants)
	}
	var existing uuid.UUID
	if err := s.f.pool.QueryRow(context.Background(), `SELECT user_id FROM model_provider_grants WHERE provider_id = $1 LIMIT 1`, id).Scan(&existing); err != nil {
		t.Fatal(err)
	}

	// At the cap an existing grant is idempotent, silently.
	if rec := s.putGrant(t, id, existing); rec.Code != http.StatusNoContent {
		t.Errorf("put of an existing grant at the cap = %d %s, want 204", rec.Code, rec.Body.String())
	}
	if n := len(auditEvents(logs, "admin_grant_add")); n != 0 {
		t.Errorf("put of an existing grant at the cap wrote %d audit line(s)", n)
	}
	// A new one is refused with the typed reason and writes nothing.
	extra, extra2 := s.seedUser(t), s.seedUser(t)
	rec := s.putGrant(t, id, extra)
	if rec.Code != http.StatusConflict || providerReason(t, rec) != "provider_grant_cap_reached" {
		t.Fatalf("put of a new grant at the cap = %d %s, want 409 provider_grant_cap_reached", rec.Code, rec.Body.String())
	}
	if n := s.grantCount(t, id); n != maxProviderGrants || len(auditEvents(logs, "admin_grant_add")) != 0 {
		t.Errorf("a refused add left %d grant(s) and %d audit line(s)", n, len(auditEvents(logs, "admin_grant_add")))
	}
	// The unknown-user check precedes the cap check.
	if rec := s.putGrant(t, id, uuid.New()); rec.Code != http.StatusNotFound {
		t.Errorf("put for an unknown user at the cap = %d, want 404", rec.Code)
	}

	// Remove one: now exactly one slot is free, and two concurrent adds admit exactly one.
	if rec := s.adminDo(t, http.MethodDelete, grantsPath(id, existing.String()), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	reqs := []func() *httptest.ResponseRecorder{
		cookieReqPrepared(t, s.f.router, http.MethodPut, grantsPath(id, extra.String()), s.admin.jwt, ""),
		cookieReqPrepared(t, s.f.router, http.MethodPut, grantsPath(id, extra2.String()), s.admin.jwt, ""),
	}
	codes := make([]int, len(reqs))
	var wg sync.WaitGroup
	for i, run := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = run().Code
		}()
	}
	wg.Wait()
	ok, full := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusNoContent:
			ok++
		case http.StatusConflict:
			full++
		}
	}
	if ok != 1 || full != 1 {
		t.Errorf("concurrent adds at cap-1 = %v, want exactly one 204 and one 409", codes)
	}
	if n := s.grantCount(t, id); n != maxProviderGrants {
		t.Errorf("grants after the race = %d, want %d", n, maxProviderGrants)
	}
	if n := len(auditEvents(logs, "admin_grant_add")); n != 1 {
		t.Errorf("admin_grant_add lines after the race = %d, want 1", n)
	}
}
