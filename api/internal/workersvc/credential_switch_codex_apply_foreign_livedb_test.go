package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

// A different worker of the same user cannot apply a pending Codex switch for a
// run it does not own: apply errors with ErrRunNotOwned, preflight is fenced,
// nothing is written, and the real owner then applies the same op.
func TestCodexApplyForeignWorkerFencedLiveDB(t *testing.T) {
	f, run, target := codexApplyFixture(t, false)
	op := f.opOf(t, run)
	other := uuid.New()
	const otherNonce = "nonce-foreign-worker"
	f.env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at,protocol_capabilities,snapshot_register_nonce,credential_switch_apply_nonce,snapshot_epoch)
	            VALUES ($1,$2,'foreign-apply',$3,'online',now(),$4,$5,$5,0)`,
		other, f.o.userID, other[:], []string{capability.CodexCredentialSwitchV1, capability.CodexSwitchStoppedV1}, otherNonce)
	req := CredentialSwitchApplyRequest{ClaimGeneration: f.gen, RegisterNonce: otherNonce, OpID: op.String()}
	f.noWrite(t, run, func() {
		// Apply refuses a non-owner at the claim-mutation lock (ErrRunNotOwned).
		res, err := f.svc.ApplyCredentialSwitch(f.env.ctx, wkrRow(t, f.env, other), run, req)
		if !errors.Is(err, ErrRunNotOwned) || res.Codex != nil || res.Token != "" {
			t.Fatalf("a worker that does not own the run was not refused: %v %v", res, err)
		}
		// Preflight reports the same non-owner as fenced.
		pre, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, wkrRow(t, f.env, other), run, CodexSwitchPreflightRequest{CredentialSwitchApplyRequest: req, TargetSecretID: target})
		if err != nil || pre.Outcome != ApplyOutcomeFenced {
			t.Fatalf("preflight from a non-owner was not fenced: %v %v", pre, err)
		}
	})
	if res := f.apply(t, run, f.req(op)); res.Outcome != ApplyOutcomeApplied || res.Codex == nil {
		t.Fatalf("owning worker could not apply after the foreign attempt: %v", res)
	}
}

// A committed op replayed under a NEW claim generation, on a live running claim,
// hits the prior-journal generation check (credential_switch_apply.go Step 1: the
// journal row belongs to the old generation) and is fenced rather than reported
// as refused_committed.
func TestCodexReplayCommittedOpUnderNewGenerationFencedLiveDB(t *testing.T) {
	x := newOutageCodexFlight(t, codexAuthModeAPIKey)
	id := x.f.runID
	x.env.exec("UPDATE workers SET protocol_capabilities=array_append(array_append(protocol_capabilities,'codex_switch_stopped_v1'),$2), credential_switch_apply_nonce=snapshot_register_nonce WHERE id=$1", x.f.workerID, capability.CodexCredentialSwitchV1)
	x.env.exec("UPDATE runs SET status='running', session_id=$2 WHERE id=$1", id, "12345678-1234-1234-1234-123456789abc")
	target := x.env.seedStaticAPIKey(t, x.f.userID, "replay-target", "replay-target-key")
	if _, err := x.f.svc.SetRunCredential(x.env.ctx, x.f.userID, id, BindModePinned, &target); err != nil {
		t.Fatal(err)
	}
	pending := mustRun(t, x.env, id)
	op := uuid.UUID(pending.CredentialSwitchOpID.Bytes).String()
	nonce := hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String
	first := CredentialSwitchApplyRequest{ClaimGeneration: pending.ClaimGeneration, RegisterNonce: nonce, OpID: op}
	if res, err := x.f.svc.ApplyCredentialSwitch(x.env.ctx, hbWorker(t, x.env, x.f.workerID), id, first); err != nil || res.Outcome != ApplyOutcomeApplied {
		t.Fatal("switch setup failed", err)
	}
	x.beat(t, 1, nonce, x.liveEntry("running"))
	x.provisional(t, "stale_worker")
	x.beat(t, 2, nonce, []ActiveRunEntry{})
	if fresh, err := x.f.svc.Claim(x.env.ctx, hbWorker(t, x.env, x.f.workerID), nil); err != nil || fresh == nil {
		t.Fatal("fresh recovery claim failed", err)
	}
	// The fresh claim leaves the run claimed; move it to running (as the
	// first generation's fixture does) so the live-status fence passes and the
	// prior-journal generation check is what decides the replay.
	x.env.exec("UPDATE runs SET status='running' WHERE id=$1", id)
	after := mustRun(t, x.env, id)
	if after.ClaimGeneration != pending.ClaimGeneration+1 || after.Status != "running" {
		t.Fatal("fresh claim did not advance the generation to a running claim")
	}
	snap := func() string {
		var s string
		if err := x.env.pool.QueryRow(x.env.ctx, `SELECT jsonb_build_object(
		    'run', (SELECT to_jsonb(r) FROM runs r WHERE r.id = $1),
		    'switch_epochs', (SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.claim_generation, e.ordinal), '[]'::jsonb) FROM run_credential_switch_epochs e WHERE e.run_id = $1),
		    'base_epochs', (SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.claim_generation), '[]'::jsonb) FROM run_credential_epochs e WHERE e.run_id = $1),
		    'messages', (SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.seq), '[]'::jsonb) FROM run_messages m WHERE m.run_id = $1)
		)::text`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snap()
	req := CredentialSwitchApplyRequest{ClaimGeneration: after.ClaimGeneration, RegisterNonce: hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String, OpID: op}
	res, err := x.f.svc.ApplyCredentialSwitch(x.env.ctx, hbWorker(t, x.env, x.f.workerID), id, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ApplyOutcomeFenced || res.Codex != nil || res.Token != "" {
		t.Fatalf("committed op replayed under a new generation was not fenced: %v", res)
	}
	if snap() != before {
		t.Fatal("fenced replay under a new generation wrote to the run")
	}
}
