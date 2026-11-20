package store_test

import (
	"regexp"
	"strings"
	"testing"
)

// The run-env claim gate (PRD #4416 M2): a run in the run-env lane whose repo has an enabled variable,
// or that already holds a secret receipt, is claimable only by a worker reporting run_env_v1. The lane
// is decided ONLY by the SQL function fn_run_env_lane (migration 00498), never by an inline kind list,
// so it cannot drift from leadcfg.LeadLane. These source-scan tests pin where the clause lives; the
// workersvc LiveDB tests prove the behaviour at the claim.

// runEnvGateClause matches one capability-first gate clause with its aliases captured, and nothing
// else: a kind list or a changed predicate would not match.
var runEnvGateClause = regexp.MustCompile(
	`\('run_env_v1' = ANY\([^)\s]+\) OR NOT \(fn_run_env_lane\((\w+)\.kind, (\w+)\.review_target_run_id, (\w+)\.egress_profile_id\) AND ` +
		`\(EXISTS \(SELECT 1 FROM run_env_vars v WHERE v\.user_id = (\w+)\.user_id AND v\.repo_id = (\w+)\.repo_id AND v\.retired_at IS NULL AND v\.pending_delete_at IS NULL AND v\.disabled_at IS NULL\) ` +
		`OR EXISTS \(SELECT 1 FROM run_env_receipts rr WHERE rr\.run_id = (\w+)\.id AND rr\.secret\)\)\)\)`)

var runEnvCapabilityMention = regexp.MustCompile(`'run_env_v1' = ANY\(`)

func TestRunEnvGateClauseCountPinned(t *testing.T) {
	want := map[string]int{
		"ClaimRun":                             2, // the claimant and the fleet-spread peer arm
		"CountOnlineWorkersClaimableForRun":    1,
		"ListSaturationQueuedRunsForEphemeral": 2,
	}
	got := map[string]int{}
	mentions := map[string]int{}
	for name, body := range namedQueryBodies(t) {
		matches := runEnvGateClause.FindAllStringSubmatch(body, -1)
		if len(matches) > 0 {
			got[name] = len(matches)
		}
		for _, m := range matches {
			for i := 2; i < len(m); i++ {
				if m[i] != m[1] {
					t.Errorf("%s: a run-env clause mixes aliases %q and %q", name, m[1], m[i])
				}
			}
		}
		if n := len(runEnvCapabilityMention.FindAllString(body, -1)); n > 0 {
			mentions[name] = n
		}
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s carries %d run-env gate clauses, want %d", name, got[name], n)
		}
	}
	for name, n := range mentions {
		switch {
		case name == "RunEnvWorkerMissingForRun":
			if n != 1 || got[name] != 0 {
				t.Errorf("health query mentions run_env_v1 %d times with %d claim clauses, want 1 and 0", n, got[name])
			}
		case want[name] == 0:
			t.Errorf("%s carries %d unexpected run_env_v1 mentions", name, n)
		case n != got[name]:
			t.Errorf("%s mentions run_env_v1 %d times but %d are the full gate clause", name, n, got[name])
		}
	}
	if mentions["RunEnvWorkerMissingForRun"] != 1 {
		t.Error("the health query RunEnvWorkerMissingForRun was not found")
	}
}

// TestRunEnvGateSitesUseTheLaneFunctionOnly: every query that decides the lane calls fn_run_env_lane,
// and none of them inlines the lane's kind list (a copy would drift from leadcfg.LeadLane).
func TestRunEnvGateSitesUseTheLaneFunctionOnly(t *testing.T) {
	laneCalls := map[string]int{
		"ClaimRun": 2, "CountOnlineWorkersClaimableForRun": 1, "ListSaturationQueuedRunsForEphemeral": 2,
		"RunEnvWorkerMissingForRun":             1,
		"ListUnplaceableQueuedRunsForEphemeral": 1, // the demand arm (the per-worker clause is in the placement function)
	}
	for name, body := range namedQueryBodies(t) {
		n := strings.Count(body, "fn_run_env_lane(")
		if n != laneCalls[name] {
			t.Errorf("%s calls fn_run_env_lane %d times, want %d", name, n, laneCalls[name])
		}
		if n == 0 {
			continue
		}
		// The run-env predicate text around every call must not carry a kind list.
		for i := strings.Index(body, "fn_run_env_lane("); i >= 0; {
			window := body[i:min(i+400, len(body))]
			if strings.Contains(window, "'self_improve'") || strings.Contains(window, "'mr_rework'") || strings.Contains(window, "'ci_fix'") {
				t.Errorf("%s inlines a kind list next to fn_run_env_lane", name)
			}
			next := strings.Index(body[i+1:], "fn_run_env_lane(")
			if next < 0 {
				break
			}
			i += 1 + next
		}
	}
}

// TestRunEnvClauseLivesInThePlacementFunction pins the clause in the body of
// fn_placement_admitting_workers (its last definition across the migrations), where the capability-gap
// trigger and the wake-demand query read the per-worker claim clauses.
func TestRunEnvClauseLivesInThePlacementFunction(t *testing.T) {
	body := placementFunctionBody(t, "migrations")
	if n := len(runEnvGateClause.FindAllString(body, -1)); n != 1 {
		t.Fatalf("fn_placement_admitting_workers carries %d run-env gate clauses, want 1", n)
	}
	m := runEnvGateClause.FindStringSubmatch(body)
	if m[1] != "r" || !strings.Contains(m[0], "ANY(w.protocol_capabilities)") {
		t.Fatalf("the placement clause reads run %q and %q, want r and w.protocol_capabilities", m[1], m[0][:40])
	}
	if got := strings.Count(body, "fn_run_env_lane("); got != 1 {
		t.Fatalf("fn_placement_admitting_workers calls fn_run_env_lane %d times, want 1", got)
	}
}
