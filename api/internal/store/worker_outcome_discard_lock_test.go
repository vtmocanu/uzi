package store

import (
	"strconv"
	"strings"
	"testing"
)

// The discard lock's class is a literal in queries/worker_outcome_discards.sql (sqlc cannot bind a
// Go constant into the statement). This pins the generated statement, which is what executes, to
// the named constant in migrate.go and proves the class is among those the source enumeration
// checks for collisions (TestProductTokenMintLockClassMatchesSQL).
func TestWorkerOutcomeDiscardLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(WorkerOutcomeDiscardLockClass), 10)
	if !strings.Contains(lockWorkerOutcomeDiscards, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockWorkerOutcomeDiscards does not lock class %s (WorkerOutcomeDiscardLockClass):\n%s", want, lockWorkerOutcomeDiscards)
	}
	classes := lockClassesFromSource(t)
	if got, ok := classes["WorkerOutcomeDiscardLockClass"]; !ok || got != int64(WorkerOutcomeDiscardLockClass) {
		t.Fatalf("source parse gave WorkerOutcomeDiscardLockClass = %d (found %t), compiled value %d", got, ok, WorkerOutcomeDiscardLockClass)
	}
	for name, v := range classes {
		if name != "WorkerOutcomeDiscardLockClass" && v == int64(WorkerOutcomeDiscardLockClass) {
			t.Errorf("WorkerOutcomeDiscardLockClass collides with %s = %d", name, v)
		}
	}
}
