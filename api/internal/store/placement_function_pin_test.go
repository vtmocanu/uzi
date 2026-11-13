package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// placementFunctionCall is the call token of the placement function. Query texts never contain
// its CREATE FUNCTION header, so counting this token counts call sites only.
const placementFunctionCall = "fn_placement_admitting_workers("

var placementFunctionHeader = regexp.MustCompile(`(?i)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+fn_placement_admitting_workers\(`)

// stripSQLCommentLines drops lines whose first non-space characters are "--" (SQL) or "//" (the
// Go doc comments of generated code), so prose can neither satisfy nor inflate a textual pin.
func stripSQLCommentLines(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// placementFunctionBody returns the comment-stripped body (between the $$ markers) of the LAST
// CREATE [OR REPLACE] FUNCTION fn_placement_admitting_workers in dir's migrations (files sorted
// by name, Down sections ignored). It fails the test when none is found or the body is empty.
func placementFunctionBody(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	body := ""
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // fixed migrations directory
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		locs := placementFunctionHeader.FindAllStringIndex(up, -1)
		if len(locs) == 0 {
			continue
		}
		rest := up[locs[len(locs)-1][1]:]
		_, afterOpen, ok := strings.Cut(rest, "$$")
		if !ok {
			t.Fatalf("%s: fn_placement_admitting_workers has no opening $$", name)
		}
		inner, _, ok := strings.Cut(afterOpen, "$$")
		if !ok {
			t.Fatalf("%s: fn_placement_admitting_workers has no closing $$", name)
		}
		body = stripSQLCommentLines(inner)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("fn_placement_admitting_workers body not found (or empty) in migrations")
	}
	return body
}

// expandPlacementFunction returns text with the placement function body appended once per call of
// fn_placement_admitting_workers( outside comment lines, so a textual pin over a query that moved
// its per-worker block into the function still sees the clauses that execute. It works on raw text
// (a query block, the whole generated runtime.sql.go) and leaves text without a call unchanged.
func expandPlacementFunction(t *testing.T, text string) string {
	t.Helper()
	n := strings.Count(stripSQLCommentLines(text), placementFunctionCall)
	if n == 0 {
		return text
	}
	body := placementFunctionBody(t, "migrations")
	return text + strings.Repeat("\n"+body, n)
}

// TestPlacementAdmittingWorkersCarriesEveryInlineClause pins that the function holds every clause
// of the per-worker block ListUnplaceableQueuedRunsForEphemeral carried inline before PRD #4205 M5.
// The counts are those of the removed block. It pins the owner scoping, the ephemeral/lease arm
// with its Docker-preference conjunct, the structural predicates and each capability consequent;
// the capability antecedents are pinned by the per-clause count tests (ci_config_guard,
// repo_agent_source, semantic, agent_source_request, cross-check), which also read this body.
func TestPlacementAdmittingWorkersCarriesEveryInlineClause(t *testing.T) {
	body := placementFunctionBody(t, "migrations")
	want := map[string]int{
		// Owner scoping: without it the wake-demand caller could pick another user's worker.
		"WHERE w.user_id = r.user_id":                                            1,
		"(r.kind = 'cross_check' OR NOT w.ephemeral":                             1,
		"NOT fn_worker_accepts_new_intake(w),":                                   1,
		"p_ephemeral_lease, now(),":                                              1,
		"(NOT p_docker_preference_applies OR COALESCE(w.docker_enabled, false))": 1,
		"fn_cross_check_child_eligible(":                                         1,
		"fn_ephemeral_lease_admits(":                                             1,
		"AND NOT w.isolated_lane":                                                1,
		"pc.worker_id IS DISTINCT FROM w.id":                                     1,
		"protocol_check.round = (SELECT max(latest.round)":                       1,
		"r.required_capabilities <@":                                             1,
	}
	for _, c := range []string{
		"milestone_reconciliation_claim_v1", "task_checkpoint_coordinates_v1", "publication_resume_v1",
		"completion_interlock_v1", "ci_config_guard_v1", "repo_agent_source_v1", "agent_source_request_v1",
		"provider_policy_hold_v1", "ci_artifacts_v1", "codex_harness_v1", "codex_runtime_v2",
		"codex_completion_interlock_v1", "codex_custom_model_v1", "cross_check_v1",
		"cross_check_codex_lead_v1", "cross_check_semantic_plan_v1", "cross_check_owner_steering_v1",
		"completion_semantic_publication_v1", "cross_check_code_v1", "cross_check_lane_v1",
		"cross_check_rounds_v1", "cross_check_pins_v1",
	} {
		want["'"+c+"' = ANY(w.protocol_capabilities)"] = 1
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got := strings.Count(body, k); got != want[k] {
			t.Fatalf("fn_placement_admitting_workers carries %q %d times, want %d (first mismatch; %d clauses checked)", k, got, want[k], len(keys))
		}
	}
}

// TestListUnplaceableQueuedRunsForEphemeralCallsPlacementFunction pins that the capability-gap
// trigger calls the placement function exactly once and keeps the availability conjuncts the
// function deliberately omits.
func TestListUnplaceableQueuedRunsForEphemeralCallsPlacementFunction(t *testing.T) {
	body, ok := namedQueryBodies(t)["ListUnplaceableQueuedRunsForEphemeral"]
	if !ok {
		t.Fatal("ListUnplaceableQueuedRunsForEphemeral not found")
	}
	if got := strings.Count(body, placementFunctionCall); got != 1 {
		t.Fatalf("calls %s %d times, want 1", placementFunctionCall, got)
	}
	for _, conjunct := range []string{
		"w.status = 'online'",
		"NOT w.maintenance_fenced",
		"w.hibernation_state = 'none'",
		"fn_worker_accepts_new_intake(w)",
		"r.kind = 'cross_check' AND r.worker_id = w.id",
	} {
		if !strings.Contains(body, conjunct) {
			t.Errorf("ListUnplaceableQueuedRunsForEphemeral lost the availability conjunct %q", conjunct)
		}
	}
}
