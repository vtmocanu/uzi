package handler

import (
	"slices"
	"testing"
)

// TestRegisterAdvertisesProtocolFeatures pins the register response's protocol_features to
// EXACTLY the union of the tokens the landed PRDs ship — #1392 M1's recovery pair plus
// #1391 Run A's heartbeat_outbox — in slice order, and — importantly — asserts it does NOT
// advertise "claim_generation_fence"/"terminal_fence", owned by #1390/#1247 and Run B and
// not to appear until those land. A drift here is a wire-contract change a worker negotiates on.
func TestRegisterAdvertisesProtocolFeatures(t *testing.T) {
	got := protocolFeatures()
	want := []string{"recovery_park_cause", "recovery_release_exact_echo", "heartbeat_outbox"}
	if !slices.Equal(got, want) {
		t.Fatalf("protocolFeatures() = %v, want exactly %v", got, want)
	}
	if slices.Contains(got, "claim_generation_fence") {
		t.Fatal("protocolFeatures() advertises claim_generation_fence; that token is owned by #1390 and must not appear here")
	}
	// Fresh slice: a caller mutating the result must not corrupt the advertised set.
	got[0] = "clobbered"
	if protocolFeatures()[0] == "clobbered" {
		t.Fatal("protocolFeatures() returns an aliasable view; it must return a fresh slice")
	}
}
