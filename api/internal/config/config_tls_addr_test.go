package config

import "testing"

// The API_TLS_ADDR / API_ADDR collision guard (PRD #58 M3).
//
// Two independent validators flagged the string compare this replaces. The shape
// it missed: API_ADDR=0.0.0.0:8443 against the DEFAULT API_TLS_ADDR=:8443 — different
// text, same socket, because an empty host means all interfaces. It fails closed
// (the losing Listen takes the process down) and the chart cannot produce it, so
// this is a clarity fix, not a security one: the guard exists to NAME the problem
// rather than let it surface as a race between two Listen calls.
func TestSamePortCatchesWildcardAliases(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
		why  string
	}{
		// The shape the string compare missed.
		{"0.0.0.0:8443", ":8443", true, "an empty host IS the wildcard: same socket"},
		{":8443", "0.0.0.0:8443", true, "and symmetrically"},
		{"[::]:8443", ":8443", true, "the v6 wildcard too"},
		// The plain string-equal case the old guard did catch.
		{":8443", ":8443", true, "identical"},
		// The defaults must stay legal.
		{":8080", ":8443", false, "the shipped defaults differ by port"},
		{"0.0.0.0:8080", ":8443", false, "different ports never collide, whatever the host"},
		// Two concrete different hosts on one port is legitimate and not ours to refuse.
		{"127.0.0.1:8443", "10.0.0.1:8443", false, "distinct concrete hosts can share a port"},
		// A wildcard overlaps any concrete host on the same port.
		{"127.0.0.1:8443", ":8443", true, "the wildcard swallows the concrete host"},
	} {
		if got := samePort(tc.a, tc.b); got != tc.want {
			t.Errorf("samePort(%q, %q) = %v, want %v (%s)", tc.a, tc.b, got, tc.want, tc.why)
		}
	}
}

// Unparseable input falls back to the literal compare rather than guessing.
func TestSamePortFallsBackOnUnparseableAddresses(t *testing.T) {
	if !samePort("garbage", "garbage") {
		t.Error("identical unparseable addresses must still collide")
	}
	if samePort("garbage", "other") {
		t.Error("different unparseable addresses must not be reported as colliding")
	}
}
