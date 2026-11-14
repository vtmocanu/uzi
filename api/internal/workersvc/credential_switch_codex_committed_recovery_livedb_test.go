package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Model worker loss after switch commit but before provider initialization.
// Ordinary recovery must claim the committed B binding, never replay the old op.
func TestCodexCommittedSwitchFreshClaimLiveDB(t *testing.T) {
	x := newOutageCodexFlight(t, codexAuthModeAPIKey)
	id := x.f.runID
	x.env.exec("UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,$2), credential_switch_apply_nonce=snapshot_register_nonce WHERE id=$1", x.f.workerID, capability.CodexCredentialSwitchV1)
	x.env.exec("UPDATE runs SET status='running' WHERE id=$1", id)
	target := x.env.seedStaticAPIKey(t, x.f.userID, "committed-target", "committed-target-key")
	if _, err := x.f.svc.SetRunCredential(x.env.ctx, x.f.userID, id, BindModePinned, &target); err != nil {
		t.Fatal(err)
	}
	pending := mustRun(t, x.env, id)
	nonce := hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String
	req := CredentialSwitchApplyRequest{ClaimGeneration: pending.ClaimGeneration, RegisterNonce: nonce, OpID: uuid.UUID(pending.CredentialSwitchOpID.Bytes).String()}
	applied, err := x.f.svc.ApplyCredentialSwitch(x.env.ctx, hbWorker(t, x.env, x.f.workerID), id, req)
	if err != nil || applied.Outcome != ApplyOutcomeApplied || applied.Codex == nil {
		t.Fatal("switch setup failed", err)
	}
	committed := mustRun(t, x.env, id)
	x.beat(t, 1, nonce, x.liveEntry("running"))
	x.provisional(t, "stale_worker")
	x.beat(t, 2, nonce, []ActiveRunEntry{})
	fresh, err := x.f.svc.Claim(x.env.ctx, hbWorker(t, x.env, x.f.workerID), nil)
	if err != nil || fresh == nil || fresh.Secrets.Codex == nil {
		t.Fatal("fresh recovery claim failed", err)
	}
	after := mustRun(t, x.env, id)
	if fresh.Secrets.Codex.AccessToken != "committed-target-key" ||
		fresh.Secrets.Codex.Capability == applied.Codex.Capability ||
		after.CodexSecretID != committed.CodexSecretID ||
		after.ClaimGeneration != committed.ClaimGeneration+1 ||
		after.CodexClaimEpoch != committed.CodexClaimEpoch+2 ||
		after.CredentialSwitchOpID.Valid || after.CodexSwitchTargetSecretID.Valid {
		t.Fatal("fresh claim did not remint the committed B binding")
	}
	x.assertAuthority(t, hbWorker(t, x.env, x.f.workerID), applied.Codex.Capability, ErrCodexCapabilityEpoch)
	baseline, err := x.env.q.GetCodexClaimBaseline(x.env.ctx, store.GetCodexClaimBaselineParams{RunID: id, UserID: x.f.userID, ClaimGeneration: after.ClaimGeneration})
	if err != nil || baseline.SecretID != after.CodexSecretID || !baseline.CodexClaimEpoch.Valid ||
		baseline.CodexClaimEpoch.Int64 != after.CodexClaimEpoch {
		t.Fatal("recovered B baseline has wrong authority", err)
	}
	late, err := x.f.svc.ApplyCredentialSwitch(x.env.ctx, hbWorker(t, x.env, x.f.workerID), id, req)
	if err != nil || late.Outcome != ApplyOutcomeFenced || late.Codex != nil {
		t.Fatal("old committed operation returned fresh authority", err)
	}
}
