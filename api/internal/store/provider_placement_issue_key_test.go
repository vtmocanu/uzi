package store_test

import (
	"os"
	"strings"
	"testing"
)

func TestProviderPlacementDownPreservesIssueKeyGate(t *testing.T) {
	b, err := os.ReadFile("migrations/00486_placement_provider_capability.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(b), "-- +goose Down")
	clause := "AND ('issue_key_v1' = ANY(w.protocol_capabilities) OR NOT EXISTS (SELECT 1 FROM repos lr WHERE lr.id = r.repo_id AND lr.issue_tracker_connection_id IS NOT NULL))"
	if !ok || strings.Count(down, clause) != 1 || strings.Contains(down, "AND (r.model_provider_key_mode IS NULL") {
		t.Fatal("provider placement Down must remove only the provider gate and retain the issue-key gate")
	}
}
