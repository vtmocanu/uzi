package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// PRD #4574 M1: every claim, placement, demand, wake and reap site carries the Codex judge clause
// inside its Codex provider arm, each in its own operand form. The forms are read from the
// generated SQL constants (what the database receives), so a regenerated or hand-edited query that
// drops or reshapes one is named individually. No database is needed.
func TestCodexProviderJudgeCapabilityClauseFormsPinned(t *testing.T) {
	read := func(file string) string {
		b, err := os.ReadFile(file) //nolint:gosec // G304: a fixed generated-file name in this package directory, read-only.
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// constant extracts one generated query's SQL text.
	constant := func(src, name string) string {
		start := strings.Index(src, "const "+name+" = `-- name: ")
		if start < 0 {
			t.Fatalf("generated constant %s not found", name)
		}
		body := src[start:]
		end := strings.Index(body[len("const "):], "\n`")
		if end < 0 {
			t.Fatalf("generated constant %s is unterminated", name)
		}
		return body[:end+len("const ")]
	}
	// squash collapses every whitespace run, so a multi-line arm is compared by its tokens.
	squash := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	runtime, hosted, hibernation := read("runtime.sql.go"), read("hosted_workers.sql.go"), read("hibernation.sql.go")

	// Positional parameters renumber with every sqlc run; the claimant's array must be the same
	// parameter in all three operands of the clause.
	claimRe := regexp.MustCompile(`\(r\.harness = 'codex' AND 'codex_provider_v1' = ANY\((\$\d+::text\[\])\) AND \(r\.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY\((\$\d+::text\[\])\)\)\)`)
	m := claimRe.FindStringSubmatch(constant(runtime, "claimRun"))
	if m == nil || m[1] != m[2] {
		t.Errorf("ClaimRun claimant arm lacks the Codex judge clause over one parameter: %v", m)
	}

	for _, tc := range []struct {
		query, src, form string
		want             int
		squashed         bool
	}{
		{"claimRun", runtime, "(r.harness = 'codex' AND 'codex_provider_v1' = ANY(p.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(p.protocol_capabilities)))", 1, false},
		{"countOnlineWorkersClaimableForRun", runtime, "(run.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities) AND (run.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities)))", 1, false},
		{"listSaturationQueuedRunsForEphemeral", runtime, "(r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities)))", 2, false},
		{"listStaleHibernationWakes", hibernation, "WHEN 'codex' THEN 'codex_provider_v1' = ANY(w.protocol_capabilities) AND 'codex_harness_v1' = ANY(w.protocol_capabilities) AND 'codex_runtime_v2' = ANY(w.protocol_capabilities) AND 'codex_runtime_v3' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities))", 1, true},
		{"lockReapableEphemeralWorkers", hosted, "(r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities) AND 'codex_harness_v1' = ANY(w.protocol_capabilities) AND 'codex_runtime_v2' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities)))", 1, true},
		{"deleteLockedEphemeralWorkers", hosted, "(r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities) AND 'codex_harness_v1' = ANY(w.protocol_capabilities) AND 'codex_runtime_v2' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities)))", 1, true},
		// The health count takes the run kind as its own parameter ($3 after user and engine).
		{"countOnlineWorkersSatisfyingProvider", runtime, "'codex_runtime_v3' = ANY(w.protocol_capabilities) AND ($3::text <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities)))", 1, true},
	} {
		sql := constant(tc.src, tc.query)
		if tc.squashed {
			sql = squash(sql)
		}
		if got := strings.Count(sql, tc.form); got != tc.want {
			t.Errorf("%s carries the Codex judge-clause form %q %d times, want %d", tc.query, tc.form, got, tc.want)
		}
	}

	// No Codex provider arm anywhere in these queries may stay without the clause.
	for name, sql := range map[string]string{
		"claimRun": constant(runtime, "claimRun"), "countOnlineWorkersClaimableForRun": constant(runtime, "countOnlineWorkersClaimableForRun"),
		"listSaturationQueuedRunsForEphemeral": constant(runtime, "listSaturationQueuedRunsForEphemeral"),
		"lockReapableEphemeralWorkers":         constant(hosted, "lockReapableEphemeralWorkers"), "deleteLockedEphemeralWorkers": constant(hosted, "deleteLockedEphemeralWorkers"),
		"listStaleHibernationWakes": constant(hibernation, "listStaleHibernationWakes"),
	} {
		arms := strings.Count(sql, "'codex_provider_v1' = ANY(")
		clauses := strings.Count(sql, "'codex_provider_judge_v1' = ANY(")
		if arms != clauses {
			t.Errorf("%s has %d Codex provider arms but %d judge clauses", name, arms, clauses)
		}
	}
}
