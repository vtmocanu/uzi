package store

import "testing"

// AnalyticsPrecomputeLockClass (issue #4771) is taken from Go with a fixed objid, so there is no SQL
// literal to pin. This proves the class is among the source-enumerated *LockClass constants (whose
// pairwise distinctness TestProductTokenMintLockClassMatchesSQL asserts) with the documented value,
// and that no other class shares it.
func TestAnalyticsPrecomputeLockClassIsRegistered(t *testing.T) {
	classes := lockClassesFromSource(t)
	if got, ok := classes["AnalyticsPrecomputeLockClass"]; !ok || got != int64(AnalyticsPrecomputeLockClass) {
		t.Fatalf("source parse gave AnalyticsPrecomputeLockClass = %d (found %t), compiled value %d", got, ok, AnalyticsPrecomputeLockClass)
	}
	if AnalyticsPrecomputeLockClass != 0x757A6170 {
		t.Fatalf("AnalyticsPrecomputeLockClass = %#x, want the documented 0x757A6170", AnalyticsPrecomputeLockClass)
	}
	for name, v := range classes {
		if name != "AnalyticsPrecomputeLockClass" && v == int64(AnalyticsPrecomputeLockClass) {
			t.Errorf("AnalyticsPrecomputeLockClass collides with %s = %d", name, v)
		}
	}
}
