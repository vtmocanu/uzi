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

// The per-user grant lock's class is a literal in queries/oauth.sql too: this pins the generated
// LockOAuthUserGrants statement to OAuthUserLockClass, and the shared source enumeration keeps it
// distinct from every other two-int lock class.
func TestOAuthUserLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(OAuthUserLockClass), 10)
	if !strings.Contains(lockOAuthUserGrants, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockOAuthUserGrants does not lock class %s (OAuthUserLockClass):\n%s", want, lockOAuthUserGrants)
	}
	if got, ok := lockClassesFromSource(t)["OAuthUserLockClass"]; !ok || got != int64(OAuthUserLockClass) {
		t.Fatalf("source parse gave OAuthUserLockClass = %d (found %t), compiled value %d", got, ok, OAuthUserLockClass)
	}
}
