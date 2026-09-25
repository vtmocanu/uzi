package store_test

import (
	"os"
	"strings"
	"testing"
)

// Both generic recovery promoters must leave the account-driven hold in place.
// Read generated SQL too: these constants are what store.Queries actually executes.
func TestRecoveryPromotersExcludeCodexAccountUnavailable(t *testing.T) {
	const exclusion = "recovery_wait_cause IS DISTINCT FROM 'codex_account_unavailable'"
	source, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile("runtime.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		file string
		raw  []byte
	}{{"queries/runtime.sql", source}, {"runtime.sql.go", generated}} {
		for _, tc := range []struct{ name, constant string }{
			{"PromoteRecoveryWaitRuns", "promoteRecoveryWaitRuns"},
			{"PromoteRecoveryWaitRunNow", "promoteRecoveryWaitRunNow"},
		} {
			t.Run(input.file+"/"+tc.name, func(t *testing.T) {
				var body string
				if input.file == "runtime.sql.go" {
					marker := "const " + tc.constant + " = `"
					start := strings.Index(string(input.raw), marker)
					if start < 0 {
						t.Fatalf("generated query %s missing", tc.name)
					}
					body = string(input.raw[start+len(marker):])
					end := strings.Index(body, "`")
					if end < 0 {
						t.Fatal("generated SQL string is unterminated")
					}
					body = body[:end]
				} else {
					marker := "-- name: " + tc.name + " "
					start := strings.Index(string(input.raw), marker)
					if start < 0 {
						t.Fatalf("source query %s missing", tc.name)
					}
					body = string(input.raw[start+len(marker):])
					update := strings.Index(body, "UPDATE runs SET")
					if update < 0 {
						t.Fatalf("%s has no UPDATE", tc.name)
					}
					body = body[update:]
					end := strings.Index(body, ";")
					if end < 0 {
						t.Fatal("source SQL statement is unterminated")
					}
					body = body[:end]
				}
				if !strings.Contains(body, exclusion) {
					t.Fatalf("%s in %s can promote a quarantined account hold", tc.name, input.file)
				}
			})
		}
	}
}
