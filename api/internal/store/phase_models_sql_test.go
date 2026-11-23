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
var phaseModelsKindList = regexp.MustCompile(`(\w+)\.harness IN \('claude', 'codex'\)\s+AND\s+(\w+)\.kind IN \(([^)]*)\)\s+AND\s+(\w+)\.review_target_run_id IS NULL\s+AND\s+(\w+)\.egress_profile_id IS NULL\s+AND\s+EXISTS \(SELECT 1 FROM users pu WHERE pu\.id = (\w+)\.user_id AND \(CASE (\w+)\.harness WHEN 'claude' THEN pu\.planning_claude_model IS NOT NULL OR pu\.planning_claude_effort IS NOT NULL WHEN 'codex' THEN pu\.planning_codex_model IS NOT NULL OR pu\.planning_codex_effort IS NOT NULL ELSE false END\)\)`)

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
		if m[1] != m[2] || m[1] != m[4] || m[1] != m[5] || m[1] != m[6] || m[1] != m[7] {
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
		if !strings.Contains(sqlLineCommentRE.ReplaceAllString(body, ""), "WHEN 'claude' THEN 'claude_phase_models_v1' WHEN 'codex' THEN 'codex_phase_models_v1' END = ANY") && name != "ListActiveRunsForHealth" && name != "ListUnplaceableQueuedRunsForEphemeral" {
			t.Errorf("%s does not test the claude_phase_models_v1 capability", name)
		}
	}
	phaseModelsSiteKinds(t, "fn_placement_admitting_workers", placementFunctionBody(t, "migrations"), 1)
}

// Every custom-root mirror must consider planning while leaving frozen curated roots authoritative.
func TestCustomPlanningModelMirrors(t *testing.T) {
	bodies := namedQueryBodies(t)
	for name, count := range map[string]int{"ClaimRun": 2, "CountOnlineWorkersClaimableForRun": 1, "ListActiveRunsForHealth": 1, "ListSaturationQueuedRunsForEphemeral": 2} {
		body := sqlLineCommentRE.ReplaceAllString(bodies[name], "")
		customPlanningSites(t, name, body, count)
	}
	body := placementFunctionBody(t, "migrations")
	customPlanningSites(t, "placement", body, 1)
	if !strings.Contains(body, "WHEN 'codex' THEN 'codex_phase_models_v1' END = ANY(w.protocol_capabilities)") {
		t.Fatal("placement lacks the harness-specific phase capability consequent")
	}
}

func customPlanningSites(t *testing.T, name, body string, count int) {
	t.Helper()
	pattern := regexp.MustCompile(`(\w+)\.kind IN \('issue', 'ci_fix', 'self_improve', 'prompt', 'mr_rework', 'task'\)\s+AND (\w+)\.egress_profile_id IS NULL\s+AND NOT COALESCE\((\w+)\.model = ANY\(([^)]+)\), false\)\s+AND EXISTS \(SELECT 1 FROM users pu WHERE pu.id = (\w+)\.user_id\s+AND pu.planning_codex_model IS NOT NULL\s+AND NOT \(pu.planning_codex_model = ANY\(([^)]+)\)\)`)
	matches := pattern.FindAllStringSubmatch(body, -1)
	if len(matches) != count {
		t.Fatalf("%s: %d custom planning branches, want %d", name, len(matches), count)
	}
	for _, m := range matches {
		if m[1] != m[2] || m[1] != m[3] || m[1] != m[5] || m[4] != m[6] {
			t.Fatalf("%s: mixed aliases or curated sets in custom planning branch", name)
		}
	}
}
