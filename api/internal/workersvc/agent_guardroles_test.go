package workersvc

import (
	"context"
	"testing"
)

// excludedGuardRoles reports the guard roles a selection EXPLICITLY excludes, in
// exclusion order (PRD #319 M3, D4). It fires only on an active exclusion — never on a
// role merely absent — and ignores non-guard exclusions.
func TestExcludedGuardRoles(t *testing.T) {
	cases := []struct {
		name string
		sel  AgentSelection
		want []string
	}{
		{
			name: "spec-keeper excluded",
			sel:  AgentSelection{Source: "own", Exclusions: []string{"spec-keeper"}},
			want: []string{"spec-keeper"},
		},
		{
			name: "non-guard exclusion notifies nothing",
			sel:  AgentSelection{Exclusions: []string{"tester"}},
			want: nil,
		},
		{
			name: "guard picked out of a mixed list, in exclusion order",
			sel:  AgentSelection{Exclusions: []string{"tester", "spec-keeper"}},
			want: []string{"spec-keeper"},
		},
		{
			name: "no exclusions",
			sel:  AgentSelection{Source: "own"},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := excludedGuardRoles(tc.sel)
			if len(got) != len(tc.want) {
				t.Fatalf("excludedGuardRoles = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("excludedGuardRoles = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// An accepted approve surfaces the excluded guard role(s) on SubmitInputResult so the
// handler can emit the owner heads-up (PRD #319 M3). Populated only after
// validateSelection accepts the exclusion; a valid non-guard exclusion surfaces nothing.
func TestSubmitInputApproveSurfacesExcludedGuardRole(t *testing.T) {
	guarded := []RepoAgent{
		{Name: "coder", Description: "Implements changes."},
		{Name: "spec-keeper", Description: "Guards the specs."},
	}
	for _, tc := range []struct {
		name      string
		exclusion string
		want      []string
	}{
		{name: "guard role excluded", exclusion: "spec-keeper", want: []string{"spec-keeper"}},
		{name: "non-guard exclusion", exclusion: "coder", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, svc, user, runID := gatedRun(t)
			fs.runByID.RepoAgents = repoAgentsJSON(t, guarded)
			sel := &AgentSelection{Source: AgentSourceRepo, Exclusions: []string{tc.exclusion}}

			res, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", sel)
			if err != nil {
				t.Fatalf("SubmitInput: %v", err)
			}
			if len(res.ExcludedGuardRoles) != len(tc.want) {
				t.Fatalf("ExcludedGuardRoles = %v, want %v", res.ExcludedGuardRoles, tc.want)
			}
			for i := range tc.want {
				if res.ExcludedGuardRoles[i] != tc.want[i] {
					t.Fatalf("ExcludedGuardRoles = %v, want %v", res.ExcludedGuardRoles, tc.want)
				}
			}
		})
	}
}
