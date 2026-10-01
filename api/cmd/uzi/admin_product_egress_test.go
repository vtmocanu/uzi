package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// `uzi admin products egress-profiles` (PRD #1976 M2): the read-only allowed-site-list view.

func productEgressFixture() *uzicli.FakeClient {
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	return &uzicli.FakeClient{
		AdminProducts: []apitypes.ProductDTO{{ID: "p1", Name: "Acme CRM", Enabled: true}, {ID: "p2", Name: "Other", Enabled: true}},
		ProductEgress: map[string][]uzicli.ProductEgressProfile{
			"p1": {{Name: "vendor-x", Description: "Vendor X docs", CreatedAt: at}, {Name: "plain", CreatedAt: at}},
			"p2": {},
		},
	}
}

func TestAdminProductsEgressProfilesTable(t *testing.T) {
	for _, ref := range []string{"Acme CRM", "p1", "acme crm"} {
		out, _, code := runCLI(t, fakeEnv(productEgressFixture()), "admin", "products", "egress-profiles", ref)
		if code != 0 {
			t.Fatalf("%s: exit = %d\n%s", ref, code, out)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("%s: %d lines, want header + 2 rows:\n%s", ref, len(lines), out)
		}
		for _, want := range []string{"NAME", "ALLOWED", "DESCRIPTION"} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("header %q lacks %s", lines[0], want)
			}
		}
		for _, want := range []string{"vendor-x", "2026-10-01T08:00:00Z", "Vendor X docs"} {
			if !strings.Contains(lines[1], want) {
				t.Errorf("row %q lacks %s", lines[1], want)
			}
		}
		if !strings.Contains(lines[2], "plain") {
			t.Errorf("row %q lacks plain", lines[2])
		}
	}
}

func TestAdminProductsEgressProfilesJSONAndEmpty(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(productEgressFixture()), "admin", "products", "egress-profiles", "p1", "--json")
	var got []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || len(got) != 2 || got[0]["name"] != "vendor-x" {
		t.Fatalf("exit = %d, json = %s", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(productEgressFixture()), "admin", "products", "egress-profiles", "p2", "--json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty exit = %d, json = %q, want []", code, out)
	}
}

func TestAdminProductsEgressProfilesUnknownProduct(t *testing.T) {
	_, errOut, code := runCLI(t, fakeEnv(productEgressFixture()), "admin", "products", "egress-profiles", "missing")
	if code != uzicli.ExitNotFound || !strings.Contains(errOut, "no product") {
		t.Errorf("exit = %d stderr = %q", code, errOut)
	}
}

func TestAdminProductsEgressProfilesHostileStringsEscaped(t *testing.T) {
	hostile := func(s string) string { return s + esc2J + oscTitle + "\u202e" + "\nFORGED_ROW  x" }
	fc := productEgressFixture()
	fc.ProductEgress["p1"] = []uzicli.ProductEgressProfile{{Name: hostile("n"), Description: hostile("d"), CreatedAt: time.Now()}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "products", "egress-profiles", "p1")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	assertNoTerminalControl(t, "admin products egress-profiles", out)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "FORGED_ROW") {
			t.Errorf("a newline forged a row: %q", line)
		}
	}
}
