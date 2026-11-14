package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise the expiry transaction used by claimCheckpointRun. Full claim
// admission and outer payload delivery still require the exact-head LiveDB sweep.
func TestCodexFreshClaimPendingExpiryLiveDB(t *testing.T) {
	for _, scenario := range []string{"expires", "same generation", "rollback", "notice failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newApplyFx(t)
			id, op := f.run(t, "running")
			old := f.env.seedStaticAPIKey(t, f.o.userID, "expiry-old", "old-test-key")
			target := f.env.seedStaticAPIKey(t, f.o.userID, "expiry-target", "target-test-key")
			if err := f.svc.FreezeCodexBinding(f.env.ctx, f.o.userID, id, old, codexAuthModeAPIKey); err != nil {
				t.Fatal(err)
			}
			f.env.exec("UPDATE runs SET codex_switch_target_secret_id=$2, codex_switch_target_generation=credential_switch_generation, codex_switch_target_op_id=credential_switch_op_id WHERE id=$1", id, target)
			before := mustRun(t, f.env, id)
			tx, err := f.env.pool.Begin(f.env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			q := store.New(tx)
			if scenario != "same generation" {
				if _, err := tx.Exec(f.env.ctx, "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", id); err != nil {
					t.Fatal(err)
				}
			}
			run, err := q.GetRunByID(f.env.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "notice failure" {
				if _, err := tx.Exec(f.env.ctx, "ALTER TABLE run_messages ADD CONSTRAINT codex_expiry_test_refuse_notice CHECK (kind <> 'credential_switch') NOT VALID"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := expireCodexSwitchOnFreshClaim(f.env.ctx, q, run)
			if scenario == "notice failure" {
				if err == nil {
					t.Fatal("notice failure did not fail expiry")
				}
				if rollbackErr := tx.Rollback(f.env.ctx); rollbackErr != nil {
					t.Fatal(rollbackErr)
				}
				after := mustRun(t, f.env, id)
				if after.ClaimGeneration != before.ClaimGeneration || after.LastSeq != before.LastSeq ||
					after.CredentialSwitchOpID != before.CredentialSwitchOpID || after.CodexSwitchTargetSecretID != before.CodexSwitchTargetSecretID {
					t.Fatal("failed notice retained claim or expiry writes")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.CodexSecretID != before.CodexSecretID || got.CodexAuthMode != before.CodexAuthMode ||
				got.CodexMaterialRevision != before.CodexMaterialRevision || got.CodexClaimEpoch != before.CodexClaimEpoch {
				t.Fatal("expiry changed committed binding")
			}
			if scenario == "same generation" {
				if got.CredentialSwitchOpID != before.CredentialSwitchOpID || got.LastSeq != before.LastSeq ||
					got.CodexSwitchTargetSecretID != before.CodexSwitchTargetSecretID {
					t.Fatal("same generation changed pending operation")
				}
			} else {
				if got.CredentialSwitchRequestedAt.Valid || got.CredentialSwitchOpID.Valid ||
					got.CodexSwitchTargetSecretID.Valid || got.LastSeq <= before.LastSeq {
					t.Fatal("pending operation not cleared with sequenced notice")
				}
				var payload []byte
				var generation int64
				if err := tx.QueryRow(f.env.ctx, "SELECT payload, claim_generation FROM run_messages WHERE run_id=$1 AND seq=$2 AND kind='credential_switch'", id, got.LastSeq).Scan(&payload, &generation); err != nil {
					t.Fatal(err)
				}
				var notice struct {
					Outcome string `json:"outcome"`
					Reason  string `json:"reason"`
					OpID    string `json:"op_id"`
				}
				if err := json.Unmarshal(payload, &notice); err != nil {
					t.Fatal(err)
				}
				if notice.Outcome != ApplyOutcomeNotApplied || notice.Reason != "expired_by_worker_loss" ||
					notice.OpID != op.String() || generation != f.gen+1 {
					t.Fatal("incorrect expiry notice")
				}
			}
			if scenario == "rollback" {
				if err := tx.Rollback(f.env.ctx); err != nil {
					t.Fatal(err)
				}
				after := mustRun(t, f.env, id)
				if after.ClaimGeneration != before.ClaimGeneration || after.LastSeq != before.LastSeq ||
					after.CredentialSwitchOpID != before.CredentialSwitchOpID || after.CodexSwitchTargetSecretID != before.CodexSwitchTargetSecretID {
					t.Fatal("rollback retained expiry writes")
				}
				return
			}
			if err := tx.Commit(f.env.ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
