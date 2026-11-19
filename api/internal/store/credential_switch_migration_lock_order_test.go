package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCredentialSwitchMigrationsLockOneBusyTableEach pins the issue #4201 lock-order fixes. A goose
// migration is one transaction, and one that holds a lock on one busy table while it waits on
// another can deadlock against a hot path that takes them in the other order (both reproduced
// against PostgreSQL 17): the append path takes the worker row, then the run row, then inserts into
// run_messages; a token delete locks user_secrets and then runs through its ON DELETE SET NULL
// cascade. Every migration that adds the in-place switch schema must therefore lock at most one
// busy table per section, counting both the ALTER target and every REFERENCES parent (a CREATE TABLE
// with a foreign key, or a DROP TABLE of one, locks the parent too). Matched by content, not by
// number, so the landing renumber does not move it.
func TestCredentialSwitchMigrationsLockOneBusyTableEach(t *testing.T) {
	markers := []string{"credential_epoch_ordinal", "credential_switch_apply_nonce", "run_credential_switch_epochs",
		// PRD #4320: the model provider migrations (user_secrets, users, runs columns) follow the same one-busy-table rule.
		"model_provider",
		// 00478 and 00480 validate the provider foreign keys and carry no "model_provider" literal.
		"provider_fkey", "provider_key_check"}
	busy := map[string]bool{"runs": true, "user_secrets": true, "run_messages": true, "run_usage": true, "workers": true}
	alter := regexp.MustCompile(`(?i)\bALTER TABLE\s+(?:IF EXISTS\s+)?([a-z_]+)`)
	refs := regexp.MustCompile(`(?i)\bREFERENCES\s+([a-z_]+)`)
	// An explicit LOCK takes the same table lock as an ALTER, so a migration that locks a busy table
	// this way counts toward the one-busy-table rule too. The TABLE keyword is optional (`LOCK runs`),
	// the target may be schema-qualified (`public.runs`) and one statement may lock a comma list
	// (`LOCK TABLE a, b`); busyIn splits the list and strips the schema prefix.
	lockTable := regexp.MustCompile(`(?i)\bLOCK\s+(?:TABLE\s+)?(?:ONLY\s+)?([a-z_.]+(?:\s*,\s*[a-z_.]+)*)`)
	dropTable := regexp.MustCompile(`(?i)\bDROP TABLE\b`)
	comment := regexp.MustCompile(`--[^\n]*`)
	busyIn := func(section string, re *regexp.Regexp, into map[string]bool) {
		for _, m := range re.FindAllStringSubmatch(section, -1) {
			for _, target := range strings.Split(m[1], ",") {
				name := strings.ToLower(strings.TrimSpace(target))
				if i := strings.LastIndex(name, "."); i >= 0 {
					name = name[i+1:]
				}
				if busy[name] {
					into[name] = true
				}
			}
		}
	}
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // G304: f comes from a Glob over this package's own migrations dir, not from input.
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
		up, down = comment.ReplaceAllString(up, ""), comment.ReplaceAllString(down, "")
		upLocks, downLocks := map[string]bool{}, map[string]bool{}
		busyIn(up, alter, upLocks)
		busyIn(up, refs, upLocks)
		busyIn(up, lockTable, upLocks)
		busyIn(down, alter, downLocks)
		busyIn(down, refs, downLocks)
		busyIn(down, lockTable, downLocks)
		if dropTable.MatchString(down) {
			// Dropping the table the Up created locks every parent its foreign keys reference.
			busyIn(up, refs, downLocks)
		}
		for name, locks := range map[string]map[string]bool{"Up": upLocks, "Down": downLocks} {
			if len(locks) > 1 {
				t.Errorf("%s %s locks %d busy tables %v in one transaction, want at most 1 (lock-order deadlock, issue #4201)", f, name, len(locks), locks)
			}
		}
	}
	if seen < 5 {
		t.Fatalf("found %d migrations carrying the in-place switch schema, want at least 5 (the instrument lost its targets)", seen)
	}
}
