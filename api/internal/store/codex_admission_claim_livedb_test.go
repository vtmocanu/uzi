package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A live PLAN child uses the same admission gate as the seven other run kinds,
// including when it is returning to its assigned worker with a prior generation.
func TestCodexAdmissionCrossCheckClaimLiveDB(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "fresh"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			alias, account := uuid.New(), uuid.New()
			mustExec(ctx, t, f.pool, `INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with)
				VALUES ($1,$2,'codex_auth',$3,$4,'master')`, alias, f.userID, alias.String(), []byte("fixture"))
			mustExec(ctx, t, f.pool, `INSERT INTO codex_provider_account
				(id,user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with)
				VALUES ($1,$2,$3,$3,$4,'master')`, account, f.userID, account.String(), []byte("fixture"))
			mustExec(ctx, t, f.pool, `INSERT INTO codex_credential_state (user_secret_id,user_id,status,provider_account_id)
				VALUES ($1,$2,'linked',$3)`, alias, f.userID, account)
			mustExec(ctx, t, f.pool, `UPDATE runs SET codex_secret_id=$2,codex_auth_mode='subscription',codex_material_revision=0
				WHERE id=$1`, fx.checkerID, alias)
			if resume {
				mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=2,worker_id=$2 WHERE id=$1", fx.checkerID, f.workerID)
			}
			before, err := f.q.GetRunByID(ctx, fx.checkerID)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(ctx, t, f.pool, `INSERT INTO codex_account_admission_throttle
				(user_id,provider_account_id,effective_expiry,episode_id,spread_width_ms)
				VALUES ($1,$2,now()+interval '1 hour',gen_random_uuid(),60000)`, f.userID, account)
			_, err = f.q.ClaimRun(ctx, planCrossCheckClaimParams(f))
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("held checker claim = %v", err)
			}
			after, err := f.q.GetRunByID(ctx, fx.checkerID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != "queued" || after.ClaimGeneration != before.ClaimGeneration || after.WorkerID != before.WorkerID {
				t.Fatal("held checker changed generation or ownership")
			}
			mustExec(ctx, t, f.pool, "UPDATE codex_account_admission_throttle SET effective_expiry=now()-interval '2 minutes' WHERE provider_account_id=$1", account)
			claimed, err := f.q.ClaimRun(ctx, planCrossCheckClaimParams(f))
			if err != nil || claimed.ID != fx.checkerID || claimed.ClaimGeneration != before.ClaimGeneration+1 {
				t.Fatalf("expired checker claim = %s generation=%d err=%v", claimed.ID, claimed.ClaimGeneration, err)
			}
		})
	}
}
