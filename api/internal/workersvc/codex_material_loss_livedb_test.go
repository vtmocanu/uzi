package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func TestCodexMaterialLossExhaustionLiveDB(t *testing.T) {
	for _, unverified := range []bool{false, true} {
		for _, background := range []bool{false, true} {
			name := map[bool]string{false: "commit", true: "unverified"}[unverified] + map[bool]string{false: "/sync", true: "/background"}[background]
			t.Run(name, func(t *testing.T) {
				e := setupCodexLiveDB(t)
				fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("terminal-loss")}}
				f := newRefreshFixture(t, e, fake)
				cleanupCodexRecoveryUser(t, e.pool, f.userID)
				cap := e.mintCap(t, f.runID, f.workerID)
				enrollCodexProtectedVault(t, e, f.svc.vlt, f.userID, codexVaultLockedTestPassword)
				before := e.mustAccount(t, f.userID, f.accountID)
				fake.freshClaimsSet = unverified
				fake.onRefresh = func() {
					f.svc.vlt.Lock(f.userID)
					f.svc.SetVault(vault.NewPostgres(nil, e.pool))
				}
				var pending func()
				f.svc.background = nil
				if background {
					f.svc.background = func(fn func()) { pending = fn }
				}
				op := uuid.New()
				res, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
				if err == nil || (!background && errors.Is(err, ErrCodexVaultLocked)) || res.AccessToken != "" {
					t.Fatalf("first response=(%+v,%v)", res, err)
				}
				if background {
					if pending == nil {
						t.Fatal("no background handoff")
					}
					if mustIntent(t, e, op, f.userID).State != codexIntentRotating {
						t.Fatal("pending intent must remain rotating")
					}
					pending()
				} else if !errors.Is(err, ErrCodexRefreshUnrecoverable) {
					t.Fatalf("synchronous loss: %v", err)
				}
				acct := e.mustAccount(t, f.userID, f.accountID)
				if acct.CoordState != codexCoordQuarantined || mustIntent(t, e, op, f.userID).State != codexIntentUnrecoverable {
					t.Fatal("terminal loss did not atomically park account and intent")
				}
				if acct.Generation != before.Generation || acct.CredentialRevision != before.CredentialRevision ||
					!reflect.DeepEqual(acct.SealedLogin, before.SealedLogin) || len(acct.RecoverySealed) != 0 ||
					acct.RecoveryCause.Valid || acct.ReauthRequired || acct.ReauthReason.Valid {
					t.Fatal("terminal loss changed material/revisions or invented a recovery/rejection cause")
				}
				res, err = f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
				if !errors.Is(err, ErrCodexAccountQuarantined) || errors.Is(err, ErrCodexVaultLocked) || res.AccessToken != "" {
					t.Fatalf("retry=(%+v,%v)", res, err)
				}
				if fake.calls != 1 || fake.discoverCalls != 0 {
					t.Fatalf("provider calls=%d discover=%d", fake.calls, fake.discoverCalls)
				}
			})
		}
	}
}

func TestCodexMaterialLossRecoveryWriterFenceLiveDB(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "background"}[background], func(t *testing.T) {
			e := setupCodexLiveDB(t)
			fake := &fakeRefreshClient{
				result:         codexauth.RefreshResult{AccessToken: codexToken("fence-loss")},
				freshClaimsSet: true,
			}
			f := newRefreshFixture(t, e, fake)
			cleanupCodexRecoveryUser(t, e.pool, f.userID)
			cap := e.mintCap(t, f.runID, f.workerID)
			op := uuid.New()
			move := func() {
				e.exec("UPDATE codex_provider_account SET coord_operation_id=$1 WHERE id=$2", uuid.New(), f.accountID)
				e.exec("UPDATE codex_refresh_intent SET state='reconciled' WHERE operation_id=$1", op)
			}
			var pending func()
			f.svc.background = nil
			if background {
				f.svc.q = &recoverySlotHookStore{Queries: e.q, failFirst: codexRecoverySlotSyncAttempts}
				f.svc.background = func(fn func()) { pending = fn }
			} else {
				fake.onRefresh = move
			}
			res, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
			if err == nil || res.AccessToken != "" || errors.Is(err, ErrCodexVaultLocked) {
				t.Fatalf("response=(%+v,%v)", res, err)
			}
			if background {
				if pending == nil {
					t.Fatal("missing background handoff")
				}
				move()
			}
			before := e.mustAccount(t, f.userID, f.accountID)
			if pending != nil {
				pending()
			}
			if after := e.mustAccount(t, f.userID, f.accountID); !reflect.DeepEqual(before, after) {
				t.Fatal("recovery fence miss changed another operation's account")
			}
			if mustIntent(t, e, op, f.userID).State != codexIntentReconciled {
				t.Fatal("recovery fence miss overwrote settled intent")
			}
			if fake.calls != 1 {
				t.Fatalf("provider calls=%d", fake.calls)
			}
		})
	}
}

type codexMaterialLossFailIntentBeginner struct{ *pgxpool.Pool }

func (b codexMaterialLossFailIntentBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return codexMaterialLossFailIntentTx{Tx: tx}, nil
}

type codexMaterialLossFailIntentTx struct{ pgx.Tx }

func (tx codexMaterialLossFailIntentTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "-- name: MarkCodexRejectionIntentUnrecoverable") {
		return pgconn.CommandTag{}, errors.New("injected terminal intent failure")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestCodexMaterialLossRollbackLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	f := newRefreshFixture(t, e, &fakeRefreshClient{})
	cleanupCodexRecoveryUser(t, e.pool, f.userID)
	op := uuid.New()
	e.exec("UPDATE codex_provider_account SET coord_state='in_progress', coord_operation_id=$1 WHERE id=$2", op, f.accountID)
	if _, err := e.q.InsertCodexRefreshIntent(e.ctx, store.InsertCodexRefreshIntentParams{
		OperationID: op, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
	}); err != nil {
		t.Fatal(err)
	}
	before := e.mustAccount(t, f.userID, f.accountID)
	intent := mustIntent(t, e, op, f.userID)
	f.svc.SetTxBeginner(codexMaterialLossFailIntentBeginner{Pool: e.pool})
	if applied, err := f.svc.quarantineCodexMaterialLoss(e.ctx, f.userID, f.accountID, op, 0); applied || err == nil {
		t.Fatalf("partial failure=(%v,%v)", applied, err)
	}
	if !reflect.DeepEqual(before, e.mustAccount(t, f.userID, f.accountID)) ||
		!reflect.DeepEqual(intent, mustIntent(t, e, op, f.userID)) {
		t.Fatal("failed intent update committed a partial account transition")
	}
}

func TestCodexMaterialLossFencesLiveDB(t *testing.T) {
	for _, branch := range []string{"owned", "already quarantined", "generation moved", "operation moved", "durable recovery", "settled intent", "intent generation", "intent account"} {
		t.Run(branch, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			f := newRefreshFixture(t, e, &fakeRefreshClient{})
			cleanupCodexRecoveryUser(t, e.pool, f.userID)
			op := uuid.New()
			e.exec("UPDATE codex_provider_account SET coord_state='in_progress', coord_operation_id=$1 WHERE id=$2", op, f.accountID)
			if _, err := e.q.InsertCodexRefreshIntent(e.ctx, store.InsertCodexRefreshIntentParams{
				OperationID: op, UserID: f.userID, ProviderAccountID: f.accountID, FromGeneration: 0,
			}); err != nil {
				t.Fatal(err)
			}
			switch branch {
			case "already quarantined":
				e.exec("UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1", f.accountID)
			case "generation moved":
				e.exec("UPDATE codex_provider_account SET generation=1 WHERE id=$1", f.accountID)
			case "operation moved":
				e.exec("UPDATE codex_provider_account SET coord_operation_id=$1 WHERE id=$2", uuid.New(), f.accountID)
			case "durable recovery":
				e.exec("UPDATE codex_provider_account SET recovery_sealed=$1, recovery_generation=0, recovery_sealed_with='master' WHERE id=$2", []byte("durable-copy"), f.accountID)
			case "settled intent":
				e.exec("UPDATE codex_refresh_intent SET state='reconciled' WHERE operation_id=$1", op)
			case "intent generation":
				e.exec("UPDATE codex_refresh_intent SET from_generation=1 WHERE operation_id=$1", op)
			case "intent account":
				other, err := e.q.InsertCodexProviderAccount(e.ctx, store.InsertCodexProviderAccountParams{
					UserID: f.userID, ProviderUserID: uuid.NewString(), WorkspaceAccountID: uuid.NewString(), SealedLogin: []byte("other"), SealedWith: store.SealedWithMaster,
				})
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE codex_refresh_intent SET provider_account_id=$1 WHERE operation_id=$2", other.ID, op)
			}
			before := e.mustAccount(t, f.userID, f.accountID)
			intent := mustIntent(t, e, op, f.userID)
			ctx, cancel := context.WithCancel(e.ctx)
			cancel()
			applied, err := f.svc.quarantineCodexMaterialLoss(ctx, f.userID, f.accountID, op, 0)
			want := branch == "owned" || branch == "already quarantined"
			if err != nil || applied != want {
				t.Fatalf("transition=(%v,%v), want %v", applied, err, want)
			}
			after := e.mustAccount(t, f.userID, f.accountID)
			afterIntent := mustIntent(t, e, op, f.userID)
			if !want {
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(intent, afterIntent) {
					t.Fatal("fence miss changed account or intent")
				}
			} else if after.CoordState != codexCoordQuarantined || afterIntent.State != codexIntentUnrecoverable ||
				after.Generation != before.Generation || after.CredentialRevision != before.CredentialRevision ||
				after.ReauthRequired || after.RecoveryCause.Valid {
				t.Fatal("terminal transition violated account/intent contract")
			}
		})
	}
}
