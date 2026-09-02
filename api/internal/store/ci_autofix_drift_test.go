package store

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
)

// TestCIAutofixCandidateQueryMirrorsFailedStatuses is the automated, DB-free drift
// guard between pipelinestatus.failedStatuses (via FailedStatuses()) and the
// `ps.status IN (...)` predicate of ListCIAutofixCandidateRefs. The two were only
// kept in sync by a comment; issue #1005 was exactly a failure status
// (GitHub's "failure") present in the Go set but missing from the SQL, so a whole
// forge's failed pipelines silently never became auto-fix candidates.
//
// It reads the sqlc-generated query string constant directly (unexported, same
// package) and asserts every FailedStatuses() entry appears as a quoted literal, and
// that two known non-failures ('cancelled', 'success') do NOT appear inside the
// ps.status IN (...) list. Adding a status to failedStatuses without updating the
// SQL reddens this test.
func TestCIAutofixCandidateQueryMirrorsFailedStatuses(t *testing.T) {
	query := listCIAutofixCandidateRefs

	// Positive: every canonical failure status must be a quoted literal in the query.
	for _, status := range pipelinestatus.FailedStatuses() {
		literal := "'" + status + "'"
		if !strings.Contains(query, literal) {
			t.Errorf("ListCIAutofixCandidateRefs is missing failure status %s (expected literal %s in the query); "+
				"add it to the ps.status IN (...) predicate in queries/ci_autofix.sql and regenerate sqlc — "+
				"pipelinestatus.failedStatuses and the SQL predicate MUST stay in sync (issue #1005)", status, literal)
		}
	}

	// Negative boundary: slice out THIS query's ps.status IN ( ... ) clause and assert
	// deliberate non-failures never leak into it. 'cancelled' / 'success' also appear
	// elsewhere in the query (a CTE comment, r.status <> 'cancelled'), so the negative
	// check must be scoped to the IN-list, not the whole string.
	inClause := statusInClause(t, query)
	for _, notFailure := range []string{"cancelled", "success"} {
		literal := "'" + notFailure + "'"
		if strings.Contains(inClause, literal) {
			t.Errorf("ListCIAutofixCandidateRefs ps.status IN (...) predicate must NOT contain %s — it is a deliberate "+
				"non-failure (a human cancel / a pass), not something uzi's ci_fix should act on (see pipelinestatus D8). "+
				"Clause was: %s", literal, inClause)
		}
	}
}

// statusInClause returns just the `ps.status IN ( ... )` list from the query, so the
// negative-boundary check cannot match a status literal that appears elsewhere.
func statusInClause(t *testing.T, query string) string {
	t.Helper()
	const marker = "ps.status IN ("
	start := strings.Index(query, marker)
	if start < 0 {
		t.Fatalf("could not locate %q in ListCIAutofixCandidateRefs — the query shape changed; update this drift guard", marker)
	}
	rest := query[start+len(marker):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("could not find the closing paren of the ps.status IN (...) clause in ListCIAutofixCandidateRefs")
	}
	return rest[:end]
}
