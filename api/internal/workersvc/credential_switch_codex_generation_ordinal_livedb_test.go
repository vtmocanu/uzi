package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

// Pins that the Codex switch journal ordinal restarts per claim generation:
// a switch applied after a fresh claim (new generation, whose ordinal-zero
// baseline is the claim-time run_credential_epochs row) is ordinal 1 again,
// not max-over-the-run + 1, and the earlier generation's row is untouched.
func TestCodexSwitchOrdinalRestartsPerGenerationLiveDB(t *testing.T) {
	x := newOutageCodexFlight(t, codexAuthModeAPIKey)
	id := x.f.runID
	x.env.exec("UPDATE workers SET protocol_capabilities=array_append(array_append(protocol_capabilities,'codex_switch_stopped_v1'),$2), credential_switch_apply_nonce=snapshot_register_nonce WHERE id=$1", x.f.workerID, capability.CodexCredentialSwitchV1)
	x.env.exec("UPDATE runs SET status='running', session_id=$2 WHERE id=$1", id, "12345678-1234-1234-1234-123456789abc")

	applyPending := func(label string) (CredentialSwitchApplyResult, int64) {
		t.Helper()
		pending := mustRun(t, x.env, id)
		nonce := hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String
		req := CredentialSwitchApplyRequest{ClaimGeneration: pending.ClaimGeneration, RegisterNonce: nonce, OpID: uuid.UUID(pending.CredentialSwitchOpID.Bytes).String()}
		res, err := x.f.svc.ApplyCredentialSwitch(x.env.ctx, hbWorker(t, x.env, x.f.workerID), id, req)
		if err != nil || res.Outcome != ApplyOutcomeApplied {
			t.Fatalf("%s switch apply: outcome=%q err=%v", label, res.Outcome, err)
		}
		return res, pending.ClaimGeneration
	}

	first := x.env.seedStaticAPIKey(t, x.f.userID, "gen-first-target", "gen-first-target-key")
	if _, err := x.f.svc.SetRunCredential(x.env.ctx, x.f.userID, id, BindModePinned, &first); err != nil {
		t.Fatal(err)
	}
	res1, gen1 := applyPending("first-generation")
	if res1.Ordinal != 1 {
		t.Fatalf("first-generation switch ordinal = %d, want 1", res1.Ordinal)
	}

	// Recover via a fresh claim, which starts a new claim generation and
	// records its own ordinal-zero baseline.
	nonce := hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String
	x.beat(t, 1, nonce, x.liveEntry("running"))
	x.provisional(t, "stale_worker")
	x.beat(t, 2, nonce, []ActiveRunEntry{})
	fresh, err := x.f.svc.Claim(x.env.ctx, hbWorker(t, x.env, x.f.workerID), nil)
	if err != nil || fresh == nil || fresh.Secrets.Codex == nil {
		t.Fatal("fresh recovery claim failed", err)
	}
	after := mustRun(t, x.env, id)
	if after.ClaimGeneration != gen1+1 {
		t.Fatalf("claim generation = %d, want %d", after.ClaimGeneration, gen1+1)
	}

	// A claim leaves the run `claimed` (owner requests are refused while the
	// payload is assembled); the worker moves it to running once it starts the
	// provider, which the fixture models directly as in the first generation.
	x.env.exec("UPDATE runs SET status='running' WHERE id=$1", id)
	second := x.env.seedStaticAPIKey(t, x.f.userID, "gen-second-target", "gen-second-target-key")
	if _, err := x.f.svc.SetRunCredential(x.env.ctx, x.f.userID, id, BindModePinned, &second); err != nil {
		t.Fatal("second owner request refused:", err)
	}
	res2, gen2 := applyPending("second-generation")
	if gen2 != gen1+1 {
		t.Fatalf("second request claim generation = %d, want %d", gen2, gen1+1)
	}
	if res2.Ordinal != 1 {
		t.Fatalf("second-generation switch ordinal = %d, want 1 (ordinals restart per claim generation)", res2.Ordinal)
	}

	postMint := mustRun(t, x.env, id)
	var ordinal int32
	var epoch int64
	if err := x.env.pool.QueryRow(x.env.ctx, "SELECT ordinal, codex_claim_epoch FROM run_credential_switch_epochs WHERE run_id=$1 AND claim_generation=$2", id, gen2).Scan(&ordinal, &epoch); err != nil {
		t.Fatal("second-generation journal row:", err)
	}
	if ordinal != 1 || epoch != postMint.CodexClaimEpoch {
		t.Fatalf("second-generation journal row ordinal=%d epoch=%d, want 1 and run epoch %d", ordinal, epoch, postMint.CodexClaimEpoch)
	}
	var firstOrdinal int32
	if err := x.env.pool.QueryRow(x.env.ctx, "SELECT ordinal FROM run_credential_switch_epochs WHERE run_id=$1 AND claim_generation=$2", id, gen1).Scan(&firstOrdinal); err != nil || firstOrdinal != 1 {
		t.Fatalf("first-generation journal row ordinal=%d err=%v, want 1", firstOrdinal, err)
	}
}
