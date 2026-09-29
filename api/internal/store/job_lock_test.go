package store

import (
	"strconv"
	"strings"
	"testing"
)

// The job-create lock's class is a literal in queries/jobs.sql (sqlc cannot bind a Go constant
// into the statement). This pins the generated statement, which is what executes, to the named
// constant in migrate.go. Distinctness from every other two-int class is asserted by
// TestProductTokenMintLockClassMatchesSQL, which enumerates every *LockClass from source; this
// test additionally proves JobCreateLockClass is among the enumerated classes.
func TestJobCreateLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(JobCreateLockClass), 10)
	if !strings.Contains(lockJobCreate, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockJobCreate does not lock class %s (JobCreateLockClass):\n%s", want, lockJobCreate)
	}
	classes := lockClassesFromSource(t)
	if got, ok := classes["JobCreateLockClass"]; !ok || got != int64(JobCreateLockClass) {
		t.Fatalf("source parse gave JobCreateLockClass = %d (found %t), compiled value %d", got, ok, JobCreateLockClass)
	}
	for name, v := range classes {
		if name != "JobCreateLockClass" && v == int64(JobCreateLockClass) {
			t.Errorf("JobCreateLockClass collides with %s = %d", name, v)
		}
	}
}
