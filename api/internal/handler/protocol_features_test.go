package handler

import (
	"slices"
	"testing"
)

// TestRegisterAdvertisesProtocolFeatures pins the register response's protocol_features to
// EXACTLY the union of the tokens the landed PRDs ship — #1392 M1's recovery pair, #1391 Run
// A's heartbeat_outbox, issue #2213's worker_residue_quarantine, #1247's claim_generation_fence (this api implements the fence, so it
// advertises server support), #1391 Run B M3c's terminal_fence (the api now fences a terminal
// transition on messages_through_seq contiguity), issue #1766 M2's recovery_cause_vault_locked
// (unconditional), #1795 M1's gate_revision_v1 (unconditional), PRD #1809 M5's
// recovery_cause_data_volume_full (unconditional), PRD #1809 M6's run_checkpoint_durability
// (unconditional), issue #2085's repo_agent_folder (unconditional), and #1390 M2a's active_run_snapshot when the feature is enabled — in slice
// order (append order = declaration order). terminal_fence sits AFTER claim_generation_fence,
// then recovery_cause_vault_locked, then gate_revision_v1, then recovery_cause_data_volume_full,
// then run_checkpoint_durability, then repo_agent_folder, then terminal_rejection_report
// (independent of the snapshot kill switch), then active_run_snapshot, and issue #2545's
// recovery_held_publication_v1 (own group, gated on UZI_HELD_PUBLICATION) is last. A drift here is a
// wire-contract change a worker negotiates on. Issue #2507 adds
// recovery_completed_publication_v1 beside the recovery inventory feature.
func TestRegisterAdvertisesProtocolFeatures(t *testing.T) {
	got := protocolFeatures(true, true)
	want := []string{"dind_maintenance_v1", "recovery_park_cause", "recovery_release_exact_echo", "recovery_inventory_v1", "recovery_completed_publication_v1", "heartbeat_outbox", "worker_residue_quarantine", "claim_generation_fence", "terminal_fence", "recovery_cause_vault_locked", "recovery_cause_codex_account_unavailable", "gate_revision_v1", "recovery_cause_data_volume_full", "run_checkpoint_durability", "repo_agent_folder", "terminal_rejection_report", "active_run_snapshot", "recovery_held_publication_v1"}
	if !slices.Equal(got, want) {
		t.Fatalf("protocolFeatures(true, true) = %v, want exactly %v", got, want)
	}
	// Fresh slice: a caller mutating the result must not corrupt the advertised set.
	got[0] = "clobbered"
	if protocolFeatures(true, true)[0] == "clobbered" {
		t.Fatal("protocolFeatures returns an aliasable view; it must return a fresh slice")
	}
}

// TestProtocolFeaturesOmitsSnapshotWhenDisabled pins the D7 rollback surface: an api started
// with UZI_ACTIVE_SNAPSHOT_DISABLED omits active_run_snapshot from the advertised set (so a
// worker never sends the snapshot) while every other landed token stays exactly as it was.
func TestProtocolFeaturesOmitsSnapshotWhenDisabled(t *testing.T) {
	got := protocolFeatures(false, true)
	want := []string{"dind_maintenance_v1", "recovery_park_cause", "recovery_release_exact_echo", "recovery_inventory_v1", "recovery_completed_publication_v1", "heartbeat_outbox", "worker_residue_quarantine", "claim_generation_fence", "terminal_fence", "recovery_cause_vault_locked", "recovery_cause_codex_account_unavailable", "gate_revision_v1", "recovery_cause_data_volume_full", "run_checkpoint_durability", "repo_agent_folder", "terminal_rejection_report", "recovery_held_publication_v1"}
	if !slices.Equal(got, want) {
		t.Fatalf("protocolFeatures(false, true) = %v, want exactly %v", got, want)
	}
	if slices.Contains(got, "active_run_snapshot") {
		t.Fatal("protocolFeatures(false, true) advertises active_run_snapshot; the feature is disabled and it must be omitted")
	}
}

// TestProtocolFeaturesHeldPublicationSwitch pins issue #2545's server feature: it is advertised
// as its own trailing group while UZI_HELD_PUBLICATION is on and omitted, with every other
// token untouched, while it is off (a worker then defers nothing and archives as before).
func TestProtocolFeaturesHeldPublicationSwitch(t *testing.T) {
	for _, snapshot := range []bool{true, false} {
		on, off := protocolFeatures(snapshot, true), protocolFeatures(snapshot, false)
		if !slices.Contains(on, "recovery_held_publication_v1") || on[len(on)-1] != "recovery_held_publication_v1" {
			t.Fatalf("snapshot=%t: switch on = %v, want recovery_held_publication_v1 last", snapshot, on)
		}
		if slices.Contains(off, "recovery_held_publication_v1") {
			t.Fatalf("snapshot=%t: switch off still advertises recovery_held_publication_v1: %v", snapshot, off)
		}
		if !slices.Equal(on[:len(on)-1], off) {
			t.Fatalf("snapshot=%t: the switch changed more than its own token: on=%v off=%v", snapshot, on, off)
		}
	}
}
