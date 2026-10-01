package store

import (
	"strconv"
	"strings"
	"testing"
)

// The authorize lock's class is a literal in queries/oauth.sql (sqlc cannot bind a Go constant
// into the statement). This pins the generated statement, which is what executes, to the named
// constant in migrate.go; TestProductTokenMintLockClassMatchesSQL's source enumeration keeps the
// class distinct from every other two-int lock class.
func TestOAuthAuthorizeLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(OAuthAuthorizeLockClass), 10)
	if !strings.Contains(lockOAuthAuthorize, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockOAuthAuthorize does not lock class %s (OAuthAuthorizeLockClass):\n%s", want, lockOAuthAuthorize)
	}
	if got, ok := lockClassesFromSource(t)["OAuthAuthorizeLockClass"]; !ok || got != int64(OAuthAuthorizeLockClass) {
		t.Fatalf("source parse gave OAuthAuthorizeLockClass = %d (found %t), compiled value %d", got, ok, OAuthAuthorizeLockClass)
	}
}
