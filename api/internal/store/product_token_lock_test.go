package store

import (
	"strconv"
	"strings"
	"testing"
)

// The mint lock's class is a literal in queries/product_tokens.sql (sqlc cannot bind a
// Go constant into the statement). This pins the generated statement, which is what
// executes, to the named constant in migrate.go, and keeps the class distinct from the
// other two-int lock classes.
func TestProductTokenMintLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(ProductTokenMintLockClass), 10)
	if !strings.Contains(lockProductTokenMint, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockProductTokenMint does not lock class %s (ProductTokenMintLockClass):\n%s", want, lockProductTokenMint)
	}
	for name, other := range map[string]int32{
		"HostedProvisionLockClass": HostedProvisionLockClass,
		"SecretMutationLockClass":  SecretMutationLockClass,
	} {
		if other == ProductTokenMintLockClass {
			t.Fatalf("ProductTokenMintLockClass collides with %s", name)
		}
	}
}
