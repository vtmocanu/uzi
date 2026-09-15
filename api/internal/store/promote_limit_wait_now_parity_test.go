package store_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestPromoteLimitWaitNowMirrorsSweepPass is the column-parity guard PRD #1247 M4 (D4)
// requires: the single-row early promote `PromoteLimitWaitRunNow` (the `uzi run set-token`
// verb) MUST mutate exactly the same runs columns as the sweeper's `PromoteLimitWaitRuns`
// pass — no more, no fewer — so an owner-driven early promote and the automatic promote
// leave a run in byte-identically the same shape (fresh wall, cleared pause bank, revoked
// codex cap, reset health). If someone adds a column to one promote and forgets the other,
// the two resume paths silently diverge; this DB-less test in the ordinary `go test ./...`
// gate catches that.
//
// It ALSO pins two intents by name:
//   - the mutated set is exactly the 10 columns listed below (naming every one, so a
//     reviewer sees the whole set and a drift in EITHER query reddens here);
//   - the load-bearing PRESERVED columns (worker_id, session_id, last_seq, the limit
//     fields, limit_dead_secret_id, retry_not_before and both retry counters) appear in
//     NEITHER SET clause — they are left in place so claimExclude keeps excluding the
//     still-dead token while its window is closed and worker affinity survives.
//
// It reads the .sql query source (not the generated Go const) the same way
// status_since_writer_inventory_test.go does: split on the sqlc `-- name: ` header, strip
// `--` comment lines so the queries' long prose can never satisfy the matcher, then match
// on the pure-SQL body.
func TestPromoteLimitWaitNowMirrorsSweepPass(t *testing.T) {
	const queryFile = "queries/runtime.sql"
	raw, err := os.ReadFile(filepath.FromSlash(queryFile))
	if err != nil {
		t.Fatalf("read %s: %v", queryFile, err)
	}

	sweep := setClauseColumns(t, string(raw), "PromoteLimitWaitRuns")
	now := setClauseColumns(t, string(raw), "PromoteLimitWaitRunNow")

	// The exact mutated set both must carry (D4's table names each of these). Naming them
	// here makes the reviewer's job a diff against this list, and a column added to only one
	// query fails against BOTH this expectation AND the sweep==now equality below.
	want := []string{
		"budget_paused_seconds",
		"codex_cap_hash",
		"codex_claim_epoch",
		"health",
		"health_reason",
		"health_since",
		"started_at",
		"status",
		"status_since",
		"updated_at",
	}

	if !reflect.DeepEqual(now, want) {
		t.Fatalf("PromoteLimitWaitRunNow mutates %v, want exactly %v", now, want)
	}
	if !reflect.DeepEqual(sweep, want) {
		t.Fatalf("PromoteLimitWaitRuns mutates %v, want exactly %v — the early promote pins itself to this set, so the sweep pass drifting reddens here too", sweep, want)
	}
	if !reflect.DeepEqual(sweep, now) {
		t.Fatalf("column-parity broken: PromoteLimitWaitRuns mutates %v but PromoteLimitWaitRunNow mutates %v — the two resume paths must stay identical", sweep, now)
	}

	// The load-bearing PRESERVED columns must appear in NEITHER SET clause. If any of these
	// creeps into the early promote, claimExclude stops excluding the dead token (its window
	// gauge or retry stamp got reset) or worker affinity/session resume is lost.
	preserved := []string{
		"worker_id", "session_id", "last_seq",
		"limit_wait_count", "limit_resets_at", "retry_not_before", "rate_limit_type",
		"limit_dead_secret_id", "requeue_count",
	}
	mutated := map[string]bool{}
	for _, c := range now {
		mutated[c] = true
	}
	for _, c := range preserved {
		if mutated[c] {
			t.Errorf("PromoteLimitWaitRunNow mutates %q, but it MUST be preserved (early promote must leave the limit window / affinity intact)", c)
		}
	}
}

// setClauseColumns extracts the sorted, de-duplicated set of columns assigned in the SET
// clause of the named query in the runtime.sql source. It fails the test loudly if the
// query is not found or its SET clause parses empty (a vacuous match must never pass).
func setClauseColumns(t *testing.T, sql, name string) []string {
	t.Helper()
	// sqlc delimits named queries with `-- name: Foo :kind`; split and find ours.
	var block string
	found := false
	for _, b := range strings.Split(sql, "-- name: ")[1:] {
		n, _, _ := strings.Cut(b, " ")
		if n == name {
			block = b
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("query %q not found in runtime.sql — the parser is looking at the wrong source, this result is meaningless", name)
	}

	// Strip comment lines before matching so the queries' prose (which discusses these
	// columns at length) can never satisfy the matcher.
	var sb strings.Builder
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	body := sb.String()

	// Extract the SET clause: from the SET keyword to the first following WHERE (both
	// promotes have a top-level WHERE and no subquery, so a plain first-WHERE cut is safe
	// here). setStart is the byte after "SET".
	up := strings.Index(body, "UPDATE runs SET")
	if up < 0 {
		t.Fatalf("query %q has no `UPDATE runs SET` — parser cannot find the SET clause", name)
	}
	setStart := up + len("UPDATE runs SET")
	rest := body[setStart:]
	if w := strings.Index(rest, "WHERE"); w >= 0 {
		rest = rest[:w]
	}

	// A column is ASSIGNED when it is the LHS of a top-level SET item — immediately
	// preceded (ignoring whitespace/newlines) by the SET boundary (start of the extracted
	// clause) or a comma. The leading `(?:^|,)` keeps a RHS reference like
	// `codex_claim_epoch = codex_claim_epoch + 1` from double-counting (the RHS token is
	// preceded by `=`, not by `,`).
	assign := regexp.MustCompile(`(?:^|,)\s*([a-z_]+)\s*=`)
	seen := map[string]bool{}
	var cols []string
	for _, m := range assign.FindAllStringSubmatch(rest, -1) {
		col := m[1]
		if !seen[col] {
			seen[col] = true
			cols = append(cols, col)
		}
	}
	if len(cols) == 0 {
		t.Fatalf("query %q: extracted an EMPTY SET-clause column set — the matcher is broken, a green here would be vacuous", name)
	}
	sort.Strings(cols)
	return cols
}
