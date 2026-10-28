package workersvc

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Registered after the pool-close cleanup, so the user cascade runs first.
// Each cleanup has one attempt with a five-second deadline; errors fail its test.
func cleanupCodexRecoveryUser(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id=$1", userID); err != nil {
			t.Errorf("delete recovery fixture user %s: %v", userID, err)
		}
	})
}

// Lock only after the exchange. The first reply is discarded; each retry uses a
// fresh service, including while unlocked but before any survivor promotion.
func TestCodexRefreshRecoveryLostReplyLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("recovered")}}
	f := newRefreshFixture(t, e, fake)
	cleanupCodexRecoveryUser(t, e.pool, f.userID)
	cap := e.mintCap(t, f.runID, f.workerID)
	v := vault.NewPostgres(e.box, e.pool)
	enrollCodexProtectedVault(t, e, v, f.userID, "recovery-fixture-password")
	f.svc.SetVault(v)
	fake.onRefresh = func() { v.Lock(f.userID) }
	op := uuid.New()
	_, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
	if !errors.Is(err, ErrCodexVaultLocked) {
		t.Fatalf("first response: %v", err)
	}
	acct := e.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != codexCoordQuarantined || acct.RecoveryCause.String != "vault_locked" || !acct.RecoveryCause.Valid || len(acct.RecoverySealed) == 0 || acct.RecoverySealedWith.String != store.SealedWithMasterAAD {
		t.Fatalf("durable vault recovery absent: state=%s cause=%v", acct.CoordState, acct.RecoveryCause)
	}
	for _, unlocked := range []bool{false, true} {
		if unlocked {
			if err := v.Unlock(e.ctx, f.userID, "recovery-fixture-password"); err != nil {
				t.Fatal(err)
			}
		}
		for _, state := range []string{codexIntentRotating, codexIntentReconciled} {
			e.exec("UPDATE codex_refresh_intent SET state=$1 WHERE operation_id=$2", state, op)
			fresh := &Service{q: e.q, box: e.box, codexRefresh: fake}
			fresh.SetVault(v)
			fresh.SetTxBeginner(e.pool)
			res, err := fresh.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
			if !errors.Is(err, ErrCodexVaultLocked) || res != (CodexRefreshResult{}) {
				t.Fatalf("unlocked=%v state=%s retry=(%+v,%v)", unlocked, state, res, err)
			}
			if _, err := fresh.AuthorizeCodexCredentialOp(e.ctx, f.wkr, f.runID, cap, ScopeStartRefresh); !errors.Is(err, ErrCodexAccountQuarantined) {
				t.Fatalf("public auth: %v", err)
			}
		}
	}
	if fake.calls != 1 || fake.discoverCalls != 0 {
		t.Fatalf("provider calls=%d discover=%d", fake.calls, fake.discoverCalls)
	}
}

func TestCodexRefreshRecoveryRefusalsLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(codexTestEnv, refreshFixture, uuid.UUID)
		want   error
	}{
		{"null cause", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE codex_provider_account SET recovery_cause=NULL WHERE id=$1", f.accountID)
		}, ErrCodexAccountQuarantined},
		{"slot generation", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE codex_provider_account SET recovery_generation=1 WHERE id=$1", f.accountID)
		}, ErrCodexAccountQuarantined},
		{"missing slot", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE codex_provider_account SET recovery_cause=NULL, recovery_sealed=NULL, recovery_generation=NULL, recovery_sealed_with=NULL WHERE id=$1", f.accountID)
		}, ErrCodexAccountQuarantined},
		{"reauth", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE codex_provider_account SET reauth_required=true, reauth_generation=generation, reauth_credential_revision=credential_revision WHERE id=$1", f.accountID)
		}, ErrCodexAccountQuarantined},
		{"intent account", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			other, err := e.q.InsertCodexProviderAccount(e.ctx, store.InsertCodexProviderAccountParams{UserID: f.userID, ProviderUserID: uuid.NewString(), WorkspaceAccountID: uuid.NewString(), SealedLogin: []byte("x"), SealedWith: store.SealedWithMaster})
			if err != nil {
				panic(err)
			}
			e.exec("UPDATE codex_refresh_intent SET provider_account_id=$1 WHERE operation_id=$2", other.ID, op)
		}, ErrCodexAccountQuarantined},
		{"revoked", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE codex_provider_account SET credential_revision=credential_revision+1 WHERE id=$1", f.accountID)
		}, ErrCodexAccountRevisionStale},
		{"replaced", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1", f.aliasID)
		}, ErrCodexMaterialRevisionStale},
		{"released active claim", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", f.runID)
		}, ErrCodexRunNotActivelyClaimed},
		{"superseded", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE runs SET codex_claim_epoch=codex_claim_epoch+1 WHERE id=$1", f.runID)
		}, ErrCodexCapabilityEpoch},
		{"foreign worker", func(e codexTestEnv, f refreshFixture, op uuid.UUID) {
			e.exec("UPDATE runs SET worker_id=NULL WHERE id=$1", f.runID)
		}, ErrCodexWorkerMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("new")}}
			f := newRefreshFixture(t, e, fake)
			cleanupCodexRecoveryUser(t, e.pool, f.userID)
			cap := e.mintCap(t, f.runID, f.workerID)
			enrollCodexProtectedVault(t, e, f.svc.vlt, f.userID, codexVaultLockedTestPassword)
			fake.onRefresh = func() { f.svc.vlt.Lock(f.userID) }
			op := uuid.New()
			if _, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0); !errors.Is(err, ErrCodexVaultLocked) {
				t.Fatal(err)
			}
			tc.mutate(e, f, op)
			res, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
			if !errors.Is(err, tc.want) || errors.Is(err, ErrCodexVaultLocked) || res.AccessToken != "" {
				t.Fatalf("retry=(%+v,%v), want %v", res, err, tc.want)
			}
			if fake.calls != 1 {
				t.Fatalf("provider calls=%d", fake.calls)
			}
		})
	}
}

func TestCodexRefreshRecoveryPendingCompatibilityLiveDB(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(map[bool]string{false: "old worker", true: "new worker"}[capable], func(t *testing.T) {
			e := setupCodexLiveDB(t)
			fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("new")}}
			f := newRefreshFixture(t, e, fake)
			cleanupCodexRecoveryUser(t, e.pool, f.userID)
			cap := e.mintCap(t, f.runID, f.workerID)
			enrollCodexProtectedVault(t, e, f.svc.vlt, f.userID, codexVaultLockedTestPassword)
			fake.onRefresh = func() { f.svc.vlt.Lock(f.userID) }
			if capable {
				f.wkr.ProtocolCapabilities = []string{capability.CodexRefreshRecoveryV1}
			}
			hook := &recoverySlotHookStore{Queries: e.q, failFirst: codexRecoverySlotSyncAttempts}
			f.svc.q = hook
			var pending func()
			f.svc.background = func(fn func()) { pending = fn }
			op := uuid.New()
			res, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
			if capable {
				if !errors.Is(err, ErrCodexRefreshContended) || errors.Is(err, ErrCodexVaultLocked) {
					t.Fatalf("new worker=(%+v,%v)", res, err)
				}
			} else if !errors.Is(err, ErrCodexVaultLocked) {
				t.Fatalf("old worker=%v", err)
			}
			if pending == nil {
				t.Fatal("missing background handoff")
			}
			if acct := e.mustAccount(t, f.userID, f.accountID); acct.RecoveryCause.Valid {
				t.Fatal("pending is not durable")
			}
			// No durable evidence yet. A rotating retry must remain contended.
			if _, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0); !errors.Is(err, ErrCodexRefreshContended) {
				t.Fatalf("pending retry=%v", err)
			}
			// Reap the expired lease before the delayed writer lands.
			e.exec("UPDATE codex_provider_account SET lease_deadline=now()-interval '1 second' WHERE id=$1", f.accountID)
			if n, err := e.q.QuarantineExpiredCodexLease(e.ctx, store.QuarantineExpiredCodexLeaseParams{ID: f.accountID, UserID: f.userID, Now: pgconv.Time(time.Now())}); err != nil || n != 1 {
				t.Fatalf("reap=(%d,%v)", n, err)
			}
			res, err = f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
			if !errors.Is(err, ErrCodexAccountQuarantined) || errors.Is(err, ErrCodexVaultLocked) || res != (CodexRefreshResult{}) {
				t.Fatalf("reaped pending retry=(%+v,%v)", res, err)
			}
			pending()
			res, err = f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
			if !errors.Is(err, ErrCodexVaultLocked) || res != (CodexRefreshResult{}) {
				t.Fatalf("durable retry=(%+v,%v)", res, err)
			}
			if fake.calls != 1 {
				t.Fatalf("provider calls=%d", fake.calls)
			}
		})
	}
}

func TestCodexRefreshRecoveryReleasedDuringExchangeLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("new")}}
	f := newRefreshFixture(t, e, fake)
	cleanupCodexRecoveryUser(t, e.pool, f.userID)
	cap := e.mintCap(t, f.runID, f.workerID)
	enrollCodexProtectedVault(t, e, f.svc.vlt, f.userID, codexVaultLockedTestPassword)
	fake.onRefresh = func() {
		f.svc.vlt.Lock(f.userID)
		e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", f.runID)
	}
	op := uuid.New()
	for i := 0; i < 2; i++ {
		res, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
		if !errors.Is(err, ErrCodexRunNotActivelyClaimed) || errors.Is(err, ErrCodexVaultLocked) || res.AccessToken != "" {
			t.Fatalf("attempt %d=(%+v,%v)", i, res, err)
		}
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls=%d", fake.calls)
	}
}

func TestCodexRefreshRecoveryNonVaultBranchesLiveDB(t *testing.T) {
	for _, branch := range []string{"provider rejection", "identity mismatch", "unverified material", "commit failure", "missing recovery sealer"} {
		t.Run(branch, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("non-vault")}}
			f := newRefreshFixture(t, e, fake)
			cleanupCodexRecoveryUser(t, e.pool, f.userID)
			cap := e.mintCap(t, f.runID, f.workerID)
			f.svc.txBeginner = e.pool
			wantSlot := false
			switch branch {
			case "provider rejection":
				fake.err = rejectedRefreshErr()
			case "identity mismatch":
				fake.freshClaimsSet = true
				fake.freshClaims = freshClaimsForIdentity(codexauth.Identity{ProviderUserID: uuid.NewString(), WorkspaceAccountID: f.workspaceAcctID})
			case "unverified material":
				fake.freshClaimsSet = true
				wantSlot = true
			case "commit failure":
				f.svc.q = refreshHookStore{Queries: e.q, commitErr: errors.New("injected commit failure")}
				wantSlot = true
			case "missing recovery sealer":
				enrollCodexProtectedVault(t, e, f.svc.vlt, f.userID, codexVaultLockedTestPassword)
				// Open while unlocked, then remove the recovery sealer and lock.
				fake.onRefresh = func() {
					f.svc.vlt.Lock(f.userID)
					f.svc.SetVault(vault.NewPostgres(nil, e.pool))
				}
			}
			op := uuid.New()
			res, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
			if err == nil || errors.Is(err, ErrCodexVaultLocked) || res.AccessToken != "" {
				t.Fatalf("first response=(%+v,%v)", res, err)
			}
			acct := e.mustAccount(t, f.userID, f.accountID)
			if acct.RecoveryCause.Valid || (len(acct.RecoverySealed) > 0) != wantSlot || acct.CoordState != codexCoordQuarantined {
				t.Fatalf("branch state: cause=%v slot=%v state=%s", acct.RecoveryCause, len(acct.RecoverySealed) > 0, acct.CoordState)
			}
			if !wantSlot && mustIntent(t, e, op, f.userID).State != codexIntentUnrecoverable {
				t.Fatal("permanent failure intent was not unrecoverable")
			}
			res, err = f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, op, 0)
			if !errors.Is(err, ErrCodexAccountQuarantined) || errors.Is(err, ErrCodexVaultLocked) || res != (CodexRefreshResult{}) {
				t.Fatalf("same-op NULL-cause retry=(%+v,%v)", res, err)
			}
			if fake.calls != 1 || fake.discoverCalls != 0 {
				t.Fatalf("provider calls=%d discover=%d", fake.calls, fake.discoverCalls)
			}
		})
	}
}

// Promote after the fenced recovery transaction commits, before response selection.
// The callback fires once for this fixture; a failed commit never runs it.
type recoveryAfterCommitBeginner struct {
	*pgxpool.Pool
	afterCommit func()
	fired       bool
}

func (b *recoveryAfterCommitBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.BeginTx(ctx, pgx.TxOptions{})
}

func (b *recoveryAfterCommitBeginner) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := b.Pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &recoveryAfterCommitTx{Tx: tx, parent: b}, nil
}

type recoveryAfterCommitTx struct {
	pgx.Tx
	parent *recoveryAfterCommitBeginner
}

func (tx *recoveryAfterCommitTx) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	if !tx.parent.fired {
		tx.parent.fired = true
		tx.parent.afterCommit()
	}
	return nil
}

func TestCodexRefreshRecoveryPromotedBeforeFirstResponseLiveDB(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(map[bool]string{false: "old worker", true: "new worker"}[capable], func(t *testing.T) {
			cases := []struct {
				name   string
				sql    string
				target string
				want   error
			}{
				{"authorized", "", "", ErrCodexVaultLocked},
				{"released claim", "UPDATE runs SET claim_released_at=now() WHERE id=$1", "run", ErrCodexRunNotActivelyClaimed},
				{"capability epoch", "UPDATE runs SET codex_claim_epoch=codex_claim_epoch+1 WHERE id=$1", "run", ErrCodexCapabilityEpoch},
				{"worker ownership", "UPDATE runs SET worker_id=NULL WHERE id=$1", "run", ErrCodexWorkerMismatch},
				{"material revision", "UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1", "alias", ErrCodexMaterialRevisionStale},
				{"credential revision", "UPDATE codex_provider_account SET credential_revision=credential_revision+1 WHERE id=$1", "account", ErrCodexAccountRevisionStale},
				{"identity tuple", "UPDATE codex_provider_account SET provider_user_id='changed' WHERE id=$1", "account", ErrCodexAccountTupleMismatch},
				{"frozen binding", "UPDATE runs SET codex_account_key=NULL WHERE id=$1", "run", ErrCodexAccountKeyUnfrozen},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					e := setupCodexLiveDB(t)
					fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("promoted")}}
					f := newRefreshFixture(t, e, fake)
					cleanupCodexRecoveryUser(t, e.pool, f.userID)
					cap := e.mintCap(t, f.runID, f.workerID)
					if capable {
						f.wkr.ProtocolCapabilities = []string{capability.CodexRefreshRecoveryV1}
					}
					v := vault.NewPostgres(e.box, e.pool)
					f.svc.SetVault(v)
					enrollCodexProtectedVault(t, e, v, f.userID, "first-response-password")
					fake.onRefresh = func() { v.Lock(f.userID) }
					before := e.mustAccount(t, f.userID, f.accountID)
					f.svc.SetTxBeginner(&recoveryAfterCommitBeginner{Pool: e.pool, afterCommit: func() {
						acct := e.mustAccount(t, f.userID, f.accountID)
						if !acct.RecoveryCause.Valid || acct.RecoveryCause.String != "vault_locked" {
							t.Fatal("missing fenced vault-lock recovery")
						}
						if err := v.Unlock(e.ctx, f.userID, "first-response-password"); err != nil {
							t.Fatal(err)
						}
						if changed, err := f.svc.promoteCodexRecovery(e.ctx, e.q, f.userID, e.mustAccountSnapshot(t, f.userID, f.accountID)); err != nil || !changed {
							t.Fatalf("promotion=(%v,%v)", changed, err)
						}
						acct = e.mustAccount(t, f.userID, f.accountID)
						if acct.CoordState != "idle" || acct.Generation != 1 || acct.CredentialRevision != before.CredentialRevision || acct.RecoveryCause.Valid || len(acct.RecoverySealed) != 0 {
							t.Fatal("promotion did not clear evidence with credential revision unchanged")
						}
						if tc.sql != "" {
							target := map[string]uuid.UUID{"run": f.runID, "alias": f.aliasID, "account": f.accountID}[tc.target]
							e.exec(tc.sql, target)
						}
					}})
					res, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, cap, uuid.New(), 0)
					if !errors.Is(err, tc.want) || errors.Is(err, ErrCodexVaultLocked) != (tc.want == ErrCodexVaultLocked) {
						t.Fatalf("first response=(%+v,%v), want %v", res, err, tc.want)
					}
					if res != (CodexRefreshResult{Outcome: CodexRefreshQuarantined}) {
						t.Fatalf("first response exposed credential coordinates: %+v", res)
					}
					if fake.calls != 1 || fake.discoverCalls != 1 {
						t.Fatalf("provider calls=%d discover=%d", fake.calls, fake.discoverCalls)
					}
				})
			}
		})
	}
}

func TestCodexRefreshRecoveryRegistrationDowngradeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	cleanupCodexRecoveryUser(t, e.pool, e.userID)
	svc := e.permitService(t)
	id := e.seedWorker(t, nil)
	w := store.Worker{ID: id, UserID: e.userID}
	for _, caps := range [][]string{{capability.CodexRefreshRecoveryV1}, nil} {
		_, _, err := svc.Register(context.Background(), w, "v-test", "base", nil, nil, caps, nil)
		if err != nil {
			t.Fatal(err)
		}
		stored := e.workerProtocolCaps(t, id)
		if slices.Contains(stored, capability.CodexRefreshRecoveryV1) != (len(caps) > 0) {
			t.Fatalf("stored=%v advertised=%v", stored, caps)
		}
	}
}
