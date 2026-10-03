package store

import (
	"strconv"
	"strings"
	"testing"
)

// The usage-tail lock's class is a literal in queries/run_usage_tail.sql (sqlc cannot bind a Go
// constant into the statement). This pins the generated statement, which is what executes, to
// the named constant in migrate.go and proves the class is among those the source enumeration
// checks for collisions (TestProductTokenMintLockClassMatchesSQL).
func TestRunUsageLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(RunUsageLockClass), 10)
	if !strings.Contains(lockRunUsage, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockRunUsage does not lock class %s (RunUsageLockClass):\n%s", want, lockRunUsage)
	}
	classes := lockClassesFromSource(t)
	if got, ok := classes["RunUsageLockClass"]; !ok || got != int64(RunUsageLockClass) {
		t.Fatalf("source parse gave RunUsageLockClass = %d (found %t), compiled value %d", got, ok, RunUsageLockClass)
	}
	for name, v := range classes {
		if name != "RunUsageLockClass" && v == int64(RunUsageLockClass) {
			t.Errorf("RunUsageLockClass collides with %s = %d", name, v)
		}
	}
}
