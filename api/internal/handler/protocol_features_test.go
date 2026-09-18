package handler

import (
	"slices"
	"testing"
)

// TestRegisterAdvertisesProtocolFeatures pins the register response's protocol_features to
// EXACTLY the union of the tokens the landed PRDs ship — #1392 M1's recovery pair, #1391 Run
// A's heartbeat_outbox, #1247's claim_generation_fence (this api implements the fence, so it
// advertises server support), and #1390 M2a's active_run_snapshot when the feature is enabled —
// in slice order (append order = declaration order), and — importantly — asserts it does NOT
// advertise "terminal_fence", owned by Run B and not to appear until it lands. A drift here is
// a wire-contract change a worker negotiates on.
func TestRegisterAdvertisesProtocolFeatures(t *testing.T) {
	got := protocolFeatures(true)
	want := []string{"recovery_park_cause", "recovery_release_exact_echo", "heartbeat_outbox", "claim_generation_fence", "active_run_snapshot"}
	if !slices.Equal(got, want) {
		t.Fatalf("protocolFeatures(true) = %v, want exactly %v", got, want)
	}
	if slices.Contains(got, "terminal_fence") {
		t.Fatal("protocolFeatures advertises terminal_fence; that token belongs to Run B and must not appear until it lands")
	}
	// Fresh slice: a caller mutating the result must not corrupt the advertised set.
	got[0] = "clobbered"
	if protocolFeatures(true)[0] == "clobbered" {
		t.Fatal("protocolFeatures returns an aliasable view; it must return a fresh slice")
	}
}

// TestProtocolFeaturesOmitsSnapshotWhenDisabled pins the D7 rollback surface: an api started
// with UZI_ACTIVE_SNAPSHOT_DISABLED omits active_run_snapshot from the advertised set (so a
// worker never sends the snapshot) while every other landed token stays exactly as it was.
func TestProtocolFeaturesOmitsSnapshotWhenDisabled(t *testing.T) {
	got := protocolFeatures(false)
	want := []string{"recovery_park_cause", "recovery_release_exact_echo", "heartbeat_outbox", "claim_generation_fence"}
	if !slices.Equal(got, want) {
		t.Fatalf("protocolFeatures(false) = %v, want exactly %v", got, want)
	}
	if slices.Contains(got, "active_run_snapshot") {
		t.Fatal("protocolFeatures(false) advertises active_run_snapshot; the feature is disabled and it must be omitted")
	}
}
