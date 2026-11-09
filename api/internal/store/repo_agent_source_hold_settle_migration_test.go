package store_test

import (
	"strings"
	"testing"
)

const repoAgentSourceSettleMigrationFile = "00409_repo_agent_source_hold_settle.sql"

func TestRepoAgentSourceHoldSettleMigrationChangesOnlyTheSourceHoldClause(t *testing.T) {
	preceding := closureGuardBlock(t, strings.Split(closureMigrationText(t, closureReleaseMigrationFile), "-- +goose Down\n")[0])
	parts := strings.Split(closureMigrationText(t, repoAgentSourceSettleMigrationFile), "-- +goose Down\n")
	if len(parts) != 2 {
		t.Fatal("migration must have exactly one Down section")
	}
	if got := closureGuardBlock(t, parts[1]); got != preceding {
		t.Fatal("Down does not restore the 00393 guard body")
	}
	const clause = "\n                   OR (r.status = 'paused' AND r.hold_reason = 'repo_agent_source_unavailable'\n" +
		"                       AND r.repo_agent_source_commit IS NULL\n" +
		"                       AND NEW.final_disposition = 'settled' AND NEW.release_evidence = 'forge_no_output')"
	up := closureGuardBlock(t, parts[0])
	if strings.Count(up, clause) != 1 {
		t.Fatal("the source-hold settle clause is missing or repeated")
	}
	const forgeClause = "AND NEW.final_disposition = 'settled' AND NEW.release_evidence = 'forge_no_output')"
	restored := strings.Replace(up, forgeClause+clause+"))))", forgeClause+"))))", 1)
	if restored != preceding {
		t.Fatal("the guard changed outside the source-hold settle clause")
	}
}
