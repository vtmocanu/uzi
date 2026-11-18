package store

import (
	"testing"

	"github.com/google/uuid"
)

// HibernationOwnerLockClass (issue #4395) is taken from Go, so there is no SQL literal to pin.
// This proves the class is among the source-enumerated *LockClass constants (whose pairwise
// distinctness TestProductTokenMintLockClassMatchesSQL asserts) with the documented value, and
// that no other class shares it.
func TestHibernationOwnerLockClassIsRegistered(t *testing.T) {
	classes := lockClassesFromSource(t)
	if got, ok := classes["HibernationOwnerLockClass"]; !ok || got != int64(HibernationOwnerLockClass) {
		t.Fatalf("source parse gave HibernationOwnerLockClass = %d (found %t), compiled value %d", got, ok, HibernationOwnerLockClass)
	}
	if HibernationOwnerLockClass != 0x757A686F {
		t.Fatalf("HibernationOwnerLockClass = %#x, want the documented 0x757A686F", HibernationOwnerLockClass)
	}
	for name, v := range classes {
		if name != "HibernationOwnerLockClass" && v == int64(HibernationOwnerLockClass) {
			t.Errorf("HibernationOwnerLockClass collides with %s = %d", name, v)
		}
	}
}

// The hibernation owner objid is the first four uuid bytes, big endian, and
// SecretMutationLockObjID keeps exactly its pre-refactor derivation.
func TestHibernationOwnerLockObjIDDerivation(t *testing.T) {
	id := uuid.MustParse("01020304-0000-4000-8000-000000000000")
	if got := HibernationOwnerLockObjID(id); got != 0x01020304 {
		t.Fatalf("HibernationOwnerLockObjID = %#x, want 0x01020304", got)
	}
	if got := SecretMutationLockObjID(id); got != 0x01020304 {
		t.Fatalf("SecretMutationLockObjID = %#x, want 0x01020304", got)
	}
	high := uuid.MustParse("ff000001-0000-4000-8000-000000000000")
	if got := HibernationOwnerLockObjID(high); got != -16777215 { // 0xff000001 wrapped to int32
		t.Fatalf("HibernationOwnerLockObjID(high) = %d, want -16777215", got)
	}
}
