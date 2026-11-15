package store

import (
	"strings"
	"testing"
)

// TestResumePausedRunFreshExceptionPredicateAtBothSites pins the structure of ResumePausedRun's
// pending-Fresh refusal (PRD #3250 M2). The same predicate must guard both the `candidates` CTE and
// the UPDATE's WHERE. The behavioural controls in workersvc cannot tell the two sites apart: weaken
// only one and the other still refuses, so those controls stay green. This test reads the generated
// query text (whitespace collapsed) and requires the exact predicate exactly twice, once in each site.
func TestResumePausedRunFreshExceptionPredicateAtBothSites(t *testing.T) {
	const predicate = "AND NOT (runs.hold_reason IS NOT DISTINCT FROM 'codex_policy_hold' " +
		"OR (runs.policy_pending_fresh " +
		"AND NOT (runs.hold_reason IS NOT DISTINCT FROM 'run_override_unavailable' " +
		"AND (runs.policy_outcome_id IS NOT NULL AND runs.policy_resolved_at IS NOT NULL))) " +
		"OR (runs.policy_outcome_id IS NOT NULL AND runs.policy_resolved_at IS NULL))"

	q := strings.Join(strings.Fields(resumePausedRun), " ")
	if got := strings.Count(q, predicate); got != 2 {
		t.Fatalf("predicate occurs %d times in ResumePausedRun, want exactly 2 (CTE and UPDATE)", got)
	}

	cte := strings.Index(q, "WITH candidates AS MATERIALIZED (")
	update := strings.Index(q, "WHERE runs.id = $6 AND runs.user_id = $7 AND runs.status = 'paused'")
	if cte < 0 || update < 0 || update < cte {
		t.Fatalf("could not locate the candidates CTE (%d) and the UPDATE WHERE (%d)", cte, update)
	}
	first := strings.Index(q, predicate)
	if first < cte || first > strings.Index(q, "), parent_mapping AS MATERIALIZED (") {
		t.Fatalf("first occurrence (%d) is not inside the candidates CTE", first)
	}
	last := strings.LastIndex(q, predicate)
	if last < update {
		t.Fatalf("second occurrence (%d) is not after the UPDATE's WHERE clause (%d)", last, update)
	}
}
