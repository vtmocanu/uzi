package workersvc

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

const credVaultPassword = "codex-vault-fixture-password"

// Begin is the deterministic barrier after outside preparation and before any
// install transaction locks. The lifecycle transition completes before Begin
// delegates, so it cannot be hidden by scheduling or repeated discovery.
type credInstallBarrier struct {
	next   CodexTxBeginner
	before func()
	calls  int
}

func (b *credInstallBarrier) Begin(ctx context.Context) (pgx.Tx, error) {
	b.calls++
	b.before()
	return b.next.Begin(ctx)
}

func credVaultEnrollment(t *testing.T, env codexTestEnv, owner uuid.UUID) {
	t.Helper()
	hash, err := auth.HashPassword(credVaultPassword)
	if err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE users SET password_hash=$2 WHERE id=$1", owner, hash)
	// Disable needs optional policy. Restore the exact setting for sibling tests.
	var policy string
	if err := env.pool.QueryRow(env.ctx, "SELECT value FROM app_settings WHERE key='vault_policy'").Scan(&policy); err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE app_settings SET value='optional' WHERE key='vault_policy'")
	t.Cleanup(func() { env.exec("UPDATE app_settings SET value=$1 WHERE key='vault_policy'", policy) })
}

func TestReconcileCodexInstallPolicyBarrierLiveDB(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, transition := range []string{"enable", "disable", "disable-reenable", "disable-reenable-locked"} {
			name := transition
			if restore {
				name += "-restore"
			} else {
				name += "-insert"
			}
			t.Run(name, func(t *testing.T) {
				env := setupCodexLiveDB(t)
				ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
				defer cancel()
				env.ctx = ctx
				owner := env.seedUser(t)
				credVaultEnrollment(t, env, owner)
				identity := codexauth.Identity{ProviderUserID: uuid.NewString(), WorkspaceAccountID: uuid.NewString()}
				fake := newFakeCodexIdentity()
				var accountID uuid.UUID
				if restore {
					oldToken := codexToken("old-access")
					oldAlias := env.seedStagingAlias(t, owner, "old", codexLoginBlob{AccessToken: oldToken})
					fake.idByToken[oldToken] = identity
					if err := NewCodexReconciler(env.q, env.vlt, env.box, fake, env.pool).ReconcileCodexAuthIdentity(ctx, owner, oldAlias); err != nil {
						t.Fatal(err)
					}
					accountID = uuid.UUID(mustState(t, env, owner, oldAlias).ProviderAccountID.Bytes)
					env.exec("UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1", accountID)
				}
				token := codexToken("import-access")
				refresh := codexToken("refresh")
				alias := env.seedStagingAlias(t, owner, "fresh", codexLoginBlob{AccessToken: token, RefreshToken: refresh})
				fake.idByToken[token] = identity
				if transition != "enable" {
					if err := env.vlt.Enable(ctx, owner, credVaultPassword); err != nil {
						t.Fatal(err)
					}
				}
				initial := mustState(t, env, owner, alias)
				before, err := env.vlt.State(ctx, owner)
				if err != nil {
					t.Fatal(err)
				}
				discoveries := fake.discoverCalls
				barrier := &credInstallBarrier{next: env.pool, before: func() {
					if transition != "enable" {
						if err := env.vlt.Disable(ctx, owner); err != nil {
							t.Fatal(err)
						}
					}
					if transition != "disable" {
						if err := env.vlt.Enable(ctx, owner, credVaultPassword); err != nil {
							t.Fatal(err)
						}
					}
					if transition == "disable-reenable-locked" {
						env.vlt.Lock(owner)
					}
				}}
				err = NewCodexReconciler(env.q, env.vlt, env.box, fake, barrier).ReconcileCodexAuthIdentity(ctx, owner, alias)
				if barrier.calls != 1 || fake.discoverCalls != discoveries+1 || fake.refreshCalls != 0 {
					t.Fatalf("begin=%d discovery delta=%d refresh=%d", barrier.calls, fake.discoverCalls-discoveries, fake.refreshCalls)
				}
				current, stateErr := env.vlt.State(ctx, owner)
				if stateErr != nil {
					t.Fatal(stateErr)
				}
				if current.VaultEpoch <= before.VaultEpoch {
					t.Fatal("barrier did not advance epoch")
				}
				after := mustState(t, env, owner, alias)
				if after.MaterialRevision != initial.MaterialRevision {
					t.Fatal("crypto transition changed alias material revision")
				}
				if transition == "disable-reenable-locked" {
					if !errors.Is(err, vault.ErrLocked) {
						t.Fatalf("want locked refusal, got %v", err)
					}
					if after.Status != initial.Status || after.ProviderAccountID.Valid {
						t.Fatal("locked refusal linked alias")
					}
					if restore {
						acct := env.mustAccount(t, owner, accountID)
						if acct.Generation != 0 || acct.CoordState != "quarantined" {
							t.Fatal("locked refusal restored canonical")
						}
						if err := env.vlt.Unlock(ctx, owner, credVaultPassword); err != nil {
							t.Fatal(err)
						}
						if env.credAccountBlob(t, owner, accountID).AccessToken == token {
							t.Fatal("locked refusal installed imported material")
						}
					} else if env.countProviderAccounts(t, owner) != 0 {
						t.Fatal("locked refusal inserted canonical")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if after.Status != codexStatusLinked || !after.ProviderAccountID.Valid {
					t.Fatal("alias not linked")
				}
				if restore && uuid.UUID(after.ProviderAccountID.Bytes) != accountID {
					t.Fatal("restore changed canonical identity")
				}
				accountID = uuid.UUID(after.ProviderAccountID.Bytes)
				acct := env.mustAccount(t, owner, accountID)
				wantGeneration := int64(0)
				if restore {
					wantGeneration = 1
				}
				if acct.Generation != wantGeneration || acct.ProviderUserID != identity.ProviderUserID || acct.WorkspaceAccountID != identity.WorkspaceAccountID {
					t.Fatal("identity or generation changed unexpectedly")
				}
				wantMarker := store.SealedWithDEK
				if current.VaultMode == store.VaultModeUnprotected {
					wantMarker = store.SealedWithMasterAAD
				}
				if acct.SealedWith != wantMarker {
					t.Fatalf("marker=%q want=%q", acct.SealedWith, wantMarker)
				}
				blob := env.credAccountBlob(t, owner, accountID)
				if blob.AccessToken != token || blob.RefreshToken != refresh {
					t.Fatal("current key cannot read same discovered login")
				}
			})
		}
	}
}

func TestReconcileCodexHealthyLockedPolicyLinkLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	owner := env.seedUser(t)
	credVaultEnrollment(t, env, owner)
	fake := newFakeCodexIdentity()
	identity := codexauth.Identity{ProviderUserID: uuid.NewString(), WorkspaceAccountID: uuid.NewString()}
	token := codexToken("canonical")
	alias := env.seedStagingAlias(t, owner, "canonical", codexLoginBlob{AccessToken: token})
	fake.idByToken[token] = identity
	r := NewCodexReconciler(env.q, env.vlt, env.box, fake, env.pool)
	if err := r.ReconcileCodexAuthIdentity(env.ctx, owner, alias); err != nil {
		t.Fatal(err)
	}
	accountID := uuid.UUID(mustState(t, env, owner, alias).ProviderAccountID.Bytes)
	if err := env.vlt.Enable(env.ctx, owner, credVaultPassword); err != nil {
		t.Fatal(err)
	}
	before := env.mustAccount(t, owner, accountID)
	env.vlt.Lock(owner)
	// Legacy master input remains intentionally readable while the DEK is locked.
	fresh := codexToken("duplicate")
	duplicate := env.seedStagingAlias(t, owner, "duplicate", codexLoginBlob{AccessToken: fresh})
	fake.idByToken[fresh] = identity
	initial := mustState(t, env, owner, duplicate)
	if err := r.ReconcileCodexAuthIdentity(env.ctx, owner, duplicate); err != nil {
		t.Fatal(err)
	}
	after := env.mustAccount(t, owner, accountID)
	linked := mustState(t, env, owner, duplicate)
	if linked.Status != codexStatusLinked || uuid.UUID(linked.ProviderAccountID.Bytes) != accountID || linked.MaterialRevision != initial.MaterialRevision {
		t.Fatal("healthy link changed identity or revision")
	}
	if !bytes.Equal(before.SealedLogin, after.SealedLogin) || before.SealedWith != after.SealedWith || before.Generation != after.Generation {
		t.Fatal("healthy link overwrote canonical material")
	}
	if fake.discoverCalls != 2 || fake.refreshCalls != 0 {
		t.Fatal("healthy link repeated provider work")
	}
}

// A failed outside preparation must be retried using the now-current locked
// transaction state, rather than rejecting material that became writable.
func TestReconcileCodexLockedPreparationResealsAfterDisableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
	defer cancel()
	env.ctx = ctx
	owner := env.seedUser(t)
	credVaultEnrollment(t, env, owner)
	if err := env.vlt.Enable(ctx, owner, credVaultPassword); err != nil {
		t.Fatal(err)
	}
	env.vlt.Lock(owner)
	token := codexToken("locked-preparation")
	alias := env.seedStagingAlias(t, owner, "legacy-import", codexLoginBlob{AccessToken: token})
	initial := mustState(t, env, owner, alias)
	fake := newFakeCodexIdentity()
	fake.idByToken[token] = codexauth.Identity{ProviderUserID: uuid.NewString(), WorkspaceAccountID: uuid.NewString()}
	barrier := &credInstallBarrier{next: env.pool, before: func() {
		if err := env.vlt.Unlock(ctx, owner, credVaultPassword); err != nil {
			t.Fatal(err)
		}
		if err := env.vlt.Disable(ctx, owner); err != nil {
			t.Fatal(err)
		}
	}}
	if err := NewCodexReconciler(env.q, env.vlt, env.box, fake, barrier).ReconcileCodexAuthIdentity(ctx, owner, alias); err != nil {
		t.Fatal(err)
	}
	state := mustState(t, env, owner, alias)
	if state.Status != codexStatusLinked || !state.ProviderAccountID.Valid || state.MaterialRevision != initial.MaterialRevision {
		t.Fatal("current off install did not preserve alias revision")
	}
	accountID := uuid.UUID(state.ProviderAccountID.Bytes)
	acct := env.mustAccount(t, owner, accountID)
	if acct.SealedWith != store.SealedWithMasterAAD || acct.Generation != 0 {
		t.Fatal("current off install has wrong marker or generation")
	}
	if env.credAccountBlob(t, owner, accountID).AccessToken != token {
		t.Fatal("off reseal changed imported material")
	}
	if barrier.calls != 1 || fake.discoverCalls != 1 || fake.refreshCalls != 0 {
		t.Fatal("crypto retry repeated provider work")
	}
}

func TestReconcileCodexMissingPolicyVaultRefusesWriteLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	owner := env.seedUser(t)
	token := codexToken("no-policy")
	alias := env.seedStagingAlias(t, owner, "no-policy", codexLoginBlob{AccessToken: token})
	fake := newFakeCodexIdentity()
	fake.idByToken[token] = codexauth.Identity{ProviderUserID: uuid.NewString(), WorkspaceAccountID: uuid.NewString()}
	err := NewCodexReconciler(env.q, nil, env.box, fake, env.pool).ReconcileCodexAuthIdentity(env.ctx, owner, alias)
	if !errors.Is(err, vault.ErrNotConfigured) {
		t.Fatalf("want missing policy refusal, got %v", err)
	}
	if env.countProviderAccounts(t, owner) != 0 {
		t.Fatal("nil vault wrote canonical material")
	}
	state := mustState(t, env, owner, alias)
	if state.Status != codexStatusStaging || state.ProviderAccountID.Valid {
		t.Fatal("nil vault linked alias")
	}
}
