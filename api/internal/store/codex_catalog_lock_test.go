package store

import (
	"strconv"
	"strings"
	"testing"
)

// The Codex catalog lock's class is a literal in queries/codex_catalog_defaults.sql (sqlc cannot
// bind a Go constant into the statement). This pins the generated statement, which is what
// executes, to the named constant in migrate.go and proves the class is among those the source
// enumeration checks for collisions (TestProductTokenMintLockClassMatchesSQL).
func TestCodexCatalogLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(CodexCatalogLockClass), 10)
	if !strings.Contains(lockCodexCatalogDefaults, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockCodexCatalogDefaults does not lock class %s (CodexCatalogLockClass):\n%s", want, lockCodexCatalogDefaults)
	}
	classes := lockClassesFromSource(t)
	if got, ok := classes["CodexCatalogLockClass"]; !ok || got != int64(CodexCatalogLockClass) {
		t.Fatalf("source parse gave CodexCatalogLockClass = %d (found %t), compiled value %d", got, ok, CodexCatalogLockClass)
	}
	for name, v := range classes {
		if name != "CodexCatalogLockClass" && v == int64(CodexCatalogLockClass) {
			t.Errorf("CodexCatalogLockClass collides with %s = %d", name, v)
		}
	}
}
