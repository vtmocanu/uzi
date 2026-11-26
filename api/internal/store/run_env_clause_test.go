package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The run_env_v1 claim clause (PRD #4416 M2) sits at every site that carries the issue_key_v1
// claim clause, in the body of fn_placement_admitting_workers, and as the demand arm of
// ListUnplaceableQueuedRunsForEphemeral. Every site decides the run's lane with fn_run_env_lane
// and inlines no kind list, so the lane has one definition (the function, pinned to
// leadcfg.LeadLane by TestRunEnvLaneFunctionLiveDB).

// runEnvClauseRE is the whole capability-first claim clause. The alias is captured so every
// reference in one clause must use the same run alias.
var runEnvClauseRE = regexp.MustCompile(`AND \('run_env_v1' = ANY\([^)\s]+\) OR NOT \(fn_run_env_lane\((\w+)\.kind, (\w+)\.review_target_run_id, (\w+)\.egress_profile_id\) AND ` +
	`\(EXISTS \(SELECT 1 FROM run_env_vars re WHERE re\.user_id = (\w+)\.user_id AND re\.repo_id = (\w+)\.repo_id AND re\.retired_at IS NULL AND re\.pending_delete_at IS NULL AND re\.disabled_at IS NULL\) ` +
	`OR EXISTS \(SELECT 1 FROM run_env_receipts rr WHERE rr\.run_id = (\w+)\.id AND rr\.secret\)\)\)\)`)

func TestRunEnvClaimClauseCountPinned(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	mention := regexp.MustCompile(`'run_env_v1' = ANY\(`)
	lane := regexp.MustCompile(`fn_run_env_lane\(`)
	got := map[string]int{}
	mentions := map[string]int{}
	lanes := map[string]int{}
	for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
		name := strings.Fields(block)[0]
		for _, m := range runEnvClauseRE.FindAllStringSubmatch(block, -1) {
			got[name]++
			for _, alias := range m[1:] {
				if alias != m[1] {
					t.Errorf("%s: a run_env clause mixes run aliases %v", name, m[1:])
				}
			}
		}
		if n := len(mention.FindAllString(block, -1)); n > 0 {
			mentions[name] = n
		}
		if n := len(lane.FindAllString(block, -1)); n > 0 {
			lanes[name] = n
		}
	}
	wantClause := map[string]int{
		"ClaimRun":                             2, // the claimant and the fleet-spread peer arm
		"CountOnlineWorkersClaimableForRun":    1,
		"ListSaturationQueuedRunsForEphemeral": 2,
	}
	for name, n := range wantClause {
		if got[name] != n {
			t.Errorf("%s carries %d run_env claim clauses, want %d", name, got[name], n)
		}
		if mentions[name] != n {
			t.Errorf("%s mentions run_env_v1 %d times, want %d (only as the capability-first clause)", name, mentions[name], n)
		}
		if lanes[name] != n {
			t.Errorf("%s calls fn_run_env_lane %d times, want %d", name, lanes[name], n)
		}
	}
	// The wake-demand arm calls the lane function without the capability test, and the health
	// query asks the lane function once and names the capability against the workers table.
	if lanes["ListUnplaceableQueuedRunsForEphemeral"] != 1 || mentions["ListUnplaceableQueuedRunsForEphemeral"] != 0 {
		t.Errorf("ListUnplaceableQueuedRunsForEphemeral: lane calls %d (want 1), capability mentions %d (want 0)",
			lanes["ListUnplaceableQueuedRunsForEphemeral"], mentions["ListUnplaceableQueuedRunsForEphemeral"])
	}
	if lanes["RunEnvWorkerMissingForRun"] != 1 || mentions["RunEnvWorkerMissingForRun"] != 1 || got["RunEnvWorkerMissingForRun"] != 0 {
		t.Errorf("RunEnvWorkerMissingForRun: lane %d (want 1), capability mentions %d (want 1), claim clauses %d (want 0)",
			lanes["RunEnvWorkerMissingForRun"], mentions["RunEnvWorkerMissingForRun"], got["RunEnvWorkerMissingForRun"])
	}
	for name := range mentions {
		if _, ok := wantClause[name]; !ok && name != "RunEnvWorkerMissingForRun" {
			t.Errorf("%s mentions run_env_v1 but is not a known gate site", name)
		}
	}
	for name := range lanes {
		if _, ok := wantClause[name]; !ok && name != "RunEnvWorkerMissingForRun" && name != "ListUnplaceableQueuedRunsForEphemeral" {
			t.Errorf("%s calls fn_run_env_lane but is not a known gate site", name)
		}
	}
	// No gate site inlines a kind list: runEnvClauseRE admits only the fn_run_env_lane call in
	// the lane position, so a clause that spelled the kinds out would not match and the counts
	// above would fail.
}

// TestRunEnvClauseLivesInThePlacementFunction pins the clause in the body of
// fn_placement_admitting_workers (its last definition across the migrations), the one place the
// capability-gap trigger and the wake-demand query read the per-worker claim clauses.
func TestRunEnvClauseLivesInThePlacementFunction(t *testing.T) {
	body := placementFunctionBody(t, "migrations")
	ms := runEnvClauseRE.FindAllStringSubmatch(body, -1)
	if len(ms) != 1 {
		t.Fatalf("fn_placement_admitting_workers carries %d run_env clauses, want 1", len(ms))
	}
	if ms[0][1] != "r" {
		t.Errorf("the placement function's run alias = %q, want r", ms[0][1])
	}
	if strings.Count(body, "fn_run_env_lane(") != 1 {
		t.Errorf("the placement function must decide the lane only through fn_run_env_lane")
	}
}
