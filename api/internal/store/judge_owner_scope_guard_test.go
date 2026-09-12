package store_test

import (
	"os"
	"strings"
	"testing"
)

// PRD #1184 headline-risk tripwire (Risks §"The relaxed predicate leaks into the owner path").
//
// The admin "All users" judge aggregate deliberately ships SEPARATE all-users queries with NO
// user parameter at all, rather than relaxing the owner query behind a nullable user_id sentinel.
// This test is the standing guard against a future refactor that merges the two: it asserts, at
// the SQL-text level, that the owner-scoped judge queries still carry `rv.user_id = @user_id` and
// that the all-users queries carry no `@user_id` reference whatsoever.
//
// It reads the .sql source rather than the generated *.sql.go so it pins the authored query, and
// it operates on the exact query block (delimited by `-- name:` markers) so an owner predicate
// living in a NEIGHBOURING query can never make an all-users query look scoped, or vice versa.
// The owner live-DB tests seed a single user, so a dropped owner predicate would still pass them
// (the PRD says so); this text-level assertion is what actually guards the boundary.
func TestJudgeOwnerQueriesStayOwnerScoped(t *testing.T) {
	ownerScoped := []struct {
		file, query string
	}{
		{"queries/judge_recommendations.sql", "ListJudgeRecommendationRowsForUser"},
		{"queries/dispositions.sql", "ListJudgeTriageRowsForUser"},
	}
	for _, oc := range ownerScoped {
		block := stripSQLComments(readQueryBlock(t, oc.file, oc.query))
		if !strings.Contains(block, "rv.user_id = @user_id") {
			t.Errorf("%s (%s) no longer contains the owner predicate `rv.user_id = @user_id` — "+
				"the owner scope was dropped or the query was merged with an all-users variant. "+
				"Owner-scoped judge queries MUST keep this predicate (PRD #1184 Risks).", oc.query, oc.file)
		}
	}

	allUsers := []struct {
		file, query string
	}{
		{"queries/judge_recommendations.sql", "ListJudgeRecommendationRowsAll"},
		{"queries/dispositions.sql", "ListJudgeTriageRowsAll"},
	}
	for _, au := range allUsers {
		block := stripSQLComments(readQueryBlock(t, au.file, au.query))
		if strings.Contains(block, "@user_id") {
			t.Errorf("%s (%s) references `@user_id` — the admin all-users query MUST have no user "+
				"parameter at all (not a nullable sentinel), so the owner path can never be relaxed "+
				"into it (PRD #1184 Risks + ADR 1184).", au.query, au.file)
		}
	}
}

// readQueryBlock returns the text of the `-- name: <query>` block in a queries/*.sql file, from
// its `-- name:` marker up to (but not including) the next `-- name:` marker or EOF.
func readQueryBlock(t *testing.T, file, query string) string {
	t.Helper()
	src, err := os.ReadFile(file) //nolint:gosec // G304: reads a fixed in-repo queries/*.sql source path from this test's own literal table, never user input
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	marker := "-- name: " + query + " "
	start := strings.Index(string(src), marker)
	if start < 0 {
		t.Fatalf("query %s not found in %s — renamed or moved? Update this guard.", query, file)
	}
	rest := string(src)[start+len(marker):]
	if end := strings.Index(rest, "-- name: "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// stripSQLComments removes `--` line comments (from the marker to end of line) so the predicate
// checks see only executable SQL — the `*All` query's own header comment explains that it drops
// `rv.user_id = @user_id`, and that prose must not be mistaken for a real binding (nor an owner
// query's comment be mistaken for its predicate).
func stripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
