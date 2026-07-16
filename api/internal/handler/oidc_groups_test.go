package handler

import "testing"

// TestGroupsIntersect locks the PRD #55 Decision 2 matcher semantics: exact,
// case-sensitive membership, no glob/regex/path-normalization, and either side empty
// is never a match. The exhaustive callback-flow matrix (gate reject, admin
// grant/demote, seed exemption, JIT gating) lands in M3.
func TestGroupsIntersect(t *testing.T) {
	cases := []struct {
		name       string
		configured []string
		claimed    []string
		want       bool
	}{
		{"both empty", nil, nil, false},
		{"configured empty", nil, []string{"uzi-admins"}, false},
		{"claimed empty", []string{"uzi-admins"}, nil, false},
		{"exact single match", []string{"uzi-admins"}, []string{"uzi-admins"}, true},
		{"one of many matches", []string{"ops", "uzi-admins"}, []string{"staff", "uzi-admins"}, true},
		{"no intersection", []string{"uzi-admins"}, []string{"staff", "ops"}, false},
		{"case-sensitive miss", []string{"uzi-admins"}, []string{"Uzi-Admins"}, false},
		{"substring is not a match", []string{"admin"}, []string{"uzi-admins"}, false},
		{"path-form miss (no normalization)", []string{"uzi-admins"}, []string{"/uzi-admins"}, false},
		{"path-form exact config matches", []string{"/uzi-admins"}, []string{"/uzi-admins"}, true},
		// A claim like ["a", null] unmarshals to ["a", ""]. The empty-string element
		// must never match — config names are trimmed+empty-dropped at load, so a
		// configured "" cannot exist. ["a",""] grants/gates only via the real "a".
		{"empty-string element no phantom match", []string{"staff"}, []string{"a", ""}, false},
		{"empty-string element real element still matches", []string{"a"}, []string{"a", ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupsIntersect(tc.configured, tc.claimed); got != tc.want {
				t.Errorf("groupsIntersect(%v, %v) = %v, want %v", tc.configured, tc.claimed, got, tc.want)
			}
		})
	}
}
