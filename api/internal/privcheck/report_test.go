package privcheck

import "testing"

// TestWaivable pins the single-code predicate: every "bot is too strong" BLOCK
// code is waivable, protection_unreadable never is, and the warn codes are not
// (they never blocked). It mirrors waivableCodes so a future edit to that map
// that forgets D3/R8 is caught here.
func TestWaivable(t *testing.T) {
	cases := []struct {
		code Code
		want bool
	}{
		{CodeDefaultBranchUnprotected, true},
		{CodeWriteRoleCanPush, true},
		{CodeBotCanPush, true},
		{CodeWriteRoleCanMerge, true},
		{CodeBotCanMerge, true},
		{CodeUnprotectedFilePatterns, true},
		// Never waivable — a read error / unreadable protection must still refuse
		// even an admin-allowed repo (D3/R8).
		{CodeProtectionUnreadable, false},
		// Warn codes never blocked, so there is nothing to waive.
		{CodeBotNotMember, false},
		{CodeRoleUnreadable, false},
	}
	for _, c := range cases {
		if got := Waivable(c.code); got != c.want {
			t.Errorf("Waivable(%q) = %v, want %v", c.code, got, c.want)
		}
	}
}

// TestAllBlocksWaivable pins the set predicate the override gate consumes: an
// override can clear the refusal only when there IS a block finding and EVERY
// block finding is waivable.
func TestAllBlocksWaivable(t *testing.T) {
	cases := []struct {
		name     string
		findings []Finding
		want     bool
	}{
		{
			// No findings at all: nothing blocks, so an override clears nothing.
			name:     "empty",
			findings: nil,
			want:     false,
		},
		{
			name:     "single waivable block",
			findings: []Finding{newFinding(CodeDefaultBranchUnprotected, "unprotected")},
			want:     true,
		},
		{
			name: "multiple waivable blocks",
			findings: []Finding{
				newFinding(CodeDefaultBranchUnprotected, "unprotected"),
				newFinding(CodeWriteRoleCanPush, "write can push"),
				newFinding(CodeBotCanMerge, "bot can merge"),
			},
			want: true,
		},
		{
			// A non-waivable block among waivable ones: the override leaves that one
			// blocking, so it cannot clear the refusal.
			name: "protection_unreadable among waivable blocks",
			findings: []Finding{
				newFinding(CodeDefaultBranchUnprotected, "unprotected"),
				newFinding(CodeProtectionUnreadable, "unreadable"),
			},
			want: false,
		},
		{
			// The lone non-waivable block: still false.
			name:     "only protection_unreadable",
			findings: []Finding{newFinding(CodeProtectionUnreadable, "unreadable")},
			want:     false,
		},
		{
			// Warn-only findings never blocked, so there is no block to waive.
			name: "warn only",
			findings: []Finding{
				newFinding(CodeBotNotMember, "not a member"),
				newFinding(CodeRoleUnreadable, "role unreadable"),
			},
			want: false,
		},
		{
			// A waivable block alongside a warn: the warn is ignored, the block is
			// waivable, so an override clears the refusal.
			name: "waivable block plus warn",
			findings: []Finding{
				newFinding(CodeDefaultBranchUnprotected, "unprotected"),
				newFinding(CodeBotNotMember, "not a member"),
			},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AllBlocksWaivable(c.findings); got != c.want {
				t.Errorf("AllBlocksWaivable(%+v) = %v, want %v", c.findings, got, c.want)
			}
		})
	}
}
