package store_test

import (
	"os"
	"strings"
	"testing"
)

// These lock queries deliberately return final_targets. Their suppression CTE
// does not affect that result; this contract guards SQL eligibility separately
// from the disposition and Resume behavior exercised against PostgreSQL.
func TestCrossCheckRoundsWorkerRecoverySQL(t *testing.T) {
	predicate := "cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage) AND (cc.round = 1 OR (cc.automatic_rounds_enabled AND cc.round <= cc.automatic_revision_limit + 1)) AND cc.verdict = 'pending'"
	for _, tc := range []struct{ file, name, fence string }{
		{"runtime.sql", "ResumeWorkerRecoveryEpisode", "FOR UPDATE OF cc"},
		{"worker_recovery.sql", "LockFrozenFailRunsMissingFromSnapshot", "SELECT id FROM final_targets ORDER BY id;"},
		{"worker_recovery.sql", "LockFrozenFailWorkerRunsOverCap", "SELECT id FROM final_targets ORDER BY id;"},
		{"worker_recovery.sql", "LockFrozenFailAttestedFinalizeRunsOverCap", "SELECT id FROM final_targets ORDER BY id;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("queries/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			_, query, found := strings.Cut(string(data), "-- name: "+tc.name+" :")
			if !found {
				t.Fatal("query missing")
			}
			query, _, _ = strings.Cut(query, "-- name:")
			if !strings.Contains(query, predicate) {
				t.Error("latest eligible pending round predicate missing")
			}
			if !strings.Contains(query, tc.fence) {
				t.Error("lock/result fence missing")
			}
		})
	}
}
