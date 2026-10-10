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
// (independent of the snapshot kill switch), and active_run_snapshot is last. A drift here is a
// wire-contract change a worker negotiates on.
func TestRegisterAdvertisesProtocolFeatures(t *testing.T) {
	got := protocolFeatures(true)
	want := []string{"dind_maintenance_v1", "recovery_park_cause", "recovery_release_exact_echo", "recovery_inventory_v1", "heartbeat_outbox", "worker_residue_quarantine", "claim_generation_fence", "terminal_fence", "recovery_cause_vault_locked", "recovery_cause_codex_account_unavailable", "gate_revision_v1", "recovery_cause_data_volume_full", "run_checkpoint_durability", "repo_agent_folder", "terminal_rejection_report", "active_run_snapshot"}
	if !slices.Equal(got, want) {
		t.Fatalf("protocolFeatures(true) = %v, want exactly %v", got, want)
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
	want := []string{"dind_maintenance_v1", "recovery_park_cause", "recovery_release_exact_echo", "recovery_inventory_v1", "heartbeat_outbox", "worker_residue_quarantine", "claim_generation_fence", "terminal_fence", "recovery_cause_vault_locked", "recovery_cause_codex_account_unavailable", "gate_revision_v1", "recovery_cause_data_volume_full", "run_checkpoint_durability", "repo_agent_folder", "terminal_rejection_report"}
	if !slices.Equal(got, want) {
		t.Fatalf("protocolFeatures(false) = %v, want exactly %v", got, want)
	}
	if slices.Contains(got, "active_run_snapshot") {
		t.Fatal("protocolFeatures(false) advertises active_run_snapshot; the feature is disabled and it must be omitted")
	}
}
