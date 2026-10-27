package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestOversizeRunClaimFinishesMintedCodexRunLiveDB: an ordinary Codex issue run whose real
// assembled claim exceeds the (lowered) limit goes through assembleAndFinishRunClaim and fails
// terminally as provisioning_failed with a typed reason. The minted capability's identity rides
// the refusal, so the exact-claim finish settles it (revoked, epoch bumped) rather than going
// idle on a superseded mint and leaving the run claimed.
func TestOversizeRunClaimFinishesMintedCodexRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.claimSizeLimit = 1024

	before := mustRun(t, env, f.runID)
	payload, err := svc.assembleAndFinishRunClaim(env.ctx, f.wkr, before, false)
	if payload != nil || err != nil {
		t.Fatalf("assembleAndFinishRunClaim = (%v, %v), want (nil, nil)", payload, err)
	}
	after := mustRun(t, env, f.runID)
	if after.Status != "failed" || after.FailOrigin.String != "provisioning_failed" {
		t.Fatalf("run left status=%s origin=%q reason=%q", after.Status, after.FailOrigin.String, after.FailureReason.String)
	}
	if !strings.HasPrefix(after.FailureReason.String, "claim too large:") {
		t.Fatalf("failure_reason = %q", after.FailureReason.String)
	}
	if after.CodexClaimEpoch <= before.CodexClaimEpoch || len(after.CodexCapHash) != 0 {
		t.Fatalf("capability not revoked: hash=%x epoch=%d (before %d)", after.CodexCapHash, after.CodexClaimEpoch, before.CodexClaimEpoch)
	}
}

// TestOversizeRunClaimMeasuresFinalTaskProjectionLiveDB (#3396): an enrolled task's claim gains
// the checkpoint recovery projection inside the finish transaction, after assembly. The size
// check must measure that final body: a limit one byte under the final size fails the run, and
// a limit exactly at it delivers a body of exactly that size.
func TestOversizeRunClaimMeasuresFinalTaskProjectionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, tc := range []struct {
		name    string
		slack   int
		deliver bool
	}{{"one byte over the final body fails", -1, false}, {"exactly the final body delivers", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			id := seedCoordinateTask(f)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
				[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			final := 0
			f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, payload *ClaimPayload, err error) {
				if err != nil || payload == nil || run.ID != id {
					t.Fatalf("assembly err=%v", err)
				}
				if payload.CheckpointWriteUnresolved != nil {
					t.Fatal("projection already applied before the finish transaction")
				}
				// With no checkpoint evidence the projection adds only checkpoint_write_unresolved=false.
				probe := *payload
				unresolved := false
				probe.CheckpointWriteUnresolved = &unresolved
				raw, err := json.Marshal(&probe)
				if err != nil {
					t.Fatal(err)
				}
				final = len(raw) + 1
				f.svc.claimSizeLimit = final + tc.slack
			}}
			payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			after := mustRun(t, env, id)
			if !tc.deliver {
				if payload != nil {
					t.Fatal("an over-limit final claim was delivered")
				}
				if after.Status != "failed" || after.FailOrigin.String != "provisioning_failed" ||
					!strings.HasPrefix(after.FailureReason.String, "claim too large:") {
					t.Fatalf("run status=%s origin=%q reason=%q", after.Status, after.FailOrigin.String, after.FailureReason.String)
				}
				return
			}
			if payload == nil || payload.CheckpointWriteUnresolved == nil {
				t.Fatalf("claim not delivered with its projection: status=%s reason=%q", after.Status, after.FailureReason.String)
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw)+1 != final {
				t.Fatalf("delivered body %d bytes, predicted %d", len(raw)+1, final)
			}
		})
	}
}
