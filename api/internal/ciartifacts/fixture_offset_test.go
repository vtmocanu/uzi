package ciartifacts

import "testing"

func fixtureZIPOffset(t *testing.T, n int) uint32 {
	t.Helper()
	if n < 0 || n > 64<<20 {
		t.Fatal("fixture offset exceeds archive bound")
		return 0
	}
	return uint32(n)
}
