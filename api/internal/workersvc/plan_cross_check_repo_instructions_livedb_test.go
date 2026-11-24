package workersvc

import (
	"encoding/json"
	"strings"
	"testing"
)

// The plan child claim carries plan-only consent for the repo's root CLAUDE.md: the AND of
// repo_claudemd_enabled and cross_check_repo_instructions_enabled, read at claim time. The
// candidate digest never moves with the toggles, and a code child never carries the field.
func TestPlanCrossCheckRepoInstructionsConsentLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name            string
		claudemd, cross bool
	}{
		{"default off", false, false},
		{"repo claudemd only", true, false},
		{"cross-check instructions only", false, true},
		{"both on", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckerCustodyFixture(t, "plan", "claude", "cross_check", false)
			repoID := mustRun(t, f.env, f.lead).RepoID
			f.env.exec("UPDATE repos SET repo_claudemd_enabled=$2, cross_check_repo_instructions_enabled=$3 WHERE id=$1",
				repoID, tc.claudemd, tc.cross)
			var stored string
			if err := f.env.pool.QueryRow(f.env.ctx, "SELECT encode(candidate_digest,'hex') FROM cross_checks WHERE checker_run_id=$1", f.child).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			p := f.claim(t)
			if p.CrossCheck == nil || p.CrossCheck.Stage != "plan" {
				t.Fatalf("plan child claim missing cross_check: %+v", p.CrossCheck)
			}
			got := p.CrossCheck.PlanRepoInstructionsEnabled
			if got == nil || *got != (tc.claudemd && tc.cross) {
				t.Fatalf("plan_repo_instructions_enabled = %v, want %v", got, tc.claudemd && tc.cross)
			}
			if p.CrossCheck.CandidateDigest != stored {
				t.Fatalf("candidate digest %s moved from stored %s", p.CrossCheck.CandidateDigest, stored)
			}
			if p.Repo.ClaudemdEnabled != tc.claudemd {
				t.Fatalf("repo.claudemd_enabled = %v, want %v", p.Repo.ClaudemdEnabled, tc.claudemd)
			}
			if err := ValidateCrossCheckClaim(p); err != nil {
				t.Fatalf("claim not admitted: %v", err)
			}
			raw, err := MarshalCrossCheckClaim(p)
			if err != nil || !strings.Contains(string(raw), `"plan_repo_instructions_enabled":`) {
				t.Fatalf("marshalled plan claim: %v", err)
			}
		})
	}

	t.Run("code child omits the field", func(t *testing.T) {
		f := newCheckerCustodyFixture(t, "code", "codex", "cross_check", false)
		repoID := mustRun(t, f.env, f.lead).RepoID
		f.env.exec("UPDATE repos SET repo_claudemd_enabled=true, cross_check_repo_instructions_enabled=true WHERE id=$1", repoID)
		p := f.claim(t)
		if p.CrossCheck == nil || p.CrossCheck.Stage != "code" || p.CrossCheck.PlanRepoInstructionsEnabled != nil {
			t.Fatalf("code claim: %+v", p.CrossCheck)
		}
		raw, err := json.Marshal(p.CrossCheck)
		if err != nil || strings.Contains(string(raw), "plan_repo_instructions_enabled") {
			t.Fatalf("code cross_check JSON carries the plan-only key: %v", err)
		}
	})
}
