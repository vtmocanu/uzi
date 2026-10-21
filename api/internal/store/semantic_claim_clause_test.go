package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestSemanticClaimClausesPinnedPerQuery pins where the two PRD #1230 semantic claim clauses
// (the plan-stage checker capability and the semantic lead publication capability) live in
// queries/runtime.sql: at every site that mirrors the cross_check_codex_lead_v1 conjunct, and
// nowhere else. The live-DB tests in workersvc prove the behaviour at each site; this guards
// the set of sites so a new claim, placement, health or spread copy cannot omit a clause.
func TestSemanticClaimClausesPinnedPerQuery(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	checker := regexp.MustCompile(`AND \(NOT \((\w+)\.kind = 'cross_check' AND EXISTS \(SELECT 1 FROM semantic_planning sp WHERE sp\.run_id = \w+\.target_run_id\) AND EXISTS \(SELECT 1 FROM cross_checks pc WHERE pc\.checker_run_id = \w+\.id AND pc\.stage = 'plan'\)\)\s+OR 'cross_check_semantic_plan_v1' = ANY\(`)
	lead := regexp.MustCompile(`AND \((\w+)\.kind = 'cross_check' OR NOT EXISTS \(SELECT 1 FROM semantic_planning sp WHERE sp\.run_id = \w+\.id\)\s+OR 'completion_semantic_publication_v1' = ANY\(`)
	codexLead := regexp.MustCompile(`OR 'cross_check_codex_lead_v1' = ANY\(`)
	strayChecker := regexp.MustCompile(`'cross_check_semantic_plan_v1'`)
	strayLead := regexp.MustCompile(`'completion_semantic_publication_v1'`)

	want := map[string]int{
		"ClaimRun":                              2, // the claimant and the fleet-spread peer arm
		"CountOnlineWorkersClaimableForRun":     1,
		"ListUnplaceableQueuedRunsForEphemeral": 1,
		"ListSaturationQueuedRunsForEphemeral":  2, // the capable-worker test and the free-slot test
	}
	for _, c := range []struct {
		label string
		re    *regexp.Regexp
		stray *regexp.Regexp
	}{{"checker", checker, strayChecker}, {"lead", lead, strayLead}} {
		got, strays, codex := map[string]int{}, map[string]int{}, map[string]int{}
		for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
			name := strings.Fields(block)[0]
			if n := len(c.re.FindAllString(block, -1)); n > 0 {
				got[name] = n
			}
			// GetRunSemanticHealth only counts capable workers for the queued reason; it is no claim copy.
			if n := len(c.stray.FindAllString(block, -1)); n > 0 && name != "GetRunSemanticHealth" {
				strays[name] = n
			}
			if n := len(codexLead.FindAllString(block, -1)); n > 0 {
				codex[name] = n
			}
		}
		for name, n := range want {
			if got[name] != n {
				t.Errorf("%s clause: %s carries %d, want %d", c.label, name, got[name], n)
			}
		}
		for name, n := range strays {
			if got[name] != n {
				t.Errorf("%s clause: %s mentions the capability %d times but carries the clause %d times", c.label, name, n, got[name])
			}
		}
		for name, n := range codex {
			if got[name] != n {
				t.Errorf("%s clause: %s mirrors cross_check_codex_lead_v1 %d times but carries the clause %d times", c.label, name, n, got[name])
			}
		}
	}
}
