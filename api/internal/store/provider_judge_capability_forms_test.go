package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// PRD #4322 M1a: every claim, placement and reap site carries the Claude judge clause inside its
// Claude provider arm, each in its own operand form. The forms are read from the generated SQL
// constants (what the database receives), so a regenerated or hand-edited query that drops or
// reshapes one is named individually. No database is needed.
func TestProviderJudgeCapabilityClauseFormsPinned(t *testing.T) {
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
	runtime, hosted, hibernation := read("runtime.sql.go"), read("hosted_workers.sql.go"), read("hibernation.sql.go")

	// Positional parameters renumber with every sqlc run; the claimant's array must be the same
	// parameter in both operands of the clause.
	claimRe := regexp.MustCompile(`\(r\.harness = 'claude' AND 'claude_provider_v1' = ANY\((\$\d+::text\[\])\) AND \(r\.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY\((\$\d+::text\[\])\)\)\)`)
	m := claimRe.FindStringSubmatch(constant(runtime, "claimRun"))
	if m == nil || m[1] != m[2] {
		t.Errorf("ClaimRun claimant arm lacks the judge clause over one parameter: %v", m)
	}

	for _, tc := range []struct {
		query, src, form string
		want             int
	}{
		{"claimRun", runtime, "(r.harness = 'claude' AND 'claude_provider_v1' = ANY(p.protocol_capabilities) AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(p.protocol_capabilities)))", 1},
		{"countOnlineWorkersClaimableForRun", runtime, "(run.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities) AND (run.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities)))", 1},
		{"listSaturationQueuedRunsForEphemeral", runtime, "(r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities)))", 2},
		{"listStaleHibernationWakes", hibernation, "WHEN 'claude' THEN 'claude_provider_v1' = ANY(w.protocol_capabilities)\n                        AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities))", 1},
		{"lockReapableEphemeralWorkers", hosted, "(r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities)))", 1},
		{"deleteLockedEphemeralWorkers", hosted, "(r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities)))", 1},
	} {
		if got := strings.Count(constant(tc.src, tc.query), tc.form); got != tc.want {
			t.Errorf("%s carries the judge-clause form %q %d times, want %d", tc.query, tc.form, got, tc.want)
		}
	}

	// The health count takes the run kind as its own parameter ($3 after user and engine).
	count := constant(runtime, "countOnlineWorkersSatisfyingProvider")
	// PRD #4322 M1b appends the job clause on the same kind parameter.
	if form := "($2::text = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities) AND ($3::text <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities)) AND ($3::text <> 'job' OR 'claude_provider_job_v1' = ANY(w.protocol_capabilities)))"; !strings.Contains(count, form) {
		t.Errorf("CountOnlineWorkersSatisfyingProvider lacks %q", form)
	}

	// No Claude provider arm anywhere in these queries may stay without the clause.
	for name, sql := range map[string]string{
		"claimRun": constant(runtime, "claimRun"), "countOnlineWorkersClaimableForRun": constant(runtime, "countOnlineWorkersClaimableForRun"),
		"listSaturationQueuedRunsForEphemeral": constant(runtime, "listSaturationQueuedRunsForEphemeral"),
		"lockReapableEphemeralWorkers":         constant(hosted, "lockReapableEphemeralWorkers"), "deleteLockedEphemeralWorkers": constant(hosted, "deleteLockedEphemeralWorkers"),
	} {
		arms := strings.Count(sql, "'claude' AND 'claude_provider_v1' = ANY(")
		clauses := strings.Count(sql, "'claude_provider_judge_v1' = ANY(")
		if arms != clauses {
			t.Errorf("%s has %d Claude provider arms but %d judge clauses", name, arms, clauses)
		}
	}
}
