package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCIConfigGuardClaimClauseCountPinned pins where the standalone ci_config_guard_v1 claim
// clause (PRD #2865) lives in queries/runtime.sql: it must sit at every site that mirrors the
// completion_interlock_v1 claim clause for ordinary runs, and nowhere else. A new mirror of the
// claim gate that forgets the clause (or a stray copy) changes this table. The live-DB tests in
// workersvc prove the clause's behaviour at each site; this guards the set of sites.
func TestCIConfigGuardClaimClauseCountPinned(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	clause := regexp.MustCompile(`NOT (?:r|run)\.ci_config_guard OR 'ci_config_guard_v1' = ANY\(`)
	stray := regexp.MustCompile(`'ci_config_guard_v1' = ANY\(`)
	interlock := regexp.MustCompile(`\.completion_contract_version IS NULL\s+OR 'completion_interlock_v1' = ANY\(`)

	got := map[string]int{}
	strays := map[string]int{}
	interlocks := map[string]int{}
	for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
		name := strings.Fields(block)[0]
		if n := len(clause.FindAllString(block, -1)); n > 0 {
			got[name] = n
		}
		if n := len(stray.FindAllString(block, -1)); n > 0 {
			strays[name] = n
		}
		if n := len(interlock.FindAllString(block, -1)); n > 0 {
			interlocks[name] = n
		}
	}
	want := map[string]int{
		"ClaimRun":                              2, // the claimant and the fleet-spread peer arm
		"CountOnlineWorkersClaimableForRun":     1,
		"ListUnplaceableQueuedRunsForEphemeral": 1,
		"ListSaturationQueuedRunsForEphemeral":  2, // the capable-worker test and the free-slot test
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s carries %d ci_config_guard clauses, want %d", name, got[name], n)
		}
	}
	for name, n := range got {
		if want[name] == 0 {
			t.Errorf("%s carries %d unexpected ci_config_guard clauses", name, n)
		}
	}
	// Every query that mirrors completion_interlock_v1 for ordinary runs carries the guard
	// clause too (the interlock mirror is the structural template for the claim gate).
	for name, n := range interlocks {
		if got[name] != n {
			t.Errorf("%s mirrors completion_interlock_v1 %d times but ci_config_guard_v1 %d times", name, n, got[name])
		}
	}
	// Both health counts require the guard; the artifact count requires it on
	// the same worker as ci_artifacts_v1. Neither is a claim clause.
	for _, name := range []string{"CountOnlineWorkersSatisfyingCIConfigGuard", "CountOnlineWorkersSatisfyingCIArtifacts"} {
		if strays[name] != 1 {
			t.Errorf("health count %s mentions ci_config_guard_v1 %d times, want 1", name, strays[name])
		}
	}
	total := 0
	for _, n := range strays {
		total += n
	}
	if total != 8 {
		t.Errorf("ci_config_guard_v1 appears in %d ANY() tests across runtime.sql, want 8 (6 claim clauses + 2 health counts): %v", total, strays)
	}
}
