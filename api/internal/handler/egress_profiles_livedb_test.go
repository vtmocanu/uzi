package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/egressprofile"
)

// PRD #1906 M1: egress-profile admin routes against the REAL h.Routes() router, the only
// place the RequireUser + RequireAdminRO (reads) and RequireAuth + RequireAdmin (writes)
// chains are wired. A direct handler call would prove neither mount.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// egressName returns a fresh profile name (the LiveDB runner shares one database, and
// name is UNIQUE) and deletes that profile when the test ends.
func egressName(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	name := "t-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM egress_profiles WHERE name = $1`, name)
	})
	return name
}

func egressProfileCount(t *testing.T, pool *pgxpool.Pool, name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM egress_profiles WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("count egress profiles: %v", err)
	}
	return n
}

func decodeEgressProfile(t *testing.T, body []byte) apitypes.EgressProfileDTO {
	t.Helper()
	var env struct {
		EgressProfile apitypes.EgressProfileDTO `json:"egress_profile"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode egress_profile envelope: %v; body=%s", err, body)
	}
	return env.EgressProfile
}

// TestAdminEgressProfileReadRoutesCeilingLiveDB: the two reads are in the admin READ group.
// No credential is 401; a non-admin session and any uzc_ token (masked to IsAdmin=false,
// even an admin's) are 403; a uza_ admin_ro token and an admin session read.
func TestAdminEgressProfileReadRoutesCeilingLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)
	memberUzc := cliMintToken(t, pool, member, clitoken.ScopeUser)
	memberJWT := cliMintJWT(t, pool, member)
	adminJWT := cliMintJWT(t, pool, admin)

	name := egressName(t, pool)
	cliMustExec(t, pool, `INSERT INTO egress_profiles (name, hosts) VALUES ($1, '{docs.example.com}')`, name)

	for _, p := range []string{"/api/admin/egress-profiles", "/api/admin/egress-profiles/" + name} {
		if rec := bearerReq(router, http.MethodGet, p, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("no-credential GET %s = %d, want 401\nbody: %s", p, rec.Code, rec.Body.String())
		}
		if rec := bearerReq(router, http.MethodGet, p, adminUzc); rec.Code != http.StatusForbidden {
			t.Errorf("admin uzc_ GET %s = %d, want 403 (a user-scope token is masked to non-admin)\nbody: %s", p, rec.Code, rec.Body.String())
		}
		if rec := bearerReq(router, http.MethodGet, p, memberUzc); rec.Code != http.StatusForbidden {
			t.Errorf("member uzc_ GET %s = %d, want 403\nbody: %s", p, rec.Code, rec.Body.String())
		}
		if rec := cookieReq(t, router, http.MethodGet, p, memberJWT, ""); rec.Code != http.StatusForbidden {
			t.Errorf("member session GET %s = %d, want 403\nbody: %s", p, rec.Code, rec.Body.String())
		}
		if rec := bearerReq(router, http.MethodGet, p, adminUza); rec.Code != http.StatusOK {
			t.Errorf("admin uza_ GET %s = %d, want 200\nbody: %s", p, rec.Code, rec.Body.String())
		}
		if rec := cookieReq(t, router, http.MethodGet, p, adminJWT, ""); rec.Code != http.StatusOK {
			t.Errorf("admin session GET %s = %d, want 200\nbody: %s", p, rec.Code, rec.Body.String())
		}
	}
}

// TestAdminEgressProfileWriteRoutesCeilingLiveDB: create, update and delete are cookie-only.
// Every Bearer token (uza_ admin_ro included) is 401 before the handler, a non-admin session
// is 403, and none of those attempts changes the table.
func TestAdminEgressProfileWriteRoutesCeilingLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)
	memberJWT := cliMintJWT(t, pool, member)

	existing := egressName(t, pool)
	cliMustExec(t, pool, `INSERT INTO egress_profiles (name, description, hosts) VALUES ($1, 'orig', '{docs.example.com}')`, existing)
	fresh := egressName(t, pool)

	createBody := fmt.Sprintf(`{"name":%q,"hosts":["docs.example.com"]}`, fresh)
	updateBody := `{"description":"changed","hosts":["other.example.com"]}`
	cases := []struct{ verb, path, body string }{
		{http.MethodPost, "/api/admin/egress-profiles", createBody},
		{http.MethodPut, "/api/admin/egress-profiles/" + existing, updateBody},
		{http.MethodDelete, "/api/admin/egress-profiles/" + existing, ""},
	}
	for _, c := range cases {
		if rec := bearerReqBody(router, c.verb, c.path, "", c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("no-credential %s %s = %d, want 401\nbody: %s", c.verb, c.path, rec.Code, rec.Body.String())
		}
		if rec := bearerReqBody(router, c.verb, c.path, adminUza, c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("admin uza_ %s %s = %d, want 401 (cookie-only write group; a Bearer never reaches the write)\nbody: %s", c.verb, c.path, rec.Code, rec.Body.String())
		}
		if rec := bearerReqBody(router, c.verb, c.path, adminUzc, c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("admin uzc_ %s %s = %d, want 401\nbody: %s", c.verb, c.path, rec.Code, rec.Body.String())
		}
		if rec := cookieReq(t, router, c.verb, c.path, memberJWT, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("member session %s %s = %d, want 403\nbody: %s", c.verb, c.path, rec.Code, rec.Body.String())
		}
	}
	if n := egressProfileCount(t, pool, fresh); n != 0 {
		t.Errorf("a refused create stored %d rows for %s", n, fresh)
	}
	var desc string
	if err := pool.QueryRow(context.Background(), `SELECT description FROM egress_profiles WHERE name = $1`, existing).Scan(&desc); err != nil {
		t.Fatalf("a refused delete removed %s: %v", existing, err)
	}
	if desc != "orig" {
		t.Errorf("a refused update changed the description to %q", desc)
	}
}

// TestAdminEgressProfileCRUDLiveDB drives the admin session through create, read (over the
// uza_ token as the CLI does), duplicate, update, and delete, and checks what is stored.
func TestAdminEgressProfileCRUDLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	admin2 := cliSeedUser(t, pool, true)
	adminJWT := cliMintJWT(t, pool, admin)
	admin2JWT := cliMintJWT(t, pool, admin2)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	name := egressName(t, pool)
	base := "/api/admin/egress-profiles"

	// Create: entries normalized (case, IDNA, trailing dot) and de-duplicated in order.
	body := fmt.Sprintf(`{"name":%q,"description":"Vendor X official docs","hosts":["Docs.Vendor-X.com.","docs.vendor-x.com","*.CDN.vendor-x.com","bücher.de"]}`, name)
	rec := cookieReq(t, router, http.MethodPost, base, adminJWT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201\nbody: %s", rec.Code, rec.Body.String())
	}
	got := decodeEgressProfile(t, rec.Body.Bytes())
	if want := "docs.vendor-x.com,*.cdn.vendor-x.com,xn--bcher-kva.de"; strings.Join(got.Hosts, ",") != want {
		t.Fatalf("created hosts = %v, want %s", got.Hosts, want)
	}
	if got.Name != name || got.Description != "Vendor X official docs" || len(got.Warnings) != 0 || len(got.MultiPublisherOverride) != 0 {
		t.Fatalf("created profile = %+v", got)
	}
	if got.CreatedBy == nil || *got.CreatedBy != admin.String() || got.UpdatedBy == nil || *got.UpdatedBy != admin.String() {
		t.Fatalf("created_by/updated_by = %v/%v, want %s", got.CreatedBy, got.UpdatedBy, admin)
	}

	// Read back over the admin_ro Bearer, as `uzi admin egress-profile show` does.
	rec = bearerReq(router, http.MethodGet, base+"/"+name, adminUza)
	if rec.Code != http.StatusOK {
		t.Fatalf("show = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if shown := decodeEgressProfile(t, rec.Body.Bytes()); strings.Join(shown.Hosts, ",") != strings.Join(got.Hosts, ",") {
		t.Fatalf("show hosts = %v, want %v", shown.Hosts, got.Hosts)
	}
	rec = bearerReq(router, http.MethodGet, base, adminUza)
	var list struct {
		EgressProfiles []apitypes.EgressProfileDTO `json:"egress_profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%v)\nbody: %s", rec.Code, err, rec.Body.String())
	}
	found := false
	for _, p := range list.EgressProfiles {
		found = found || p.Name == name
	}
	if !found {
		t.Fatalf("list does not include %s", name)
	}

	// Duplicate name → 409; the stored row is untouched.
	if rec := cookieReq(t, router, http.MethodPost, base, adminJWT, fmt.Sprintf(`{"name":%q,"hosts":["x.example.com"]}`, name)); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409\nbody: %s", rec.Code, rec.Body.String())
	}

	// Update by a second admin: full replacement, updated_by moves, created_by stays.
	rec = cookieReq(t, router, http.MethodPut, base+"/"+name, admin2JWT, `{"description":"","hosts":["kb.vendor-x.com"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	got = decodeEgressProfile(t, rec.Body.Bytes())
	if strings.Join(got.Hosts, ",") != "kb.vendor-x.com" || got.Description != "" {
		t.Fatalf("updated profile = %+v", got)
	}
	if *got.CreatedBy != admin.String() || *got.UpdatedBy != admin2.String() {
		t.Fatalf("after update created_by/updated_by = %s/%s, want %s/%s", *got.CreatedBy, *got.UpdatedBy, admin, admin2)
	}
	if !got.UpdatedAt.After(got.CreatedAt) && !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Fatalf("updated_at %v precedes created_at %v", got.UpdatedAt, got.CreatedAt)
	}

	// The name is immutable: a PUT body carrying one is refused as an unknown field.
	if rec := cookieReq(t, router, http.MethodPut, base+"/"+name, adminJWT, `{"name":"renamed","hosts":["kb.vendor-x.com"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("update with a name in the body = %d, want 400\nbody: %s", rec.Code, rec.Body.String())
	}

	// Unknown and impossible names are 404 on every verb that addresses one.
	for _, missing := range []string{"no-such-profile-" + uuid.NewString()[:8], "Not%20A%20Slug"} {
		if rec := bearerReq(router, http.MethodGet, base+"/"+missing, adminUza); rec.Code != http.StatusNotFound {
			t.Errorf("show %s = %d, want 404", missing, rec.Code)
		}
		if rec := cookieReq(t, router, http.MethodPut, base+"/"+missing, adminJWT, `{"hosts":["a.example.com"]}`); rec.Code != http.StatusNotFound {
			t.Errorf("update %s = %d, want 404\nbody: %s", missing, rec.Code, rec.Body.String())
		}
		if rec := cookieReq(t, router, http.MethodDelete, base+"/"+missing, adminJWT, ""); rec.Code != http.StatusNotFound {
			t.Errorf("delete %s = %d, want 404", missing, rec.Code)
		}
	}

	// Delete → 204, then gone.
	if rec := cookieReq(t, router, http.MethodDelete, base+"/"+name, adminJWT, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := egressProfileCount(t, pool, name); n != 0 {
		t.Fatalf("after delete %d rows remain", n)
	}
	if rec := bearerReq(router, http.MethodGet, base+"/"+name, adminUza); rec.Code != http.StatusNotFound {
		t.Fatalf("show after delete = %d, want 404", rec.Code)
	}
}

// TestAdminEgressProfileValidationLiveDB drives every refusal class through the real write
// route: a 422 with reason invalid_egress_profile and the problem's stable code, and nothing
// stored. Then the multi-publisher override path: accepted with a warning that the read
// repeats.
func TestAdminEgressProfileValidationLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	adminJWT := cliMintJWT(t, pool, admin)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	const base = "/api/admin/egress-profiles"

	type invalidBody struct {
		Error    string                  `json:"error"`
		Reason   string                  `json:"reason"`
		Problems []egressprofile.Problem `json:"problems"`
	}
	hostCases := []struct{ entry, code string }{
		{"93.184.216.34", egressprofile.CodeIPAddress},
		{"[2001:db8::1]", egressprofile.CodeIPAddress},
		{"docs.example.com:8443", egressprofile.CodePort},
		{"https://docs.example.com", egressprofile.CodeScheme},
		{"docs.example.com/guide", egressprofile.CodePath},
		{"user@docs.example.com", egressprofile.CodeUserinfo},
		{"*", egressprofile.CodeBareWildcard},
		{"docs.*.example.com", egressprofile.CodeWildcardPosition},
		{"docs..example.com", egressprofile.CodeEmptyLabel},
		{"*.com", egressprofile.CodePublicSuffixWildcard},
		{"*.co.uk", egressprofile.CodePublicSuffixWildcard},
		{"*.github.io", egressprofile.CodePublicSuffixWildcard},
		{"*.cloudfront.net", egressprofile.CodePublicSuffixWildcard},
		{"*.amazonaws.com", egressprofile.CodeSharedParentWildcard},
		{"github.com", egressprofile.CodeMultiPublisher},
		{"s3.amazonaws.com", egressprofile.CodeMultiPublisher},
		{"*.reddit.com", egressprofile.CodeMultiPublisher},
	}
	for _, c := range hostCases {
		name := egressName(t, pool)
		rec := cookieReq(t, router, http.MethodPost, base, adminJWT,
			fmt.Sprintf(`{"name":%q,"hosts":["ok.example.com",%q]}`, name, c.entry))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("create with %q = %d, want 422\nbody: %s", c.entry, rec.Code, rec.Body.String())
			continue
		}
		var b invalidBody
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatalf("decode 422 body: %v; body=%s", err, rec.Body.String())
		}
		if b.Reason != "invalid_egress_profile" || b.Error == "" || len(b.Problems) != 1 ||
			b.Problems[0].Field != "hosts[1]" || b.Problems[0].Code != c.code || b.Problems[0].Message == "" {
			t.Errorf("create with %q: 422 body = %+v, want one %s problem on hosts[1]", c.entry, b, c.code)
		}
		if n := egressProfileCount(t, pool, name); n != 0 {
			t.Errorf("a refused create (%q) stored %d rows", c.entry, n)
		}
	}

	// Name and description rules, including termsafe (control and bidi characters).
	fieldCases := []struct{ body, field, code string }{
		{`{"name":"Has Space","hosts":["a.example.com"]}`, "name", egressprofile.CodeInvalidName},
		{`{"name":"` + strings.Repeat("a", 65) + `","hosts":["a.example.com"]}`, "name", egressprofile.CodeInvalidName},
		{`{"name":"esc\u001b[2J","hosts":["a.example.com"]}`, "name", egressprofile.CodeUnsafeText},
		{`{"name":"desc-bidi","description":"a\u202eb","hosts":["a.example.com"]}`, "description", egressprofile.CodeUnsafeText},
		{`{"name":"desc-long","description":"` + strings.Repeat("x", 501) + `","hosts":["a.example.com"]}`, "description", egressprofile.CodeDescriptionTooLong},
		{`{"name":"no-hosts","hosts":[]}`, "hosts", egressprofile.CodeNoEntries},
	}
	for _, c := range fieldCases {
		rec := cookieReq(t, router, http.MethodPost, base, adminJWT, c.body)
		var b invalidBody
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		if rec.Code != http.StatusUnprocessableEntity || len(b.Problems) != 1 || b.Problems[0].Field != c.field || b.Problems[0].Code != c.code {
			t.Errorf("create %s = %d %+v, want 422 with one %s problem on %s", c.body[:min(60, len(c.body))], rec.Code, b, c.code, c.field)
		}
	}
	for _, n := range []string{"desc-bidi", "desc-long", "no-hosts"} {
		if egressProfileCount(t, pool, n) != 0 {
			t.Errorf("a refused create stored %s", n)
		}
	}

	// A PUT is validated the same way and stores nothing on 422.
	name := egressName(t, pool)
	cliMustExec(t, pool, `INSERT INTO egress_profiles (name, hosts) VALUES ($1, '{docs.example.com}')`, name)
	if rec := cookieReq(t, router, http.MethodPut, base+"/"+name, adminJWT, `{"hosts":["*.github.io"]}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update with *.github.io = %d, want 422\nbody: %s", rec.Code, rec.Body.String())
	}
	var hosts []string
	if err := pool.QueryRow(context.Background(), `SELECT hosts FROM egress_profiles WHERE name = $1`, name).Scan(&hosts); err != nil || strings.Join(hosts, ",") != "docs.example.com" {
		t.Fatalf("after a refused update hosts = %v (%v), want unchanged", hosts, err)
	}

	// The multi-publisher override: accepted, stored, and warned on write and read.
	over := egressName(t, pool)
	rec := cookieReq(t, router, http.MethodPost, base, adminJWT,
		fmt.Sprintf(`{"name":%q,"hosts":["docs.vendor.com","GitHub.com"],"multi_publisher_override":["github.com"]}`, over))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with override = %d, want 201\nbody: %s", rec.Code, rec.Body.String())
	}
	got := decodeEgressProfile(t, rec.Body.Bytes())
	if strings.Join(got.MultiPublisherOverride, ",") != "github.com" || len(got.Warnings) != 1 ||
		got.Warnings[0].Entry != "github.com" || got.Warnings[0].Code != egressprofile.WarningCodeMultiPublisherOverride {
		t.Fatalf("created profile override/warnings = %v/%+v, want github.com with one warning", got.MultiPublisherOverride, got.Warnings)
	}
	rec = bearerReq(router, http.MethodGet, base+"/"+over, adminUza)
	if shown := decodeEgressProfile(t, rec.Body.Bytes()); len(shown.Warnings) != 1 || shown.Warnings[0].Entry != "github.com" {
		t.Fatalf("show warnings = %+v, want the override warning repeated on read", shown.Warnings)
	}
	// Re-PUT without the override: the entry is refused again (an override is per write).
	if rec := cookieReq(t, router, http.MethodPut, base+"/"+over, adminJWT, `{"hosts":["docs.vendor.com","github.com"]}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update dropping the override = %d, want 422\nbody: %s", rec.Code, rec.Body.String())
	}
}

// TestEgressProfileTableChecksLiveDB pins the schema floor under the Go validation: a writer
// that skips the api cannot store an empty host list, an override outside hosts, or a
// non-slug name.
func TestEgressProfileTableChecksLiveDB(t *testing.T) {
	_, _, pool := cliLiveDB(t)
	ctx := context.Background()
	name := egressName(t, pool)
	bad := []struct {
		label, sql string
		args       []any
	}{
		{"empty hosts", `INSERT INTO egress_profiles (name, hosts) VALUES ($1, '{}')`, []any{name}},
		{"override not in hosts", `INSERT INTO egress_profiles (name, hosts, multi_publisher_override) VALUES ($1, '{a.example.com}', '{github.com}')`, []any{name}},
		{"non-slug name", `INSERT INTO egress_profiles (name, hosts) VALUES ('Bad Name', '{a.example.com}')`, nil},
		{"long description", `INSERT INTO egress_profiles (name, description, hosts) VALUES ($1, repeat('x', 501), '{a.example.com}')`, []any{name}},
		{"null host", `INSERT INTO egress_profiles (name, hosts) VALUES ($1, ARRAY['a.example.com', NULL])`, []any{name}},
	}
	for _, b := range bad {
		if _, err := pool.Exec(ctx, b.sql, b.args...); err == nil {
			t.Errorf("%s: insert succeeded, want a CHECK violation", b.label)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO egress_profiles (name, hosts, multi_publisher_override) VALUES ($1, '{a.example.com,github.com}', '{github.com}')`, name); err != nil {
		t.Fatalf("a valid row was refused: %v", err)
	}
}
