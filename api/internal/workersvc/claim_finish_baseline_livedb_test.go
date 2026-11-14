package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// The real Claim path must persist its required baseline before delivering the mint.
// Executed by the exact-head CI LiveDB sweep.
func TestFinishRunClaimCodexBaselineLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	wkr := fx.claimant(t, true)
	payload, err := fx.svc.Claim(env.ctx, wkr, nil)
	if err != nil || payload == nil || payload.Secrets.Codex == nil {
		t.Fatalf("claim did not deliver Codex authority: payload_present=%t err=%v", payload != nil, err)
	}
	minted, _, ok := parseCodexCapability(payload.Secrets.Codex.Capability)
	if !ok {
		t.Fatal("claim did not deliver a valid capability")
	}
	run := mustRun(t, env, fx.runID)
	if minted != run.CodexClaimEpoch || minted == 0 {
		t.Fatalf("mint=%d persisted epoch=%d", minted, run.CodexClaimEpoch)
	}
	var provider, account string
	if err := env.pool.QueryRow(env.ctx, `SELECT provider_user_id,workspace_account_id
 FROM codex_provider_account WHERE id=$1`, fx.accountID).Scan(&provider, &account); err != nil {
		t.Fatal(err)
	}
	key, err := json.Marshal([]string{provider, account})
	if err != nil {
		t.Fatal(err)
	}
	type baseline struct {
		secret                uuid.UUID
		label, reason, mode   string
		revision, epoch       int64
		accountKey, accountID string
	}
	want := baseline{
		secret: fx.aliasID, label: run.CodexSecretLabel.String, reason: "codex_binding",
		mode: "subscription", revision: run.CodexMaterialRevision.Int64, epoch: minted,
		accountKey: string(key), accountID: account,
	}
	assertBaseline := func() {
		t.Helper()
		var got baseline
		var count int
		if err := env.pool.QueryRow(env.ctx, `SELECT secret_id,label,select_reason,codex_auth_mode,
 codex_material_revision,codex_claim_epoch,codex_account_key,codex_account_id,count(*) OVER ()
 FROM run_credential_epochs WHERE run_id=$1 AND claim_generation=$2`, run.ID, run.ClaimGeneration).
			Scan(&got.secret, &got.label, &got.reason, &got.mode, &got.revision, &got.epoch,
				&got.accountKey, &got.accountID, &count); err != nil {
			t.Fatal(err)
		}
		if count != 1 || got != want {
			t.Fatalf("baseline count=%d got=%+v want=%+v", count, got, want)
		}
	}
	assertBaseline()
	// Retry the same finalized claim with changed display metadata. The first
	// baseline must survive, including its original label and actual mint.
	env.exec("UPDATE runs SET codex_secret_label='renamed-after-claim' WHERE id=$1", run.ID)
	changed := mustRun(t, env, run.ID)
	repeated, err := fx.svc.finishRunClaim(env.ctx, changed, payload, nil,
		claimRecoveryIdentity{workerID: wkr.ID, recoveryCapable: true})
	if err != nil || repeated == nil {
		t.Fatalf("repeat finish failed: payload_present=%t err=%v", repeated != nil, err)
	}
	assertBaseline()
	// Confirm the row used for the repeat really carried the changed metadata.
	if changed.CodexSecretLabel.String == want.label || changed.ClaimGeneration != run.ClaimGeneration {
		t.Fatal("retry fixture did not change metadata within the same claim")
	}
}
