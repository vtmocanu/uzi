package store_test

import (
	"os"
	"strings"
	"testing"
)

// Both generic recovery promoters must leave the account-driven hold in place.
// Removing either predicate makes this test red without requiring a database.
func TestRecoveryPromotersExcludeCodexAccountUnavailable(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	const exclusion = "recovery_wait_cause IS DISTINCT FROM 'codex_account_unavailable'"
	for _, name := range []string{"PromoteRecoveryWaitRuns", "PromoteRecoveryWaitRunNow"} {
		t.Run(name, func(t *testing.T) {
			marker := "-- name: " + name + " "
			start := strings.Index(string(raw), marker)
			if start < 0 {
				t.Fatalf("query %s missing", name)
			}
			body := string(raw[start+len(marker):])
			update := strings.Index(body, "UPDATE runs SET")
			if update < 0 {
				t.Fatalf("%s has no UPDATE", name)
			}
			body = body[update:]
			end := strings.Index(body, ";")
			if end < 0 || !strings.Contains(body[:end], exclusion) {
				t.Fatalf("%s can promote a quarantined account hold", name)
			}
		})
	}
}
