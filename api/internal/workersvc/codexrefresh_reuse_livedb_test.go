package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// The CI-owned LiveDB lane executes the real receipt query and the survivor's
// SQL fence with the service's derived bound. No reuse provider exchange occurs.
func TestCodexReuseReceiptSurvivorLiveDB(t *testing.T) {
	for _, lane := range []string{"callback", "usage401"} {
		t.Run(lane, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			provider := &fakeRefreshClient{err: errors.New("ambiguous transport")}
			f := newRefreshFixture(t, env, provider)
			op := uuid.New()
			if lane == "callback" {
				res, err := f.svc.coordinatedRefresh(env.ctx, f.userID, f.accountID, op, 0, time.Now().Add(codexRefreshLeaseTTL), time.Now().Add(codexRefreshLeaseTTL-codexRefreshCommitReserve))
				assertAmbiguous(t, res, err)
			} else {
				principal := codexPollPrincipal{userID: f.userID, accountID: f.accountID, generation: 0}
				_, _, failure := f.svc.handleCodexUsageUnauthorized(env.ctx, env.q, nil, principal)
				if failure == nil {
					t.Fatal("usage failure missing")
				}
				acct := env.mustAccount(t, f.userID, f.accountID)
				if !acct.CoordOperationID.Valid {
					t.Fatal("usage operation missing")
				}
				op = uuid.UUID(acct.CoordOperationID.Bytes)
			}
			it, err := env.q.GetCodexRefreshIntent(env.ctx, store.GetCodexRefreshIntentParams{UserID: f.userID, OperationID: op})
			if err != nil || !it.ProviderErrorAt.Valid || it.ReuseAttemptedAt.Valid || it.State != "rotating" {
				t.Fatalf("receipt=%+v,%v", it, err)
			}
			if _, err := env.pool.Exec(env.ctx, "UPDATE codex_provider_account SET lease_deadline=now()-interval '1 second' WHERE id=$1", f.accountID); err != nil {
				t.Fatal(err)
			}
			if _, err := env.pool.Exec(env.ctx, "UPDATE user_secrets SET disabled_at=now() WHERE id=$1", f.aliasID); err != nil {
				t.Fatal(err)
			}
			f.svc.vlt = vault.New(env.box, env.q) // locked, even the master-sealed login must defer
			resolved, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
			if err != nil || resolved != 0 {
				t.Fatalf("eligible receipt resolved=%d,%v", resolved, err)
			}
			// Simulate a consumed retry whose caller clock incorrectly declares its
			// policy window expired. The SQL fence must use DB time and the same
			// derived bound, despite the stale caller verdict.
			if _, err := env.pool.Exec(env.ctx, "UPDATE codex_refresh_intent SET reuse_attempted_at=now(),created_at=now()-interval '1 hour' WHERE operation_id=$1", op); err != nil {
				t.Fatal(err)
			}
			f.svc.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
			resolved, err = f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID)
			if err != nil || resolved != 0 {
				t.Fatalf("inflight receipt resolved=%d,%v", resolved, err)
			}
			it, err = env.q.GetCodexRefreshIntent(env.ctx, store.GetCodexRefreshIntentParams{UserID: f.userID, OperationID: op})
			if err != nil || it.State != "rotating" {
				t.Fatalf("stale clock defeated fence: %+v,%v", it, err)
			}
			if provider.calls != 1 || provider.discoverCalls != 0 {
				t.Fatal("M2 unexpectedly called provider again")
			}
		})
	}
}
