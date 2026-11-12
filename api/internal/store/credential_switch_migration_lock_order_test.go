package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCredentialSwitchMigrationsLockOneBusyTableEach pins the issue #4201 lock-order fix: a goose
// migration is one transaction, and one that holds ACCESS EXCLUSIVE on one busy table (run_messages,
// run_usage, workers) while it waits on another deadlocks against the append path, which takes the
// worker row, then the run row, then inserts into run_messages (reproduced against PostgreSQL 17).
// Every migration that adds the in-place switch schema must therefore touch exactly one table in its
// Up section, and the same in its Down section. Matched by content, not by number, so the landing
// renumber does not move it.
func TestCredentialSwitchMigrationsLockOneBusyTableEach(t *testing.T) {
	markers := []string{"credential_epoch_ordinal", "credential_switch_apply_nonce", "run_credential_switch_epochs"}
	stmt := regexp.MustCompile(`(?im)^\s*(ALTER TABLE|CREATE TABLE|DROP TABLE)\b`)
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		hit := false
		for _, m := range markers {
			if strings.Contains(body, m) {
				hit = true
			}
		}
		if !hit {
			continue
		}
		seen++
		up, down, ok := strings.Cut(body, "-- +goose Down")
		if !ok {
			t.Fatalf("%s: no Down section", f)
		}
		for name, section := range map[string]string{"Up": up, "Down": down} {
			if n := len(stmt.FindAllString(section, -1)); n > 1 {
				t.Errorf("%s %s: %d table statements in one transaction, want at most 1 (lock-order deadlock, issue #4201)", f, name, n)
			}
		}
	}
	if seen < 4 {
		t.Fatalf("found %d migrations carrying the in-place switch schema, want at least 4 (the instrument lost its targets)", seen)
	}
}
