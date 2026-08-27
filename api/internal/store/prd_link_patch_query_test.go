package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestPRDLinkPatchCandidatesReadsNoIssueState enforces Decision 10: the PRD-link
// patch candidate query must NOT join `issues` and must NOT filter on issue state.
//
// The mechanism, because the conclusion alone is not enough to keep this right: a
// merge CLOSES the issue via the `Closes #N` in the MR description
// (agent/src/runner.ts), and the poller runs the issue sync BEFORE SyncMRStates
// (poller.syncRepo). So by the time a merge is observable the issue is already
// `state='closed'`. Any `i.state = 'opened'` predicate would miss every candidate
// DETERMINISTICALLY, not just sometimes. ListMRWatchCandidates now ALSO has a
// closed-issue recording lane (#527), but it still keeps its open-issue predicate
// for the board-move lane and its closed lane only RECORDS mr_state — so the reason
// ListPRDLinkPatchCandidates joins no `issues` and filters no state is unchanged:
// it does a forge description WRITE, not just an mr_state record, which is why PRD
// #24's prefilter is not reused here.
//
// WHY THIS EXISTS AS A SOURCE-TEXT TEST RATHER THAN RELYING ON THE FIXTURE. The
// property IS pinned by prd_link_patch_integration_test.go, but only INCIDENTALLY:
// that fixture inserts no `issues` rows at all, so any join returns zero rows and
// the forbidden predicate reddens it. Anyone who later adds `issues` rows to make
// the fixture "more realistic" silently unpins Decision 10 — and the ordinary api
// gate is blind to the fold (exit 0, zero FAILs), so only the live sweep would
// catch it. This test needs no fixture, no database and no sqlc regen, and it runs
// on the ordinary gate, which is where the incidental pin is blind.
//
// Modelled on TestMRStateIsWatcherOwned in this package, including its sawQuery
// guard: an ABSENCE assertion passes vacuously if the query is renamed or moved,
// so the test must first prove it found the thing it is asserting about.
func TestPRDLinkPatchCandidatesReadsNoIssueState(t *testing.T) {
	const queryName = "ListPRDLinkPatchCandidates"
	src, err := os.ReadFile("queries/forge.sql")
	if err != nil {
		t.Fatalf("read queries/forge.sql: %v", err)
	}
	body, ok := splitNamedQueries(string(src))[queryName]
	if !ok {
		t.Fatalf("%s not found in queries/forge.sql — renamed or moved? "+
			"Until it is found again this test asserts nothing.", queryName)
	}

	// Strip comments: the query's own prose explains at length why it does NOT
	// join issues, and matching that text would make this permanently red.
	var sql strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		sql.WriteString(line)
		sql.WriteString("\n")
	}
	stmt := sql.String()

	if regexp.MustCompile(`(?is)\bjoin\s+issues\b`).MatchString(stmt) {
		t.Errorf("%s joins `issues`. Decision 10: a merge closes the issue before the merge is "+
			"observable, so any issue-state predicate misses every candidate deterministically.", queryName)
	}
	if regexp.MustCompile(`(?is)\bi\.state\b|\bissues\.state\b`).MatchString(stmt) {
		t.Errorf("%s filters on issue state. Decision 10: see above — this is the exact predicate "+
			"ListMRWatchCandidates carries and the reason its prefilter is not reused here.", queryName)
	}
	// Positive control on the strip: if the comment-stripper ever ate the whole
	// body, both assertions above would pass vacuously.
	if !strings.Contains(stmt, "FROM runs r") {
		t.Fatalf("the stripped statement no longer contains `FROM runs r`; the assertions above "+
			"would pass against an empty string. Stripped body:\n%s", stmt)
	}
}
