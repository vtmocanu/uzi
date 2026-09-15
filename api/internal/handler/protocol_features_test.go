package handler

import (
	"slices"
	"testing"
)

// TestRegisterAdvertisesProtocolFeatures pins the register response's protocol_features to
// EXACTLY the two tokens this PRD ships (PRD #1392 M1), in order, and — importantly — asserts
// it does NOT advertise "claim_generation_fence", which is owned by #1390 and must not appear
// until that PRD lands. A drift here is a wire-contract change a worker negotiates on.
func TestRegisterAdvertisesProtocolFeatures(t *testing.T) {
	got := protocolFeatures()
	want := []string{"recovery_park_cause", "recovery_release_exact_echo"}
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
