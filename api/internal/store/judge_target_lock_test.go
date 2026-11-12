package store

import (
	"strconv"
	"strings"
	"testing"
)

// The judge target lock's class is a literal in queries/judge.sql (sqlc cannot bind a Go
// constant into the statement). This pins the generated statement, which is what executes, to the
// named constant in migrate.go and proves the class is among those the source enumeration checks
// for collisions (TestProductTokenMintLockClassMatchesSQL).
func TestJudgeTargetLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(JudgeTargetLockClass), 10)
	if !strings.Contains(lockRunForJudge, "pg_advisory_xact_lock("+want+",") {
		t.Fatalf("LockRunForJudge does not lock class %s (JudgeTargetLockClass):\n%s", want, lockRunForJudge)
	}
	classes := lockClassesFromSource(t)
	if got, ok := classes["JudgeTargetLockClass"]; !ok || got != int64(JudgeTargetLockClass) {
		t.Fatalf("source parse gave JudgeTargetLockClass = %d (found %t), compiled value %d", got, ok, JudgeTargetLockClass)
	}
	for name, v := range classes {
		if name != "JudgeTargetLockClass" && v == int64(JudgeTargetLockClass) {
			t.Errorf("JudgeTargetLockClass collides with %s = %d", name, v)
		}
	}
}
