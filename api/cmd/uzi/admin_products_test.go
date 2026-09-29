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
		{ID: "p1", Name: "Acme CRM", Description: "Syncs tickets", Enabled: true, CreatedAt: created, ActiveTokenCount: 3},
		{ID: "p2", Name: "Paused", Enabled: false, CreatedAt: created},
		{ID: "p3", Name: "Gone", Enabled: false, DeletedAt: &deleted, CreatedAt: created, ActiveTokenCount: 2},
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
	for _, h := range []string{"NAME", "STATE", "ACTIVE_TOKENS", "CREATED", "DESCRIPTION"} {
		if !strings.Contains(lines[0], h) {
			t.Errorf("header %q lacks %s", lines[0], h)
		}
	}
	want := []struct {
		row    int
		fields []string
	}{
		{1, []string{"Acme CRM", "enabled", "3", "2026-09-01T10:00:00Z", "Syncs tickets"}},
		{2, []string{"Paused", "disabled", "0"}},
		// deleted wins over the (always false) enabled flag.
		{3, []string{"Gone", "deleted", "2"}},
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
