package runkind

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Go twin of agent/test/run-kind-db-parity.test.ts — same algorithm, same
// three diagnostic messages. runkind.All() is a hand-maintained mirror of the DB
// runs_kind_check CHECK constraint (latest redefinition under
// api/internal/store/migrations/). Nothing links the Go constants to the SQL, so
// this test is the drift guard: it reads the live constraint out of the migration
// that last redefined it and asserts set-equality (both directions) AND order
// against All(). This file lives inside the api/ module, so Go's test cache
// rechecks it whenever the migrations change.

// dbRunKinds returns the runs_kind_check allowed set, read from the
// highest-numbered migration whose `-- +goose Up` section redefines the
// constraint.
func dbRunKinds(t *testing.T) []string {
	t.Helper()

	migrationsDir := filepath.Join("..", "store", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("could not read migrations dir %s: %v", migrationsDir, err)
	}

	constraintRe := regexp.MustCompile(`runs_kind_check\b`)
	numRe := regexp.MustCompile(`^(\d+)`)

	type candidate struct {
		name string
		up   string
		num  int
	}
	var live *candidate

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(migrationsDir, name)) //nolint:gosec // G304: test reads migration files from the fixed migrations dir
		if err != nil {
			t.Fatalf("could not read migration %s: %v", name, err)
		}
		src := string(raw)

		// Up section: everything BEFORE the first `-- +goose Down`. A Down-only
		// mention (the rollback) must not count as the live definition.
		up := src
		if idx := strings.Index(src, "-- +goose Down"); idx >= 0 {
			up = src[:idx]
		}

		m := numRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		num, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}

		if !constraintRe.MatchString(up) {
			continue
		}

		if live == nil || num > live.num {
			live = &candidate{name: name, up: up, num: num}
		}
	}

	if live == nil {
		t.Fatal("no migration Up section redefines runs_kind_check — did the constraint get renamed?")
	}

	// Anchor on the constraint name so a different CHECK in the same file cannot
	// match. `[\s\S]` so a newline between the name and CHECK(...) is fine.
	listRe := regexp.MustCompile(`(?is)runs_kind_check\b[\s\S]*?CHECK\s*\(\s*kind\s+IN\s*\(([^)]*)\)`)
	lm := listRe.FindStringSubmatch(live.up)
	if lm == nil {
		t.Fatalf("could not extract the runs_kind_check CHECK (kind IN (...)) list from %s — did its shape change?", live.name)
	}

	litRe := regexp.MustCompile(`'([^']+)'`)
	var kinds []string
	for _, mm := range litRe.FindAllStringSubmatch(lm[1], -1) {
		kinds = append(kinds, mm[1])
	}
	if len(kinds) == 0 {
		t.Fatalf("runs_kind_check in %s yielded no string literals — did the regex miss the list?", live.name)
	}
	return kinds
}

func TestAllMatchesMigrationRunsKindCheck(t *testing.T) {
	db := dbRunKinds(t)
	all := All()

	dbSet := make(map[string]bool, len(db))
	for _, k := range db {
		dbSet[k] = true
	}
	allSet := make(map[string]bool, len(all))
	for _, k := range all {
		allSet[k] = true
	}

	var missingFromAll []string
	for _, k := range db {
		if !allSet[k] {
			missingFromAll = append(missingFromAll, k)
		}
	}
	if len(missingFromAll) > 0 {
		t.Errorf("DB runs_kind_check allows kinds absent from runkind.All(): %s", strings.Join(missingFromAll, ", "))
	}

	var extraInAll []string
	for _, k := range all {
		if !dbSet[k] {
			extraInAll = append(extraInAll, k)
		}
	}
	if len(extraInAll) > 0 {
		t.Errorf("runkind.All() declares kinds the DB runs_kind_check does not allow: %s", strings.Join(extraInAll, ", "))
	}

	// Order-equality: All() is authored in DB order, so the extracted list must
	// equal All() slice-for-slice.
	if len(db) != len(all) {
		t.Fatalf("runkind.All() has drifted from the DB runs_kind_check constraint: got %v, migration has %v", all, db)
	}
	for i := range all {
		if all[i] != db[i] {
			t.Fatalf("runkind.All() has drifted from the DB runs_kind_check constraint (order): got %v, migration has %v", all, db)
		}
	}
}
