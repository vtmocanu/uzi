package workersvc

import (
	"reflect"
	"sort"
	"testing"
)

// TestJudgeSkipFailOrigins pins the origin sets the Quality analytics denominator binds: the
// literal lists below are the expectation, and the source maps are cross-checked so a member
// added to one of them cannot silently stay out of the analytics population.
func TestJudgeSkipFailOrigins(t *testing.T) {
	always, prestart := JudgeSkipFailOrigins()

	wantAlways := []string{
		"base_diverged", "data_volume_full", "finalize_base_align_conflict", "forge_unreachable",
		"gate_presentation_refused", "push_secret_blocked", "skills_plugin_load_failed",
		"task_undispatched", "worker_residue_blocked", "workflow_scope_missing",
	}
	wantPrestart := []string{"credential_unavailable", "guardrail_blocked", "provisioning_failed"}
	if !reflect.DeepEqual(always, wantAlways) {
		t.Errorf("always = %q, want %q", always, wantAlways)
	}
	if !reflect.DeepEqual(prestart, wantPrestart) {
		t.Errorf("prestartOnly = %q, want %q", prestart, wantPrestart)
	}

	fromMaps := map[string]bool{}
	for o := range neverJudgeFailOrigins {
		fromMaps[o] = true
	}
	for o := range envPublishFailOrigins {
		fromMaps[o] = true
	}
	var keys []string
	for o := range fromMaps {
		keys = append(keys, o)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(always, keys) {
		t.Errorf("always = %q, want the sorted union of the source maps %q", always, keys)
	}
	var pre []string
	for o := range preStartInfraFailOrigins {
		pre = append(pre, o)
	}
	sort.Strings(pre)
	if !reflect.DeepEqual(prestart, pre) {
		t.Errorf("prestartOnly = %q, want the sorted preStartInfraFailOrigins %q", prestart, pre)
	}

	// Fresh slices: a caller's append must not alias the next call's result.
	always[0] = "mutated"
	again, _ := JudgeSkipFailOrigins()
	if again[0] == "mutated" {
		t.Error("JudgeSkipFailOrigins returned an aliased slice")
	}
}
