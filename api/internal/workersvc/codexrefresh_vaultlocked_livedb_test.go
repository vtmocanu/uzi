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

const codexVaultLockedTestPassword = "codex-vault-locked-test-password"

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
	raw, err := json.Marshal(codexLoginBlob{AccessToken: f.accessToken, RefreshToken: f.prevRefreshToken})
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
