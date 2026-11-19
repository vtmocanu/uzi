package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestIssueKeyClaimClauseCountPinned pins where the standalone issue_key_v1 claim clause
// (PRD #4173) lives in queries/runtime.sql: at every site that carries the ci_config_guard_v1
// claim clause, and nowhere else. A new mirror of the claim gate that forgets it changes this
// table. The live-DB tests in workersvc prove the behaviour at each site.
//
// ListUnplaceableQueuedRunsForEphemeral no longer carries the per-worker clauses inline: they
// live in fn_placement_admitting_workers (PRD #4205 M5), which the hibernation wake-demand query
// calls too, so the clause is pinned once in the function body (the migration that adds it).
func TestIssueKeyClaimClauseCountPinned(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	// The claim clause is capability arm first, so an upgraded worker never probes repos; the
	// health query only mentions the capability, so mention counts every site and clause only
	// the claim form.
	mention := regexp.MustCompile(`'issue_key_v1' = ANY\(`)
	clause := regexp.MustCompile(`\('issue_key_v1' = ANY\([^)\s]+\) OR NOT EXISTS \(SELECT 1 FROM repos lr WHERE lr\.id = \w+\.repo_id AND lr\.issue_tracker_connection_id IS NOT NULL\)\)`)
	guard := regexp.MustCompile(`NOT (?:r|run)\.ci_config_guard OR 'ci_config_guard_v1' = ANY\(`)

	got := map[string]int{}
	mentions := map[string]int{}
	guards := map[string]int{}
	for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
		name := strings.Fields(block)[0]
		if n := len(clause.FindAllString(block, -1)); n > 0 {
			got[name] = n
		}
		if n := len(mention.FindAllString(block, -1)); n > 0 {
			mentions[name] = n
		}
		if n := len(guard.FindAllString(block, -1)); n > 0 {
			guards[name] = n
		}
	}
	want := map[string]int{
		"ClaimRun":                             2, // the claimant and the fleet-spread peer arm
		"CountOnlineWorkersClaimableForRun":    1,
		"ListSaturationQueuedRunsForEphemeral": 2,
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s carries %d issue_key clauses, want %d", name, got[name], n)
		}
	}
	for name, n := range mentions {
		if name == "IssueKeyWorkerMissingForRepo" {
			if n != 1 || got[name] != 0 {
				t.Errorf("health query mentions issue_key_v1 %d times with %d claim clauses, want 1 and 0", n, got[name])
			}
			continue
		}
		if want[name] == 0 {
			t.Errorf("%s carries %d unexpected issue_key mentions", name, n)
		} else if n != got[name] {
			t.Errorf("%s mentions issue_key_v1 %d times but %d are the capability-first claim clause", name, n, got[name])
		}
	}
	if mentions["IssueKeyWorkerMissingForRepo"] != 1 {
		t.Error("the health query IssueKeyWorkerMissingForRepo was not found")
	}
	for name, n := range guards {
		if got[name] != n {
			t.Errorf("%s mirrors the ci_config_guard clause %d times but the issue_key clause %d times", name, n, got[name])
		}
	}
}

// TestIssueKeyClauseLivesInThePlacementFunction pins the issue_key_v1 claim clause in the body of
// fn_placement_admitting_workers (its LAST definition across the migrations), the one place the
// capability-gap trigger and the wake-demand query both read the per-worker claim clauses.
func TestIssueKeyClauseLivesInThePlacementFunction(t *testing.T) {
	body := placementFunctionBody(t, "migrations")
	clause := regexp.MustCompile(`\('issue_key_v1' = ANY\(w\.protocol_capabilities\) OR NOT EXISTS \(SELECT 1 FROM repos lr WHERE lr\.id = r\.repo_id AND lr\.issue_tracker_connection_id IS NOT NULL\)\)`)
	if n := len(clause.FindAllString(body, -1)); n != 1 {
		t.Fatalf("fn_placement_admitting_workers carries %d issue_key clauses, want 1", n)
	}
}
