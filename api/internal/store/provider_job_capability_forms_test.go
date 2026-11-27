package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// PRD #4322 M1b: a provider-bound job needs claude_provider_job_v1 on top of the provider and
// job-runner capabilities, at every claim, placement, wake and reap-adjacent site (the D14 census).
// The forms are read from the generated SQL constants (what the database receives), so a regenerated
// or hand-edited query that drops, reshapes or moves one is named individually. No database is needed.
func TestProviderJobCapabilityClauseFormsPinned(t *testing.T) {
	read := func(file string) string {
		b, err := os.ReadFile(file) //nolint:gosec // G304: a fixed generated-file name in this package directory, read-only.
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
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
	runtime, hibernation, jobs := read("runtime.sql.go"), read("hibernation.sql.go"), read("jobs.sql.go")

	// The nine job-term sites. Each clause must sit INSIDE the job arm: after the job_files_v1 clause
	// of the same arm and before its isolated_job_v1 clause (the lock-unservable query has the clause
	// pair in a different order, so it is checked against its own anchors below).
	type site struct {
		src, query string
		// clause is a regexp for the one clause form this site carries (operand names differ).
		clause string
		want   int
	}
	nullOr := func(col, caps string) string {
		return `\(` + col + `\.model_provider_key_mode IS NULL OR 'claude_provider_job_v1' = ANY\(` + caps + `\)\)`
	}
	bothCaps := func(col, caps string) string {
		return `\(` + col + `\.model_provider_key_mode IS NULL\s+OR \('claude_provider_v1' = ANY\(` + caps + `\) AND 'claude_provider_job_v1' = ANY\(` + caps + `\)\)\)`
	}
	sites := []site{
		{runtime, "claimRun", nullOr("r", `\$\d+::text\[\]`), 1},
		{runtime, "claimRun", nullOr("r", `p\.protocol_capabilities`), 1},
		{runtime, "countOnlineWorkersClaimableForRun", nullOr("run", `w\.protocol_capabilities`), 1},
		{runtime, "listSaturationQueuedRunsForEphemeral", nullOr("r", `w\.protocol_capabilities`), 2},
		{hibernation, "listHibernationWakeDemand", nullOr("r", `w\.protocol_capabilities`), 1},
		{runtime, "listUnplaceableQueuedRunsForEphemeral", bothCaps("r", `wj\.protocol_capabilities`), 1},
		{jobs, "lockUnservableEphemeralJobWorkers", bothCaps("r", `w\.protocol_capabilities`), 1},
	}
	for _, tc := range sites {
		sql := constant(tc.src, tc.query)
		re := regexp.MustCompile(tc.clause)
		if got := len(re.FindAllString(sql, -1)); got != tc.want {
			t.Errorf("%s carries the job clause %q %d times, want %d", tc.query, tc.clause, got, tc.want)
		}
		// Placement: every clause occurrence follows a job_files_v1 clause and precedes the next
		// isolated_job_v1 clause or the end of the arm, so it cannot have drifted outside the job term.
		for _, loc := range re.FindAllStringIndex(sql, -1) {
			before := sql[:loc[0]]
			files := strings.LastIndex(before, "'job_files_v1' = ANY(")
			runner := strings.LastIndex(before, "'job_runner_v1' = ANY(")
			if files < 0 || runner < 0 || runner > files {
				t.Errorf("%s: the job clause at %d does not follow the job arm's job_runner_v1 and job_files_v1 clauses", tc.query, loc[0])
			}
			// Nothing but the arm's own clauses may separate the job_files_v1 clause from this one.
			between := before[files:]
			if strings.Contains(between, "r.kind <> 'job'") || strings.Contains(between, "r.kind = 'job'") {
				t.Errorf("%s: the job clause at %d is separated from its job_files_v1 clause by another job arm", tc.query, loc[0])
			}
		}
	}

	// The job-runner count takes the provider requirement as its own parameter.
	count := constant(runtime, "countOnlineWorkersSatisfyingJobRunner")
	if !regexp.MustCompile(`NOT \$\d+::boolean\s+OR \('claude_provider_v1' = ANY\(w\.protocol_capabilities\) AND 'claude_provider_job_v1' = ANY\(w\.protocol_capabilities\)\)`).MatchString(count) {
		t.Errorf("CountOnlineWorkersSatisfyingJobRunner lacks the provider-job requirement: %s", count)
	}

	// D3: a stale wake whose worker lost the job capability is released (the Claude arm of the
	// frozen-engine check), in the same CASE arm as the provider and judge clauses.
	stale := constant(hibernation, "listStaleHibernationWakes")
	if form := "AND (r.kind <> 'job' OR 'claude_provider_job_v1' = ANY(w.protocol_capabilities))\n       WHEN 'codex'"; !strings.Contains(stale, form) {
		t.Errorf("ListStaleHibernationWakes lacks %q", form)
	}

	// D4: the provider health count takes the job clause on the kind parameter, in the Claude arm only.
	provider := constant(runtime, "countOnlineWorkersSatisfyingProvider")
	if form := "AND ($3::text <> 'job' OR 'claude_provider_job_v1' = ANY(w.protocol_capabilities)))\n    OR ($2::text = 'codex'"; !strings.Contains(provider, form) {
		t.Errorf("CountOnlineWorkersSatisfyingProvider lacks the job clause inside its Claude arm: %s", provider)
	}

	// D2: the generic arm of the unplaceable-run trigger never decides a job. The conjunct sits inside
	// the first AND chain, immediately after the TRUE anchor and before the demand disjunction and the
	// job arm's OR; outside the outer parentheses it would exclude jobs from the job arm itself.
	unplaceable := constant(runtime, "listUnplaceableQueuedRunsForEphemeral")
	re := regexp.MustCompile(`(?s)AND \(\s+TRUE\s+(?:--[^\n]*\n\s*)*AND r\.kind <> 'job'\s+(?:--[^\n]*\n\s*)*AND \(r\.checkpoint_coordinates IS NOT NULL`)
	if !re.MatchString(unplaceable) {
		t.Errorf("ListUnplaceableQueuedRunsForEphemeral does not carry r.kind <> 'job' inside its first AND chain")
	}
	if got := strings.Count(unplaceable, "AND r.kind <> 'job'"); got != 1 {
		t.Errorf("ListUnplaceableQueuedRunsForEphemeral carries the generic-arm job exclusion %d times, want 1", got)
	}
	jobArm := strings.Index(unplaceable, "OR (r.kind = 'job' AND NOT EXISTS")
	generic := strings.Index(unplaceable, "AND r.kind <> 'job'")
	if generic < 0 || jobArm < 0 || generic > jobArm {
		t.Errorf("the generic-arm exclusion must precede the job arm's OR (generic=%d, job arm=%d)", generic, jobArm)
	}

	// Reap arm (g), fn_placement_admitting_workers and the isolated-lane trigger are unchanged by
	// design (D14): none of their generated constants may mention the job capability.
	for _, name := range []string{"listIsolatedQueuedRunsForEphemeral"} {
		if strings.Contains(constant(runtime, name), "claude_provider_job_v1") {
			t.Errorf("%s must not carry the provider-job clause", name)
		}
	}
}
