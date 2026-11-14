package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAgentSourceRequestClaimClauseSitesPinned(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	clause := regexp.MustCompile(`\((?:r|run)\.agent_source_requested IS NULL OR 'agent_source_request_v1' = ANY\(`)
	mention := regexp.MustCompile(`'agent_source_request_v1' = ANY\(`)
	guard := regexp.MustCompile(`NOT (?:r|run)\.ci_config_guard OR 'ci_config_guard_v1' = ANY\(`)

	clauses := map[string]int{}
	mentions := map[string]int{}
	guards := map[string]int{}
	for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
		name := strings.Fields(block)[0]
		block = expandPlacementFunction(t, block)
		if n := len(clause.FindAllString(block, -1)); n > 0 {
			clauses[name] = n
		}
		if n := len(mention.FindAllString(block, -1)); n > 0 {
			mentions[name] = n
		}
		if n := len(guard.FindAllString(block, -1)); n > 0 {
			guards[name] = n
		}
	}
	if clauses["ClaimRun"] != 2 {
		t.Errorf("ClaimRun carries %d agent_source_request clauses, want 2 (claimant and fleet-spread peer)", clauses["ClaimRun"])
	}
	for name, n := range guards {
		if clauses[name] != n {
			t.Errorf("%s mirrors the claim gate %d times but carries %d agent_source_request clauses", name, n, clauses[name])
		}
	}
	for name, n := range clauses {
		if guards[name] != n {
			t.Errorf("%s carries %d unexpected agent_source_request clauses", name, n)
		}
	}
	healthCounts := []string{
		"CountOnlineWorkersSatisfyingAgentSourceRequest",
		// Issue #4005: the one-worker intersection with repo_agent_source_v1.
		"CountOnlineWorkersSatisfyingRepoAgentSourceAndRequest",
	}
	isHealthCount := func(name string) bool {
		for _, h := range healthCounts {
			if name == h {
				return true
			}
		}
		return false
	}
	for name, n := range mentions {
		if isHealthCount(name) {
			if n != 1 {
				t.Errorf("health count %s mentions agent_source_request_v1 %d times, want 1", name, n)
			}
			continue
		}
		if clauses[name] != n {
			t.Errorf("%s mentions agent_source_request_v1 %d times outside a claim clause", name, n-clauses[name])
		}
	}
	for _, h := range healthCounts {
		if mentions[h] != 1 {
			t.Errorf("%s must test agent_source_request_v1", h)
		}
	}
}
