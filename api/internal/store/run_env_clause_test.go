package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The run_env_v1 claim clause (PRD #4416 M2) is the twin of the issue_key_v1 one: it lives at
// every site that carries the ci_config_guard_v1 clause, and nowhere else. These pins read the
// sources; the LiveDB tests in workersvc prove the behaviour at each site.

// runEnvClause matches the capability-first claim clause with any alias and capability list.
var runEnvClause = regexp.MustCompile(`\('run_env_v1' = ANY\([^)\s]+\) OR NOT \(fn_run_env_lane\((\w+)\.kind, (\w+)\.review_target_run_id, (\w+)\.egress_profile_id\) AND \(EXISTS \(SELECT 1 FROM run_env_vars ev WHERE ev\.user_id = (\w+)\.user_id AND ev\.repo_id = (\w+)\.repo_id AND ev\.retired_at IS NULL AND ev\.pending_delete_at IS NULL AND ev\.disabled_at IS NULL\) OR EXISTS \(SELECT 1 FROM run_env_receipts rr WHERE rr\.run_id = (\w+)\.id AND rr\.secret\)\)\)\)`)

func TestRunEnvClaimClauseCountPinned(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	mention := regexp.MustCompile(`'run_env_v1' = ANY\(`)
	guard := regexp.MustCompile(`NOT (?:r|run)\.ci_config_guard OR 'ci_config_guard_v1' = ANY\(`)
	got := map[string]int{}
	mentions := map[string]int{}
	guards := map[string]int{}
	for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
		name := strings.Fields(block)[0]
		for _, m := range runEnvClause.FindAllStringSubmatch(block, -1) {
			got[name]++
			// One alias per clause: the lane columns, the owner, the repo and the run id all come from the run row.
			for _, a := range m[2:] {
				if a != m[1] {
					t.Errorf("%s: a run_env clause mixes aliases %q and %q", name, m[1], a)
				}
			}
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
			t.Errorf("%s carries %d run_env clauses, want %d", name, got[name], n)
		}
	}
	for name, n := range mentions {
		switch name {
		case "RunEnvWorkerMissingForRun":
			if n != 1 || got[name] != 0 {
				t.Errorf("health query mentions run_env_v1 %d times with %d claim clauses, want 1 and 0", n, got[name])
			}
		default:
			if want[name] == 0 {
				t.Errorf("%s carries %d unexpected run_env_v1 mentions", name, n)
			} else if n != got[name] {
				t.Errorf("%s mentions run_env_v1 %d times but %d are the capability-first claim clause", name, n, got[name])
			}
		}
	}
	// RunEnvWorkerMissingForRun lives in the receipts file, not runtime.sql; check it there.
	receipts, err := os.ReadFile("queries/run_env_receipts.sql")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(receipts), "'run_env_v1' = ANY(w.protocol_capabilities)"); n != 1 {
		t.Errorf("the health query carries run_env_v1 %d times, want 1", n)
	}
	for name, n := range guards {
		if got[name] != n {
			t.Errorf("%s mirrors the ci_config_guard clause %d times but the run_env clause %d times", name, n, got[name])
		}
	}
	// The demand arm of ListUnplaceableQueuedRunsForEphemeral names the lane function too.
	demand := regexp.MustCompile(`OR \(fn_run_env_lane\(r\.kind, r\.review_target_run_id, r\.egress_profile_id\) AND \(EXISTS \(SELECT 1 FROM run_env_vars ev WHERE ev\.user_id = r\.user_id AND ev\.repo_id = r\.repo_id`)
	for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
		if strings.Fields(block)[0] != "ListUnplaceableQueuedRunsForEphemeral" {
			continue
		}
		if n := len(demand.FindAllString(block, -1)); n != 1 {
			t.Errorf("ListUnplaceableQueuedRunsForEphemeral carries %d run_env demand arms, want 1", n)
		}
	}
}

// TestRunEnvLaneFunctionIsTheOnlyKindList is the source scan: every gate site calls
// fn_run_env_lane and none inlines a kind list, so a lane change is one edit in two places (the
// Go predicate and the function) and the LiveDB pin test compares those two.
func TestRunEnvLaneFunctionIsTheOnlyKindList(t *testing.T) {
	kindList := regexp.MustCompile(`(?i)kind\s+(?:NOT\s+)?IN\s*\(`)
	// Every claim clause line (runtime.sql, the placement function) calls the function and inlines
	// no kind list; the demand arm line names the function too.
	scan := func(label, text string) int {
		sites := 0
		for _, line := range strings.Split(stripSQLCommentLines(text), "\n") {
			if !strings.Contains(line, "run_env_vars ev") {
				continue
			}
			sites++
			if !strings.Contains(line, "fn_run_env_lane(") {
				t.Errorf("%s: a run-env gate line does not call fn_run_env_lane: %.140s", label, line)
			}
			if kindList.MatchString(line) {
				t.Errorf("%s: a run-env gate line inlines a kind list: %.140s", label, line)
			}
		}
		return sites
	}
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	// 5 capability clauses + 1 demand arm.
	if n := scan("runtime.sql", string(raw)); n != 6 {
		t.Errorf("runtime.sql carries %d run-env gate lines, want 6", n)
	}
	if n := scan("fn_placement_admitting_workers", placementFunctionBody(t, "migrations")); n != 1 {
		t.Errorf("fn_placement_admitting_workers carries %d run-env gate lines, want 1", n)
	}
	receipts, err := os.ReadFile("queries/run_env_receipts.sql")
	if err != nil {
		t.Fatal(err)
	}
	health := ""
	for _, block := range strings.Split(string(receipts), "\n-- name: ")[1:] {
		if strings.Fields(block)[0] == "RunEnvWorkerMissingForRun" {
			health = stripSQLCommentLines(block)
		}
	}
	if !strings.Contains(health, "fn_run_env_lane(r.kind, r.review_target_run_id, r.egress_profile_id)") || kindList.MatchString(health) {
		t.Error("the health query must call fn_run_env_lane and inline no kind list")
	}
}
