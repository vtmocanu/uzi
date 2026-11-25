package store

import "testing"

// FreshResumeOwnerLockClass (issue #4628) is taken from Go with a hashtext objid, so there is no SQL
// literal to pin. This proves the class is among the source-enumerated *LockClass constants (whose
// pairwise distinctness TestProductTokenMintLockClassMatchesSQL asserts) with the documented value, and
// that no other class shares it.
func TestFreshResumeOwnerLockClassIsRegistered(t *testing.T) {
	classes := lockClassesFromSource(t)
	if got, ok := classes["FreshResumeOwnerLockClass"]; !ok || got != int64(FreshResumeOwnerLockClass) {
		t.Fatalf("source parse gave FreshResumeOwnerLockClass = %d (found %t), compiled value %d", got, ok, FreshResumeOwnerLockClass)
	}
	if FreshResumeOwnerLockClass != 0x757A6672 {
		t.Fatalf("FreshResumeOwnerLockClass = %#x, want the documented 0x757A6672", FreshResumeOwnerLockClass)
	}
	for name, v := range classes {
		if name != "FreshResumeOwnerLockClass" && v == int64(FreshResumeOwnerLockClass) {
			t.Errorf("FreshResumeOwnerLockClass collides with %s = %d", name, v)
		}
	}
}
