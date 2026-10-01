package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// `uzi admin products connections` (PRD #1910 M5): the read-only OAuth connections view.

func productConnectionsFixture() *uzicli.FakeClient {
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	used := at.Add(time.Hour)
	return &uzicli.FakeClient{
		AdminProducts: []apitypes.ProductDTO{{ID: "p1", Name: "Acme CRM", Enabled: true}, {ID: "p2", Name: "Other", Enabled: true}},
		ProductConns: map[string]uzicli.ProductConnections{
			"p1": {Connections: []apitypes.AdminOAuthConnectionDTO{
				{ID: "g1", UserID: "u1", OwnerEmail: "ann@example.test", Scopes: []string{"jobs:run", "jobs:read"}, ConnectedAt: at, CreatedAt: at, LastUsedAt: &used},
				{ID: "g2", UserID: "u2", OwnerEmail: "bob@example.test", Scopes: []string{"jobs:read"}, ConnectedAt: at, CreatedAt: at},
			}},
			"p2": {},
		},
	}
}

func TestAdminProductsConnectionsTable(t *testing.T) {
	for _, ref := range []string{"Acme CRM", "p1", "acme crm"} {
		out, _, code := runCLI(t, fakeEnv(productConnectionsFixture()), "admin", "products", "connections", ref)
		if code != 0 {
			t.Fatalf("%s: exit = %d\n%s", ref, code, out)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("%s: %d lines, want header + 2 rows:\n%s", ref, len(lines), out)
		}
		for _, want := range []string{"USER", "SCOPES", "CONNECTED", "LAST USED", "ID"} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("header %q lacks %s", lines[0], want)
			}
		}
		for _, want := range []string{"ann@example.test", "jobs:run,jobs:read", "2026-10-01T08:00:00Z", "2026-10-01T09:00:00Z", "g1"} {
			if !strings.Contains(lines[1], want) {
				t.Errorf("row %q lacks %s", lines[1], want)
			}
		}
		// A never-used connection prints a dash for last use.
		if !strings.Contains(lines[2], "bob@example.test") || !strings.Contains(lines[2], " - ") {
			t.Errorf("row %q lacks the never-used dash", lines[2])
		}
	}
}

func TestAdminProductsConnectionsJSONEmptyAndTruncated(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(productConnectionsFixture()), "admin", "products", "connections", "p1", "--json")
	var got struct {
		Connections []map[string]any `json:"connections"`
		Truncated   bool             `json:"truncated"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || len(got.Connections) != 2 || got.Connections[0]["owner_email"] != "ann@example.test" || got.Truncated {
		t.Fatalf("exit = %d, json = %s", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(productConnectionsFixture()), "admin", "products", "connections", "p2", "--json")
	if code != 0 || !strings.Contains(out, `"connections": []`) {
		t.Errorf("empty exit = %d, json = %q, want an empty connections array", code, out)
	}
	fc := productConnectionsFixture()
	fc.ProductConns["p2"] = uzicli.ProductConnections{Truncated: true}
	out, _, code = runCLI(t, fakeEnv(fc), "admin", "products", "connections", "p2")
	if code != 0 || !strings.Contains(out, "truncated") {
		t.Errorf("truncated table exit = %d, out = %q, want a truncation note", code, out)
	}
}

func TestAdminProductsConnectionsUnknownProduct(t *testing.T) {
	_, errOut, code := runCLI(t, fakeEnv(productConnectionsFixture()), "admin", "products", "connections", "missing")
	if code != uzicli.ExitNotFound || !strings.Contains(errOut, "no product") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
}

func TestAdminProductsConnectionsHostileStringsEscaped(t *testing.T) {
	hostile := func(s string) string { return s + esc2J + oscTitle + "\u202e" + "\nFORGED_ROW  x" }
	fc := productConnectionsFixture()
	fc.ProductConns["p1"] = uzicli.ProductConnections{Connections: []apitypes.AdminOAuthConnectionDTO{
		{ID: hostile("i"), OwnerEmail: hostile("e"), Scopes: []string{hostile("s")}, ConnectedAt: time.Now()},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "products", "connections", "p1")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	assertNoTerminalControl(t, "admin products connections", out)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "FORGED_ROW") {
			t.Errorf("a newline forged a row: %q", line)
		}
	}
}
