package workersvc

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These regressions enter through Service.Claim and Heartbeat, not the expiry
// helper. Removing expiry from claimCheckpointRun must fail the fresh-claim case.
func TestCodexSwitchWorkerLossClaimIntegrationLiveDB(t *testing.T) {
	for _, scenario := range []string{"fresh claim", "notice failure", "same generation readoption"} {
		t.Run(scenario, func(t *testing.T) {
			x := newOutageCodexFlight(t, codexAuthModeAPIKey)
			id := x.f.runID
			x.env.exec("UPDATE workers SET credential_switch_apply_nonce=snapshot_register_nonce, protocol_capabilities=array_append(protocol_capabilities,$2) WHERE id=$1", x.f.workerID, capability.CodexCredentialSwitchV1)
			target := x.env.seedStaticAPIKey(t, x.f.userID, "unused-switch-target", "unused-target-key")
			x.env.exec("UPDATE runs SET credential_switch_requested_at=now(), credential_switch_generation=claim_generation WHERE id=$1", id)
			x.env.exec(`UPDATE runs SET codex_switch_target_secret_id=$2,
     codex_switch_target_generation=credential_switch_generation,
     codex_switch_target_op_id=credential_switch_op_id,
     codex_switch_target_auth_mode='api_key',
     codex_switch_target_material_revision=(SELECT material_revision FROM codex_credential_state WHERE user_secret_id=$2)
     WHERE id=$1`, id, target)
			requested := mustRun(t, x.env, id)
			if !requested.CredentialSwitchOpID.Valid {
				t.Fatal("fixture has no pending operation")
			}
			nonce := hbWorker(t, x.env, x.f.workerID).SnapshotRegisterNonce.String
			x.beat(t, 1, nonce, x.liveEntry("running"))
			x.provisional(t, "stale_worker")
			if scenario == "same generation readoption" {
				custody := x.custody(t)
				x.beat(t, 2, nonce, x.liveEntry("running"))
				restored := mustRun(t, x.env, id)
				if restored.Status != "running" || restored.ClaimGeneration != requested.ClaimGeneration ||
					restored.CredentialSwitchOpID != requested.CredentialSwitchOpID ||
					restored.CodexSwitchTargetSecretID != requested.CodexSwitchTargetSecretID ||
					restored.CodexSwitchTargetMaterialRevision != requested.CodexSwitchTargetMaterialRevision ||
					restored.LastSeq != requested.LastSeq || restored.CodexClaimEpoch != requested.CodexClaimEpoch ||
					!bytes.Equal(restored.CodexCapHash, requested.CodexCapHash) {
					t.Fatal("real readoption changed pending request or credential authority")
				}
				if !reflect.DeepEqual(custody, x.custody(t)) {
					t.Fatal("readoption changed custody")
				}
				x.assertAuthority(t, hbWorker(t, x.env, x.f.workerID), x.p.Secrets.Codex.Capability, nil)
				return
			}
			// A valid omission allows an ordinary fresh claim, rather than readoption.
			x.beat(t, 2, nonce, []ActiveRunEntry{})
			before := mustRun(t, x.env, id)
			custody := x.custody(t)
			if scenario == "notice failure" {
				x.env.exec(`ALTER TABLE run_messages ADD CONSTRAINT codex_switch_notice_failure
      CHECK (payload->>'reason' IS DISTINCT FROM 'expired_by_worker_loss') NOT VALID`)
				t.Cleanup(func() {
					if _, err := x.env.pool.Exec(context.Background(), "ALTER TABLE run_messages DROP CONSTRAINT IF EXISTS codex_switch_notice_failure"); err != nil {
						t.Error(err)
					}
				})
				p, err := x.f.svc.Claim(x.env.ctx, hbWorker(t, x.env, x.f.workerID), nil)
				if err == nil || p != nil {
					t.Fatal("notice failure released a claim payload")
				}
				// Read committed state AFTER the production caller returns; no test-owned
				// rollback can hide a missing rollback in the real claim path.
				after := mustRun(t, x.env, id)
				if after.ClaimGeneration != before.ClaimGeneration || after.LastSeq != before.LastSeq ||
					after.WorkerID != before.WorkerID || after.CredentialSwitchOpID != before.CredentialSwitchOpID ||
					after.CodexSwitchTargetSecretID != before.CodexSwitchTargetSecretID ||
					after.CodexClaimEpoch != before.CodexClaimEpoch || !bytes.Equal(after.CodexCapHash, before.CodexCapHash) ||
					!reflect.DeepEqual(custody, x.custody(t)) {
					t.Fatal("failed notice committed claim advancement, expiry, authority or custody")
				}
				return
			}
			observed := false
			x.f.svc.claimHooks = &claimTestHooks{beforeAssembly: func(_ context.Context, run store.Run) {
				observed = true
				if run.ClaimGeneration != before.ClaimGeneration+1 || run.CredentialSwitchOpID.Valid ||
					run.CredentialSwitchRequestedAt.Valid || run.CodexSwitchTargetSecretID.Valid ||
					run.CodexSwitchTargetAuthMode.Valid || run.CodexSwitchTargetMaterialRevision.Valid ||
					run.CodexSwitchTargetAccountKey.Valid || run.CodexSwitchTargetAccountRevision.Valid ||
					len(run.CodexCapHash) != 0 || run.CodexClaimEpoch != before.CodexClaimEpoch+1 ||
					run.CodexSecretID != before.CodexSecretID || run.LastSeq <= before.LastSeq {
					t.Fatal("fresh claim did not expire the request atomically before assembly")
				}
			}}
			fresh, err := x.f.svc.Claim(x.env.ctx, hbWorker(t, x.env, x.f.workerID), nil)
			if err != nil || fresh == nil || fresh.Secrets.Codex == nil || !observed {
				t.Fatalf("fresh production claim failed: payload=%t observed=%t error=%v", fresh != nil, observed, err)
			}
			after := mustRun(t, x.env, id)
			if fresh.Secrets.Codex.AccessToken != x.f.accessToken ||
				fresh.Secrets.Codex.Capability == x.p.Secrets.Codex.Capability ||
				after.CodexSecretID != before.CodexSecretID || after.CodexClaimEpoch != before.CodexClaimEpoch+2 {
				t.Fatal("fresh claim spent the pending target or reused old authority")
			}
			x.assertAuthority(t, hbWorker(t, x.env, x.f.workerID), x.p.Secrets.Codex.Capability, ErrCodexCapabilityEpoch)
			baseline, err := x.env.q.GetCodexClaimBaseline(x.env.ctx, store.GetCodexClaimBaselineParams{
				RunID: id, UserID: x.f.userID, ClaimGeneration: after.ClaimGeneration,
			})
			if err != nil || baseline.SecretID != after.CodexSecretID || baseline.CodexAuthMode != after.CodexAuthMode ||
				baseline.CodexMaterialRevision != after.CodexMaterialRevision || !baseline.CodexClaimEpoch.Valid ||
				baseline.CodexClaimEpoch.Int64 != after.CodexClaimEpoch {
				t.Fatal("fresh ordinal-zero baseline does not map to the delivered mint", err)
			}
			var kind, text, outcome, reason, op string
			var generation int64
			if err := x.env.pool.QueryRow(x.env.ctx, `SELECT kind,payload->>'text',payload->>'outcome',payload->>'reason',
      payload->>'op_id',claim_generation FROM run_messages WHERE run_id=$1 AND seq=$2`, id, after.LastSeq).
				Scan(&kind, &text, &outcome, &reason, &op, &generation); err != nil {
				t.Fatal(err)
			}
			if kind != "text" || text == "" || outcome != ApplyOutcomeNotApplied ||
				reason != "expired_by_worker_loss" || op != uuid.UUID(requested.CredentialSwitchOpID.Bytes).String() ||
				generation != after.ClaimGeneration {
				t.Fatal("fresh claim has no truthful fenced expiry notice")
			}
			var children int
			if err := x.env.pool.QueryRow(x.env.ctx, "SELECT count(*) FROM run_credential_switch_epochs WHERE run_id=$1", id).Scan(&children); err != nil {
				t.Fatal(err)
			}
			if children != 0 {
				t.Fatal("expired pending target was journaled as applied")
			}
			late, err := x.f.svc.ApplyCredentialSwitch(x.env.ctx, hbWorker(t, x.env, x.f.workerID), id,
				CredentialSwitchApplyRequest{ClaimGeneration: before.ClaimGeneration, RegisterNonce: nonce, OpID: uuid.UUID(requested.CredentialSwitchOpID.Bytes).String()})
			if err != nil || late.Outcome != ApplyOutcomeFenced {
				t.Fatal("old operation not fenced after fresh claim", err)
			}
		})
	}
}
