package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// `uzi admin products` (PRD #1907 M4/M6): the read-only product registry list.

func adminProductsFixture() []apitypes.ProductDTO {
	created := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	deleted := created.Add(48 * time.Hour)
	return []apitypes.ProductDTO{
		{ID: "p1", Name: "Acme CRM", Description: "Syncs tickets", Enabled: true, CreatedAt: created, ActiveTokenCount: 3, LiveConnectionCount: 7,
			OAuthClient: apitypes.ProductOAuthClientDTO{
				RedirectURIs: []string{"https://acme.example.com/cb"}, Scopes: []string{"jobs:run", "jobs:read"},
				HasSecret: true, SecretPrefix: "uzs_Ab12", IsClient: true,
			}},
		{ID: "p2", Name: "Paused", Enabled: false, CreatedAt: created},
		{ID: "p3", Name: "Gone", Enabled: false, DeletedAt: &deleted, CreatedAt: created, ActiveTokenCount: 2, LiveConnectionCount: 4},
	}
}

func TestAdminProductsTable(t *testing.T) {
	fc := &uzicli.FakeClient{AdminProducts: adminProductsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "products")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("rendered %d lines, want header + 3 rows:\n%s", len(lines), out)
	}
	for _, h := range []string{"NAME", "STATE", "ACTIVE_TOKENS", "CONNECTIONS", "CLIENT", "SCOPES", "CREATED", "DESCRIPTION"} {
		if !strings.Contains(lines[0], h) {
			t.Errorf("header %q lacks %s", lines[0], h)
		}
	}
	// CONNECTIONS sits right after ACTIVE_TOKENS and each row's cell under it is that product's
	// live connection count.
	hdr := strings.Fields(lines[0])
	col := -1
	for i, h := range hdr {
		if h == "CONNECTIONS" {
			col = i
		}
	}
	if col < 0 || hdr[col-1] != "ACTIVE_TOKENS" {
		t.Fatalf("header %q: CONNECTIONS is not right after ACTIVE_TOKENS", lines[0])
	}
	// Rows 2 and 3 have one-word names, so their fields line up with the header's.
	for _, c := range []struct {
		row  int
		want string
	}{{2, "0"}, {3, "4"}} {
		if f := strings.Fields(lines[c.row]); f[col] != c.want {
			t.Errorf("row %d %q: CONNECTIONS cell = %q, want %q", c.row, lines[c.row], f[col], c.want)
		}
	}
	want := []struct {
		row    int
		fields []string
	}{
		{1, []string{"Acme CRM", "enabled", "3", "7", "yes", "jobs:run,jobs:read", "2026-09-01T10:00:00Z", "Syncs tickets"}},
		{2, []string{"Paused", "disabled", "0", "no"}},
		// deleted wins over the (always false) enabled flag.
		{3, []string{"Gone", "deleted", "2", "4"}},
	}
	for _, w := range want {
		for _, f := range w.fields {
			if !strings.Contains(lines[w.row], f) {
				t.Errorf("row %d %q lacks %q", w.row, lines[w.row], f)
			}
		}
	}
}

func TestAdminProductsJSON(t *testing.T) {
	fc := &uzicli.FakeClient{AdminProducts: adminProductsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "products", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var got []apitypes.ProductDTO
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(got) != 3 || got[2].DeletedAt == nil || got[0].ActiveTokenCount != 3 {
		t.Fatalf("json round-trip = %+v", got)
	}
}

// A server error surfaces as the client's exit code, like every admin verb (a masked
// uzc_ or non-admin gets 403 from the route group).
func TestAdminProductsServerError(t *testing.T) {
	fc := &uzicli.FakeClient{Err: &uzicli.ExitError{Code: uzicli.ExitAuth, Err: errors.New("admin access required")}}
	_, _, code := runCLI(t, fakeEnv(fc), "admin", "products")
	if code != uzicli.ExitAuth {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitAuth)
	}
}

// `--json` carries oauth_client (the DTO) and never a client secret or its hash: the DTO has
// no such field, and the table prints only yes/no and the scopes.
func TestAdminProductsJSONCarriesOAuthClientButNoSecret(t *testing.T) {
	fc := &uzicli.FakeClient{AdminProducts: adminProductsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "products", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	var oc map[string]json.RawMessage
	if err := json.Unmarshal(raw[0]["oauth_client"], &oc); err != nil {
		t.Fatalf("product 0 has no oauth_client object: %v", err)
	}
	for _, k := range []string{"redirect_uris", "scopes", "has_secret", "secret_prefix", "rotated_at", "is_client"} {
		if _, ok := oc[k]; !ok {
			t.Errorf("oauth_client lacks %q: %s", k, out)
		}
	}
	for _, banned := range []string{"client_secret", "secret_hash", "hash"} {
		if strings.Contains(out, banned) {
			t.Errorf("--json output contains %q: %s", banned, out)
		}
	}
	tbl, _, _ := runCLI(t, fakeEnv(fc), "admin", "products")
	if strings.Contains(tbl, "uzs_") {
		t.Errorf("the table printed a secret-class value: %s", tbl)
	}
}
