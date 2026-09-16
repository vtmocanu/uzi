package handler

import (
	"slices"
	"testing"
)

// TestRegisterAdvertisesProtocolFeatures pins the register response's protocol_features to
// EXACTLY the union of the tokens the landed PRDs ship — #1392 M1's recovery pair, #1391 Run
// A's heartbeat_outbox, and #1247's claim_generation_fence (this api implements the fence, so
// it advertises server support) — in slice order, and — importantly — asserts it does NOT yet
// advertise "terminal_fence", owned by Run B and not to appear until it lands. A drift here is
// a wire-contract change a worker negotiates on.
func TestRegisterAdvertisesProtocolFeatures(t *testing.T) {
	got := protocolFeatures()
	want := []string{"recovery_park_cause", "recovery_release_exact_echo", "heartbeat_outbox", "claim_generation_fence"}
	if !slices.Equal(got, want) {
		t.Fatalf("protocolFeatures() = %v, want exactly %v", got, want)
	}
	if slices.Contains(got, "terminal_fence") {
		t.Fatal("protocolFeatures() advertises terminal_fence; that token belongs to Run B and must not appear until it lands")
	}
	// Fresh slice: a caller mutating the result must not corrupt the advertised set.
	got[0] = "clobbered"
	if protocolFeatures()[0] == "clobbered" {
		t.Fatal("protocolFeatures() returns an aliasable view; it must return a fresh slice")
	}
}
