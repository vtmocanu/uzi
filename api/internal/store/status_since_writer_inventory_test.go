package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryStatusWriteStampsStatusSince guards the invariant that issue #190's
// runs.status_since column depends on: status_since is the "when did this run enter its
// current status" clock the health pass reads, and it stays truthful ONLY because every
// statement that ASSIGNS runs.status stamps status_since in the SAME SET clause. A writer
// that transitions runs.status but forgets `status_since = now()` fails SILENT — no query
// errors, no test reddens, the health flag simply never fires for that transition because
// the clock still reads the previous episode's start. That silent-failure shape is exactly
// what a structural guard is for, so this test enumerates the query files and enforces the
// pairing directly, in the ordinary DB-less `go test ./...` gate.
//
// It sees SQL, not Go, and it deliberately reads the same way its sibling
// revise_writer_inventory_test.go does: split each queries/*.sql file on the sqlc
// `-- name: ` header, strip `--` comment lines so prose (this repo's queries discuss
// status_since at length) can never satisfy the matcher, then match on the pure-SQL body.
//
// # WHAT THIS DOES NOT COVER
//
// It matches the literal `UPDATE runs SET` in the .sql files, so it sees CTE-embedded
// writers (ClaimRun, ClaimChatRun) whose UPDATE is present as a literal. It does NOT see a
// Go caller that transitions status by some other means, nor a status write in a migration
// data-fix — those are out of the query-file corpus by construction. The pairing it does
// enforce is the one that the 23 shipping status writers all satisfy today.
func TestEveryStatusWriteStampsStatusSince(t *testing.T) {
	const queryDir = "queries"
	entries, err := os.ReadDir(queryDir)
	if err != nil {
		t.Fatalf("read %s: %v", queryDir, err)
	}

	// Top-level SET-list boundary matchers. A column counts as ASSIGNED only when it is
	// immediately preceded (ignoring whitespace/newlines) by the SET keyword or a comma —
	// i.e. it is the LHS of a top-level SET item. This rejects a `CASE WHEN status =
	// 'running'` right-hand-side reference (preceded by WHEN) and any WHERE-clause `status
	// = '...'` predicate. The trailing `\s*=` plus the fact that `status_since`'s `_` keeps
	// it out of `status\b` means the status matcher never fires on a status_since item.
	assignsStatus := regexp.MustCompile(`(?:\bSET\b|,)\s*status\s*=`)
	assignsStatusSince := regexp.MustCompile(`(?:\bSET\b|,)\s*status_since\s*=`)
	// Whitespace-tolerant so a statement written across lines (`UPDATE runs\nSET …`, as
	// SetRunAnthropicSecret is) is NOT silently skipped by a literal "UPDATE runs SET"
	// scan. A literal marker would let a future status writer formatted that way escape
	// the guard entirely — the vacuity guard below keeps the count at 20+ while silently
	// stopping coverage of it (CodeRabbit #657). \s+ (not \s*) keeps the keywords distinct.
	markerRe := regexp.MustCompile(`UPDATE\s+runs\s+SET`)

	var scanned int            // total named queries seen
	var statusWriters []string // names whose SET clause assigns runs.status
	var violations []string    // status writers that fail to also stamp status_since
	var converseViol []string  // status_since writers that do NOT also assign runs.status

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(queryDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		// sqlc delimits named queries with a `-- name: Foo :kind` header, so splitting on
		// that marker yields one block per named query.
		for _, block := range strings.Split(string(raw), "-- name: ")[1:] {
			scanned++
			name, _, _ := strings.Cut(block, " ")

			// Strip comment lines before matching: the query bodies discuss status_since in
			// prose, and a guard that matched prose would be satisfiable by editing a comment.
			var sb strings.Builder
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "--") {
					continue
				}
				sb.WriteString(line)
				sb.WriteString("\n")
			}
			body := sb.String()

			// A single block may hold more than one `UPDATE runs SET` (a CTE with several
			// stages), so walk every occurrence. For each, the SET clause is the text from
			// the SET keyword up to the first following WHERE, which closes the SET list.
			// A single block may hold more than one `UPDATE runs SET` (a CTE with several
			// stages), so walk every match. Each match ends exactly at the "SET" keyword, so
			// the extracted clause begins at SET (match end minus len("SET")) and the boundary
			// matchers can anchor on it.
			flaggedStatusHere := false
			for _, loc := range markerRe.FindAllStringIndex(body, -1) {
				clauseStart := loc[1] - len("SET")
				// End the SET clause at the first TOP-LEVEL (paren-depth-0) WHERE after it. A
				// plain first-substring cut would truncate on a WHERE nested inside an earlier
				// SET item's subquery (e.g. `col = (SELECT … WHERE …), status = 'failed'`),
				// dropping `status` out of the extracted clause — the block would then be
				// neither counted nor checked, the exact silent miss this guard exists to
				// prevent. If there is no top-level WHERE, the clause runs to end of body.
				rest := body[clauseStart:]
				end := clauseStart + topLevelWhereOffset(rest)
				clause := body[clauseStart:end]

				if assignsStatus.MatchString(clause) {
					if !flaggedStatusHere {
						statusWriters = append(statusWriters, name)
						flaggedStatusHere = true
					}
					if !assignsStatusSince.MatchString(clause) {
						violations = append(violations, name)
					}
				} else if assignsStatusSince.MatchString(clause) {
					// The converse invariant, which the no-flapping guarantee rests on:
					// status_since must move ONLY when status does. A statement that stamps
					// status_since without transitioning status would re-open the approval-idle
					// flap issue #190 closed — so flag it too.
					converseViol = append(converseViol, name)
				}
			}
		}
	}

	// Vacuity guards, mirroring revise_writer_inventory_test.go's `scanned < 2`: a matcher
	// that silently finds nothing must fail loudly rather than pass an empty invariant.
	if scanned == 0 {
		t.Fatalf("scanned %d named queries in %s/ — the scan did not run, this result is meaningless", scanned, queryDir)
	}
	if len(statusWriters) < 20 {
		t.Fatalf("found only %d statements that assign runs.status (%v) — expected at least 20 (there are 23); "+
			"the matcher is not seeing the status writers, so its green would be vacuous", len(statusWriters), statusWriters)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("these queries assign runs.status but do NOT stamp status_since in the same SET clause: %v.\n"+
			"Every statement that transitions runs.status must also set `status_since = now()` — see the "+
			"00163_run_status_since.sql migration. A forgotten stamp fails silent: the health pass reads a "+
			"status clock that never advanced, so the approval/queued health flag never fires for that transition.",
			violations)
	}

	if len(converseViol) > 0 {
		sort.Strings(converseViol)
		t.Fatalf("these queries assign status_since but do NOT assign runs.status in the same SET clause: %v.\n"+
			"status_since must move ONLY on a real status transition — that is what makes the episode clock "+
			"monotone within a gate episode (issue #190). A statement that stamps status_since without changing "+
			"status would re-open the approval-idle flap the fix closed.",
			converseViol)
	}
}

// topLevelWhereOffset returns the byte offset of the first `WHERE` keyword in s that sits
// at parenthesis depth 0 (i.e. the statement's own WHERE, not one nested inside a SET-item
// subquery), or len(s) if there is none. It tracks paren depth and requires the keyword to
// be delimited by a non-identifier byte on each side, so a substring like `somewhere` never
// matches. Case-sensitive: the query files write SQL keywords upper-case.
func topLevelWhereOffset(s string) int {
	const kw = "WHERE"
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth != 0 || i+len(kw) > len(s) || s[i:i+len(kw)] != kw {
			continue
		}
		if i > 0 && isIdentByte(s[i-1]) {
			continue
		}
		if j := i + len(kw); j < len(s) && isIdentByte(s[j]) {
			continue
		}
		return i
	}
	return len(s)
}

// isIdentByte reports whether b can appear inside a SQL identifier/keyword, used to keep
// topLevelWhereOffset from matching `WHERE` embedded in a longer word.
func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
