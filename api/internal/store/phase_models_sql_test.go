package store_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// phaseModelsKindList matches the kind list of the claude_phase_models_v1 predicate (PRD #3252) at
// one site: the Claude harness test, the lead-lane kind list, the review and egress exclusions and
// the planning-column EXISTS, all on the same run alias.
var phaseModelsKindList = regexp.MustCompile(`(\w+)\.harness = 'claude'\s+AND\s+(\w+)\.kind IN \(([^)]*)\)\s+AND\s+(\w+)\.review_target_run_id IS NULL\s+AND\s+(\w+)\.egress_profile_id IS NULL\s+AND\s+EXISTS \(SELECT 1 FROM users pu WHERE pu\.id = (\w+)\.user_id AND \(pu\.planning_claude_model IS NOT NULL OR pu\.planning_claude_effort IS NOT NULL\)\)`)

var sqlLineCommentRE = regexp.MustCompile(`--[^\n]*`)

// phaseModelsExpectedKinds is the explicit planning set; the registry-derived oracle below must
// agree with it, so a new lead-lane kind fails here until every SQL site is updated.
var phaseModelsExpectedKinds = []string{"ci_fix", "issue", "mr_rework", "prompt", "self_improve", "task"}

func phaseModelsSiteKinds(t *testing.T, label, text string, wantSites int) {
	t.Helper()
	matches := phaseModelsKindList.FindAllStringSubmatch(text, -1)
	if len(matches) != wantSites {
		t.Fatalf("%s carries the phase-models predicate %d times, want %d", label, len(matches), wantSites)
	}
	for _, m := range matches {
		if m[1] != m[2] || m[1] != m[4] || m[1] != m[5] || m[1] != m[6] {
			t.Errorf("%s: predicate mixes run aliases %v", label, m[1:2])
		}
		var got []string
		for _, k := range strings.Split(m[3], ",") {
			got = append(got, strings.Trim(strings.TrimSpace(k), "'"))
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(phaseModelsExpectedKinds, ",") {
			t.Errorf("%s: predicate kinds %v, want %v", label, got, phaseModelsExpectedKinds)
		}
	}
}

// TestPhaseModelsPredicateKindListMatchesLeadLane pins that every SQL copy of the planning
// predicate lists exactly the lead-lane kinds (leadcfg.LeadLane over the kind registry), so a new
// lead-lane kind cannot be planned without every claimability mirror learning about it.
func TestPhaseModelsPredicateKindListMatchesLeadLane(t *testing.T) {
	var oracle []string
	for _, k := range runkind.All() {
		if leadcfg.LeadLane(k, false, false) {
			oracle = append(oracle, k)
		}
	}
	sort.Strings(oracle)
	if strings.Join(oracle, ",") != strings.Join(phaseModelsExpectedKinds, ",") {
		t.Fatalf("lead-lane kinds %v differ from the explicit planning set %v; update every SQL site", oracle, phaseModelsExpectedKinds)
	}
	bodies := namedQueryBodies(t)
	for name, sites := range map[string]int{
		"ClaimRun":                             2,
		"CountOnlineWorkersClaimableForRun":    1,
		"ListSaturationQueuedRunsForEphemeral": 2,
		"ListActiveRunsForHealth":              1,
		// The demand gate of the capability-gap trigger (a phase-only run with no required caps).
		"ListUnplaceableQueuedRunsForEphemeral": 1,
	} {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("%s not found", name)
		}
		phaseModelsSiteKinds(t, name, body, sites)
		// Strip SQL comments first so a comment naming the capability cannot satisfy the pairing check.
		if !strings.Contains(sqlLineCommentRE.ReplaceAllString(body, ""), "claude_phase_models_v1") && name != "ListActiveRunsForHealth" && name != "ListUnplaceableQueuedRunsForEphemeral" {
			t.Errorf("%s does not test the claude_phase_models_v1 capability", name)
		}
	}
	phaseModelsSiteKinds(t, "fn_placement_admitting_workers", placementFunctionBody(t, "migrations"), 1)
}
