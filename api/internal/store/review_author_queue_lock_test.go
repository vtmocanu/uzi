package store

import (
	"strconv"
	"strings"
	"testing"
)

// The review-author queue lock's class is a literal in queries/mr_review_authors.sql (sqlc cannot
// bind a Go constant into the statement). This pins the generated statement, which is what
// executes, to the named constant in migrate.go. Distinctness from every other two-int class is
// asserted by TestProductTokenMintLockClassMatchesSQL, which enumerates every *LockClass from
// source; this test additionally proves ReviewAuthorQueueLockClass is among the enumerated
// classes.
func TestReviewAuthorQueueLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(ReviewAuthorQueueLockClass), 10)
	if !strings.Contains(lockReviewAuthorQueue, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockReviewAuthorQueue does not lock class %s (ReviewAuthorQueueLockClass):\n%s", want, lockReviewAuthorQueue)
	}
	if ReviewAuthorQueueLockClass != 1970958961 {
		t.Fatalf("ReviewAuthorQueueLockClass = %d, want the pinned literal 1970958961 (0x757A7271, \"uzrq\")", ReviewAuthorQueueLockClass)
	}
	classes := lockClassesFromSource(t)
	if got, ok := classes["ReviewAuthorQueueLockClass"]; !ok || got != int64(ReviewAuthorQueueLockClass) {
		t.Fatalf("source parse gave ReviewAuthorQueueLockClass = %d (found %t), compiled value %d", got, ok, ReviewAuthorQueueLockClass)
	}
	for name, v := range classes {
		if name != "ReviewAuthorQueueLockClass" && v == int64(ReviewAuthorQueueLockClass) {
			t.Errorf("ReviewAuthorQueueLockClass collides with %s = %d", name, v)
		}
	}
}
