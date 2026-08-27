package store_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestOnlyOneQueryInsertsRevisePlanRows guards the invariant the issue #106 fix depends
// on: runs.revise_count is the cap's source of truth, and it stays equal to the number of
// revise_plan rows only because exactly ONE query creates such a row, and that query bumps
// the counter in the same statement. A second inserting query would create revise_plan
// rows the counter never sees, and the cap would leak silently — no test would fail, the
// rows would simply outnumber the budget.
//
// It needs no database, deliberately: it is a structural fact about the query files, so it
// belongs in the ordinary `go test ./...` gate where every developer runs it, not in the
// live-DB sweep. The precedent for a test that parses SQL out of the source tree is
// TestRateLimitTypeVocabularyMatchesCheck, which reads a migration file the same way.
//
// 🔴 WHAT THIS DOES NOT COVER, because a guard whose limits are unstated is how #106's
// docs went wrong in the first place. This sees SQL, not Go. It cannot see a second writer
// that reuses the existing generic CreateRunInput query with kind = "revise_plan", since
// that adds no literal to any .sql file — measured 2026-07-29 by adding exactly such a
// writer, against which the entire `go test -count=1 ./...` gate stayed green (43 packages
// ok, 0 FAIL) and this test passes too. The layers that DO exist, and the only ones that
// may be cited as coverage:
//
//   - workersvc.TestSubmitInputRevisePlanEnqueuesPlain asserts SubmitInput's revise_plan
//     branch takes the capped path and NOT the uncapped CreateRunInput one. Measured
//     catching a second writer added inside that branch.
//   - this test, for a newly added SQL writer.
//   - nothing at all for a writer added elsewhere in Go. That is a real hole, recorded
//     rather than papered over.
func TestOnlyOneQueryInsertsRevisePlanRows(t *testing.T) {
	const queryDir = "queries"
	entries, err := os.ReadDir(queryDir)
	if err != nil {
		t.Fatalf("read %s: %v", queryDir, err)
	}

	var scanned int
	var inserters []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(queryDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		// sqlc delimits named queries with a `-- name: Foo :one` header, so splitting on
		// that marker yields one block per query.
		for _, block := range strings.Split(string(raw), "-- name: ")[1:] {
			scanned++
			name, _, _ := strings.Cut(block, " ")
			// Strip comment lines before matching: this file's own query comments discuss
			// revise_plan in prose, and a guard that counted prose would be satisfiable by
			// editing a sentence.
			var sql strings.Builder
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "--") {
					continue
				}
				sql.WriteString(line)
				sql.WriteString("\n")
			}
			body := sql.String()
			if strings.Contains(body, "INSERT INTO run_user_inputs") && strings.Contains(body, "'revise_plan'") {
				inserters = append(inserters, name)
			}
		}
	}

	// A scan that found no queries at all would report "exactly one inserter" vacuously if
	// the directory moved or the delimiter changed, so pin that the scan actually ran.
	if scanned < 2 {
		t.Fatalf("scanned %d named queries in %s/ — the scan did not run, this result is meaningless", scanned, queryDir)
	}

	sort.Strings(inserters)
	want := []string{"CreateRunReviseInputIfUnderCap"}
	if len(inserters) != 1 || inserters[0] != want[0] {
		t.Fatalf("queries inserting a 'revise_plan' row = %v, want exactly %v.\n"+
			"A second inserting query creates revise_plan rows that runs.revise_count never "+
			"counts, so the cap silently leaks. If this is deliberate, the new query must bump "+
			"runs.revise_count in the same statement — see CreateRunReviseInputIfUnderCap.",
			inserters, want)
	}
}
