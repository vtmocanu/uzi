package workersvc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Issue #1766: a codex credential route that meets a LOCKED owner vault after authorization
// answers with the typed, transient ErrCodexVaultLocked (the handler maps it to 409
// vault_locked) instead of an untyped error that fell to the generic 500. These LiveDB tests
// drive the real vault against a real Postgres. Skipped unless UZI_TEST_DATABASE_URL is set;
// run them through ./e2e/run-store-it.sh.

const codexVaultLockedTestPassword = "codex-vault-locked-test-password" //nolint:gosec // G101: synthetic vault password for a throwaway test user, never a real secret

// dekSealAccountAndLock wires a real vault into f.svc, creates and unlocks the user's vault,
// re-seals the account's committed login under the user DEK (sealed_with='dek'), and locks
// the vault again. The fixture's master-sealed login would open without an unlock, so this is
// what makes the pre-exchange open of the login hit the locked vault.
func dekSealAccountAndLock(t *testing.T, env codexTestEnv, f refreshFixture) *vault.Vault {
	t.Helper()
	vlt := vault.New(env.box, env.q)
	f.svc.SetVault(vlt)
	if err := vlt.Unlock(env.ctx, f.userID, codexVaultLockedTestPassword); err != nil {
		t.Fatalf("create+unlock vault: %v", err)
	}
	raw, err := json.Marshal(codexLoginBlob{AccessToken: f.accessToken, RefreshToken: f.prevRefreshToken}) //nolint:gosec // G117: synthetic fixture login, DEK-sealed below
	if err != nil {
		t.Fatalf("encode login: %v", err)
	}
	sealed, err := vlt.Seal(f.userID, store.KindCodexAuth, raw)
	if err != nil {
		t.Fatalf("seal login under DEK: %v", err)
	}
	env.exec(`UPDATE codex_provider_account SET sealed_login = $1, sealed_with = $2 WHERE id = $3`,
		sealed, store.SealedWithDEK, f.accountID)
	vlt.Lock(f.userID)
	if vlt.Unlocked(f.userID) {
		t.Fatal("vault still unlocked after Lock")
	}
	return vlt
}

// assertNoIntent fails when any codex refresh intent exists for op.
func assertNoIntent(t *testing.T, env codexTestEnv, op, userID uuid.UUID) {
	t.Helper()
	_, err := env.q.GetCodexRefreshIntent(env.ctx, store.GetCodexRefreshIntentParams{OperationID: op, UserID: userID})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("intent for op %s: err = %v, want none recorded", op, err)
	}
}

// TestCoordinatedCodexRefreshVaultLockedBeforeExchangeLiveDB (case a): a locked vault over a
// DEK-sealed login fails the refresh with ErrCodexVaultLocked BEFORE any durable effect: zero
// provider calls, no intent row, the account idle at its generation.
//
// FAILS OLD: the refresh returned the private errVaultLocked, not ErrCodexVaultLocked.
func TestCoordinatedCodexRefreshVaultLockedBeforeExchangeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	dekSealAccountAndLock(t, env, f)
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexVaultLocked) {
		t.Fatalf("err = %v, want ErrCodexVaultLocked", err)
	}
	if res.AccessToken != "" {
		t.Fatalf("result = %+v, want no token", res)
	}
	if fake.calls != 0 {
		t.Fatalf("provider calls = %d, want 0 (a locked vault must stop before the exchange)", fake.calls)
	}
	assertNoIntent(t, env, op, f.userID)
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != "idle" || acct.Generation != 0 {
		t.Fatalf("account = (state=%q gen=%d), want (idle,0)", acct.CoordState, acct.Generation)
	}
}

// TestReleaseCodexCredentialVaultLockedLiveDB (case a, release): both auth modes answer a
// locked vault with ErrCodexVaultLocked and no token.
//
// FAILS OLD: release returned the private errVaultLocked, not ErrCodexVaultLocked.
func TestReleaseCodexCredentialVaultLockedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	t.Run("subscription", func(t *testing.T) {
		fake := &fakeRefreshClient{}
		f := newRefreshFixture(t, env, fake)
		dekSealAccountAndLock(t, env, f)
		capw := env.mintCap(t, f.runID, f.workerID)

		released, err := f.svc.ReleaseCodexCredential(env.ctx, f.wkr, f.runID, capw)
		if !errors.Is(err, ErrCodexVaultLocked) {
			t.Fatalf("err = %v, want ErrCodexVaultLocked", err)
		}
		if released != (CodexReleaseResult{}) {
			t.Fatalf("release = %+v, want empty while the vault is locked", released)
		}
		if fake.calls != 0 {
			t.Fatalf("provider calls = %d, want 0", fake.calls)
		}
	})

	t.Run("api_key", func(t *testing.T) {
		userID, workerID, repoID := env.seedCodexInfra(t)
		key := codexToken("sk")
		aliasID := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), key)
		runID := env.seedCodexRun(t, userID, workerID, repoID)
		svc := &Service{q: env.q, box: env.box}
		if err := svc.FreezeCodexBinding(env.ctx, userID, runID, aliasID, codexAuthModeAPIKey); err != nil {
			t.Fatalf("FreezeCodexBinding: %v", err)
		}
		// The first unlock creates the vault and rewraps the master-sealed key under the DEK;
		// locking it again leaves the key unopenable until the next unlock.
		vlt := vault.New(env.box, env.q)
		svc.SetVault(vlt)
		if err := vlt.Unlock(env.ctx, userID, codexVaultLockedTestPassword); err != nil {
			t.Fatalf("create+unlock vault: %v", err)
		}
		vlt.Lock(userID)
		wkr := store.Worker{ID: workerID, UserID: userID}
		capw := env.mintCap(t, runID, workerID)

		released, err := svc.ReleaseCodexCredential(env.ctx, wkr, runID, capw)
		if !errors.Is(err, ErrCodexVaultLocked) {
			t.Fatalf("err = %v, want ErrCodexVaultLocked", err)
		}
		if released != (CodexReleaseResult{}) {
			t.Fatalf("release = %+v, want empty while the vault is locked", released)
		}

		// After unlock the same release returns the key: the lock was transient.
		if err := vlt.Unlock(env.ctx, userID, codexVaultLockedTestPassword); err != nil {
			t.Fatalf("unlock vault: %v", err)
		}
		released, err = svc.ReleaseCodexCredential(env.ctx, wkr, runID, capw)
		if err != nil || released.AccessToken != key {
			t.Fatalf("post-unlock release = (%q, %v), want the static key", released.AccessToken, err)
		}
	})
}

// TestCoordinatedCodexRefreshVaultLockedReplayLiveDB (case c): op X commits while the vault
// is unlocked; after a lock the same op X answers ErrCodexVaultLocked with no new exchange,
// and after unlock it replays the committed token, still with exactly one provider call.
//
// FAILS OLD: the locked replay returned the private errVaultLocked, not ErrCodexVaultLocked.
func TestCoordinatedCodexRefreshVaultLockedReplayLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	newAccess := codexToken("access-new")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
	f := newRefreshFixture(t, env, fake)
	vlt := dekSealAccountAndLock(t, env, f)
	if err := vlt.Unlock(env.ctx, f.userID, codexVaultLockedTestPassword); err != nil {
		t.Fatalf("unlock vault: %v", err)
	}
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	first, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if err != nil || first.Outcome != CodexRefreshAdvanced || first.Generation != 1 {
		t.Fatalf("unlocked advance = (%+v, %v), want advanced to generation 1", first, err)
	}

	vlt.Lock(f.userID)
	locked, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexVaultLocked) {
		t.Fatalf("locked replay err = %v, want ErrCodexVaultLocked", err)
	}
	if locked.AccessToken != "" {
		t.Fatalf("locked replay = %+v, want no token", locked)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (a locked replay must not exchange)", fake.calls)
	}

	if err := vlt.Unlock(env.ctx, f.userID, codexVaultLockedTestPassword); err != nil {
		t.Fatalf("re-unlock vault: %v", err)
	}
	replay, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if err != nil {
		t.Fatalf("post-unlock replay: %v", err)
	}
	if replay.Outcome != CodexRefreshReplayed || replay.AccessToken != newAccess || replay.Generation != 1 {
		t.Fatalf("post-unlock replay = %+v, want replayed (%q, 1)", replay, newAccess)
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls = %d, want still 1 after the replay", fake.calls)
	}
}

// TestCodexCredentialVaultLockedAuthorizationFirstLiveDB (case d): with the vault locked, an
// authorization failure still wins: a stale capability epoch is ErrCodexCapabilityEpoch
// (handler 403) and a foreign worker is ErrCodexWorkerMismatch (handler 404), never
// ErrCodexVaultLocked, so a locked vault is not an oracle to an unauthorized caller.
//
// This guards the ordering (authorization before any vault open), which already held before
// issue #1766; it is red on the old tree only because ErrCodexVaultLocked did not exist.
func TestCodexCredentialVaultLockedAuthorizationFirstLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("access-new")}}
	f := newRefreshFixture(t, env, fake)
	dekSealAccountAndLock(t, env, f)

	check := func(t *testing.T, name string, err error, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("%s: err = %v, want %v", name, err, want)
		}
		if errors.Is(err, ErrCodexVaultLocked) {
			t.Fatalf("%s: err = %v must not be ErrCodexVaultLocked before authorization passes", name, err)
		}
	}

	t.Run("stale capability epoch", func(t *testing.T) {
		stale := env.mintCap(t, f.runID, f.workerID)
		env.exec(`UPDATE runs SET codex_claim_epoch = codex_claim_epoch + 1, codex_cap_hash = NULL WHERE id = $1`, f.runID)
		_, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, stale, uuid.New(), 0)
		check(t, "refresh", err, ErrCodexCapabilityEpoch)
		_, err = f.svc.ReleaseCodexCredential(env.ctx, f.wkr, f.runID, stale)
		check(t, "release", err, ErrCodexCapabilityEpoch)
	})

	t.Run("foreign worker", func(t *testing.T) {
		capw := env.mintCap(t, f.runID, f.workerID)
		foreign := store.Worker{ID: uuid.New(), UserID: uuid.New()}
		_, err := f.svc.CoordinatedCodexRefresh(env.ctx, foreign, f.runID, capw, uuid.New(), 0)
		check(t, "refresh", err, ErrCodexWorkerMismatch)
		_, err = f.svc.ReleaseCodexCredential(env.ctx, foreign, f.runID, capw)
		check(t, "release", err, ErrCodexWorkerMismatch)
	})

	if fake.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", fake.calls)
	}
	if acct := env.mustAccount(t, f.userID, f.accountID); acct.CoordState != "idle" || acct.Generation != 0 {
		t.Fatalf("account = (state=%q gen=%d), want (idle,0)", acct.CoordState, acct.Generation)
	}
}

// TestCoordinatedCodexRefreshVaultLockedSealAuthorityLostLiveDB (case b, lost authority):
// authority is lost DURING the provider exchange and the post-exchange seal then meets a
// locked vault. The caller no longer holds authority, so the answer is the authorization
// error alone, never ErrCodexVaultLocked: a bumped capability epoch is ErrCodexCapabilityEpoch
// (handler 403) and an account-level revoke is ErrCodexAccountRevisionStale. The account
// quarantine the locked seal itself sets must not mask either. The durable state is exactly
// what a still-authorized caller leaves (retained recovery, intent rotating), so the recheck
// changes only the answer.
//
// FAILS OLD: the post-exchange recheck ran only on a token-bearing success, so the locked
// seal was answered ErrCodexVaultLocked (409 vault_locked) to a run that had lost authority.
func TestCoordinatedCodexRefreshVaultLockedSealAuthorityLostLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cases := []struct {
		name    string
		loseSQL string
		want    error
	}{
		{"capability epoch bumped", `UPDATE runs SET codex_claim_epoch = codex_claim_epoch + 1, codex_cap_hash = NULL WHERE id = $1`, ErrCodexCapabilityEpoch},
		{"account revoked", `UPDATE codex_provider_account SET credential_revision = credential_revision + 1 WHERE id = $1`, ErrCodexAccountRevisionStale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newAccess := codexToken("access-new")
			fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: newAccess}}
			f := newRefreshFixture(t, env, fake)
			capw := env.mintCap(t, f.runID, f.workerID)
			op := uuid.New()
			target := f.runID
			if tc.want == ErrCodexAccountRevisionStale {
				target = f.accountID
			}

			// A real vault that is locked for this user: the master-sealed login opens, but
			// the post-rotation canonical seal takes the DEK path and meets the lock.
			f.svc.SetVault(vault.New(env.box, env.q))
			fake.onRefresh = func() { env.exec(tc.loseSQL, target) }

			res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v from the post-exchange recheck", err, tc.want)
			}
			if errors.Is(err, ErrCodexVaultLocked) || errors.Is(err, ErrCodexRefreshQuarantined) {
				t.Fatalf("err = %v must carry only the authorization error once authority is lost", err)
			}
			if res.AccessToken != "" || res.Outcome != CodexRefreshQuarantined {
				t.Fatalf("result = %+v, want the quarantined outcome with NO token", res)
			}
			if fake.calls != 1 {
				t.Fatalf("provider calls = %d, want exactly 1", fake.calls)
			}
			acct := env.mustAccount(t, f.userID, f.accountID)
			if acct.Generation != 0 || acct.CoordState != codexCoordQuarantined {
				t.Fatalf("account = (state=%q gen=%d), want (quarantined,0)", acct.CoordState, acct.Generation)
			}
			if len(acct.RecoverySealed) == 0 || !acct.RecoverySealedWith.Valid || acct.RecoverySealedWith.String != store.SealedWithMaster {
				t.Fatalf("recovery not retained under master protection: sealed=%d with=%v", len(acct.RecoverySealed), acct.RecoverySealedWith)
			}
			if it := mustIntent(t, env, op, f.userID); it.State != codexIntentRotating {
				t.Fatalf("intent state = %q, want rotating (retained)", it.State)
			}
		})
	}
}

// TestReleaseCodexCredentialVaultLockedAuthorityLostLiveDB (case a, release, lost authority):
// the capability epoch is bumped between the initial authorization and the locked-vault open,
// so the release answers the recheck's ErrCodexCapabilityEpoch (handler 403), never
// ErrCodexVaultLocked.
//
// FAILS OLD: release rechecked authority only before returning a token, so a locked vault
// was answered ErrCodexVaultLocked to a run whose authority had already lapsed.
func TestReleaseCodexCredentialVaultLockedAuthorityLostLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{}
	f := newRefreshFixture(t, env, fake)
	dekSealAccountAndLock(t, env, f)
	capw := env.mintCap(t, f.runID, f.workerID)
	f.svc.q = releaseHookStore{
		Queries: env.q,
		onAccountRead: func() {
			env.exec(`UPDATE runs SET codex_claim_epoch = codex_claim_epoch + 1, codex_cap_hash = NULL WHERE id = $1`, f.runID)
		},
	}

	released, err := f.svc.ReleaseCodexCredential(env.ctx, f.wkr, f.runID, capw)
	if !errors.Is(err, ErrCodexCapabilityEpoch) {
		t.Fatalf("err = %v, want ErrCodexCapabilityEpoch from the recheck", err)
	}
	if errors.Is(err, ErrCodexVaultLocked) {
		t.Fatalf("err = %v must not be ErrCodexVaultLocked once authority is lost", err)
	}
	if released != (CodexReleaseResult{}) {
		t.Fatalf("release = %+v, want empty", released)
	}
	if fake.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", fake.calls)
	}
}

// TestCoordinatedCodexRefreshVaultLockedBeforeExchangeAuthorityLostLiveDB (case a, refresh,
// lost authority): the capability epoch is bumped between the initial authorization and the
// pre-exchange open of the DEK-sealed login, which then meets the locked vault. The refresh
// answers the recheck's ErrCodexCapabilityEpoch (handler 403), never ErrCodexVaultLocked, and
// nothing reached the provider or left a durable intent.
func TestCoordinatedCodexRefreshVaultLockedBeforeExchangeAuthorityLostLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("access-new")}}
	f := newRefreshFixture(t, env, fake)
	dekSealAccountAndLock(t, env, f)
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()
	f.svc.q = releaseHookStore{
		Queries: env.q,
		onAccountRead: func() {
			env.exec(`UPDATE runs SET codex_claim_epoch = codex_claim_epoch + 1, codex_cap_hash = NULL WHERE id = $1`, f.runID)
		},
	}

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexCapabilityEpoch) {
		t.Fatalf("err = %v, want ErrCodexCapabilityEpoch from the recheck", err)
	}
	if errors.Is(err, ErrCodexVaultLocked) {
		t.Fatalf("err = %v must not be ErrCodexVaultLocked once authority is lost", err)
	}
	if res.AccessToken != "" {
		t.Fatalf("result = %+v, want no token", res)
	}
	if fake.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", fake.calls)
	}
	assertNoIntent(t, env, op, f.userID)
}
