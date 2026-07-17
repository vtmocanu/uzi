package forge

import "testing"

// TestRoleAtLeast pins the neutral role ordering. AtLeast is the only way to
// compare roles, deliberately: it is what lets privcheck distinguish "above the
// write role" from "below" it without a number re-entering a type that exists to
// remove GitLab's access levels from the seam.
func TestRoleAtLeast(t *testing.T) {
	for _, tc := range []struct {
		role Role
		min  Role
		want bool
	}{
		{RoleNone, RoleWrite, false},
		{RoleRead, RoleWrite, false},
		{RoleWrite, RoleWrite, true},
		{RoleAdmin, RoleWrite, true},
		{RoleOwner, RoleWrite, true},
		{RoleOwner, RoleAdmin, true},
		{RoleAdmin, RoleOwner, false},
		{RoleRead, RoleNone, true},
		// A role a forge invented, or a report row that predates this type, must
		// never read as privileged — it ranks as none rather than sorting
		// arbitrarily.
		{Role("wheel"), RoleWrite, false},
		{Role(""), RoleWrite, false},
		{Role("wheel"), RoleNone, true},
	} {
		if got := tc.role.AtLeast(tc.min); got != tc.want {
			t.Errorf("Role(%q).AtLeast(%q) = %v, want %v", tc.role, tc.min, got, tc.want)
		}
	}
}
