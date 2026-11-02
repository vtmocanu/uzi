package store_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// These source-query checks protect the snapshot anti-join's run scope while
// sqlc regeneration and PostgreSQL execution remain separate integration checks.
func TestPolicyHoldSnapshotSQLScope(t *testing.T) {
	antiJoin := regexp.MustCompile(`(?s)AND NOT EXISTS \(SELECT 1 FROM worker_active_runs a[^\n]*\n\s*WHERE a.worker_id = @worker_id AND a.run_id = runs.id.*?AND a.claim_generation = runs.claim_generation\)`)
	for _, tc := range []struct {
		file, name string
		stages     int
	}{
		{"runtime.sql", "RequeueRunsMissingFromSnapshot", 3},
		{"worker_recovery.sql", "frozenFailRunsMissingFromSnapshotLocked", 4},
		{"worker_recovery.sql", "FrozenRequeueRunsMissingFromSnapshot", 3},
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
			matches := antiJoin.FindAllStringIndex(query, -1)
			if len(matches) != tc.stages {
				t.Fatalf("snapshot anti-join stages=%d, want %d", len(matches), tc.stages)
			}
			previous := 0
			for i, match := range matches {
				block := query[match[0]:match[1]]
				if strings.Contains(block, "r.hold_reason") || strings.Contains(block, "r.policy_outcome_id") {
					t.Errorf("stage %d: snapshot anti-join references undefined run alias r", i)
				}
				// Each stage independently excludes a displayed policy hold and
				// an unresolved episode before consulting the worker snapshot.
				scope := query[previous:match[0]]
				if !strings.Contains(scope, "runs.hold_reason IS DISTINCT FROM 'codex_policy_hold'") ||
					!strings.Contains(scope, "(runs.policy_outcome_id IS NULL OR runs.policy_resolved_at IS NOT NULL)") {
					t.Errorf("stage %d: enclosing run policy guards missing", i)
				}
				previous = match[1]
			}
		})
	}
}
