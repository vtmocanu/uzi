package main

import "testing"

// TestRoleParity exercises the pure function against FIXTURE rosters and a
// FIXTURE accepted-list — never the live rosters, which would re-create the
// coupling issue #63 removes. It proves: a dev-team-only role is reported, a
// product-only role is reported, both accepted entries are suppressed, and lead
// is never emitted.
func TestRoleParity(t *testing.T) {
	product := []string{"architect", "coder", "lead", "judge"}
	devteam := []string{"architect", "coder", "release", "tui-ux"}
	acc := accepted{
		productOnly: map[string]bool{"lead": true},
		devteamOnly: map[string]bool{"release": true},
	}

	got := roleParity(product, devteam, acc)

	// Exactly the two un-accepted divergences, one per side, sorted
	// (devteam-only before product-only).
	want := []divergence{
		{side: sideDevteamOnly, role: "tui-ux"},
		{side: sideProductOnly, role: "judge"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d divergences, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].side != want[i].side || got[i].role != want[i].role {
			t.Errorf("divergence %d = {%s %s}, want {%s %s}", i, got[i].side, got[i].role, want[i].side, want[i].role)
		}
		if got[i].msg == "" {
			t.Errorf("divergence %d (%s %s) has an empty actionable message", i, got[i].side, got[i].role)
		}
	}

	for _, d := range got {
		if d.role == "lead" {
			t.Errorf("accepted product-only role %q was emitted: %+v", "lead", d)
		}
		if d.role == "release" {
			t.Errorf("accepted dev-team-only role %q was emitted: %+v", "release", d)
		}
	}
}

// TestRoleParityAllAccepted proves the nudge stays silent when every divergence
// is on the allowlist.
func TestRoleParityAllAccepted(t *testing.T) {
	product := []string{"coder", "lead"}
	devteam := []string{"coder", "release"}
	acc := accepted{
		productOnly: map[string]bool{"lead": true},
		devteamOnly: map[string]bool{"release": true},
	}
	if got := roleParity(product, devteam, acc); len(got) != 0 {
		t.Errorf("expected no divergences when all are accepted, got %+v", got)
	}
}

// TestRoleParityIdenticalRosters proves identical rosters produce nothing, even
// with an empty accepted-list.
func TestRoleParityIdenticalRosters(t *testing.T) {
	roles := []string{"coder", "tester"}
	if got := roleParity(roles, roles, accepted{}); len(got) != 0 {
		t.Errorf("identical rosters should produce no divergences, got %+v", got)
	}
}
